package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
)

type TelegramConfig struct {
	Token           string
	BaseURL         string // Optional Bot API server root; default https://api.telegram.org.
	AllowedUserIDs  []int64
	AllowedGroupIDs []int64 // Groups disabled by default; all non-bot participants inside these groups are accepted.
	HTTPClient      *http.Client
	PollTimeout     time.Duration
	MaxFileBytes    int64
}
type Telegram struct {
	config   TelegramConfig
	client   *http.Client
	users    map[int64]bool
	groups   map[int64]bool
	username string // Set by ConfigureCommands before polling starts.
}
type Update struct {
	Offset   int64
	Incoming *domain.Incoming
}
type TelegramError struct {
	Code           int
	RetryAfter     time.Duration
	NotModified    bool
	MessageMissing bool
}

func (e *TelegramError) HTTPStatus() int { return e.Code }

// TelegramDeliveryError means the send may have reached Telegram, but an acknowledgement
// was not safely received. Do not automatically retry it: that could duplicate a message.
type TelegramDeliveryError struct {
	Cause   error
	Unknown bool
}

func (e *TelegramDeliveryError) Error() string         { return "Telegram delivery outcome is unknown" }
func (e *TelegramDeliveryError) Unwrap() error         { return e.Cause }
func (e *TelegramDeliveryError) DeliveryUnknown() bool { return e.Unknown }
func deliveryError(err error) error {
	if err == nil {
		return nil
	}
	var rejected *TelegramError
	if errors.As(err, &rejected) && rejected.Code < 500 {
		return err
	}
	return &TelegramDeliveryError{Cause: err, Unknown: true}
}

func (e *TelegramError) Error() string { return fmt.Sprintf("Telegram API error %d", e.Code) }
func NewTelegram(c TelegramConfig) (*Telegram, error) {
	if c.Token == "" || strings.ContainsAny(c.Token, "/?#\r\n") {
		return nil, errors.New("Telegram bot token is required and must not contain URL separators")
	}
	if len(c.AllowedUserIDs) == 0 {
		return nil, errors.New("at least one Telegram allowed user ID is required")
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://api.telegram.org"
	}
	base, err := providerURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	c.BaseURL = base
	if c.PollTimeout <= 0 {
		c.PollTimeout = 25 * time.Second
	}
	if c.PollTimeout > 50*time.Second {
		c.PollTimeout = 50 * time.Second
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 20 << 20
	}
	t := &Telegram{config: c, client: httpClient(c.HTTPClient, c.PollTimeout+15*time.Second), users: map[int64]bool{}, groups: map[int64]bool{}}
	for _, id := range c.AllowedUserIDs {
		if id <= 0 {
			return nil, errors.New("Telegram user IDs must be positive")
		}
		t.users[id] = true
	}
	for _, id := range c.AllowedGroupIDs {
		if id >= 0 {
			return nil, errors.New("Telegram group IDs must be negative")
		}
		t.groups[id] = true
	}
	return t, nil
}

// ConfigureCommands makes the keyboard commands discoverable in Telegram's
// command menu. Call before Poll; getMe identifies commands addressed to this bot.
func (t *Telegram) ConfigureCommands(ctx context.Context) error {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.api(ctx, "getMe", map[string]any{}, &me); err != nil {
		return err
	}
	if me.Username == "" {
		return errors.New("Telegram bot username missing")
	}
	t.username = me.Username
	commands := []map[string]string{
		{"command": "new", "description": "Начать новый диалог"},
		{"command": "stop", "description": "Остановить текущую работу"},
	}
	return t.api(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

func (t *Telegram) command(text string) (name string, otherBot bool) {
	text = strings.TrimSpace(text)
	command, username, directed := strings.Cut(text, "@")
	if command != "/new" && command != "/stop" {
		return "", false
	}
	if directed && (t.username == "" || !strings.EqualFold(username, t.username)) {
		return "", true
	}
	return strings.TrimPrefix(command, "/"), false
}

func (t *Telegram) api(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.config.BaseURL+"/bot"+t.config.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	return t.response(ctx, req, out)
}
func (t *Telegram) response(ctx context.Context, req *http.Request, out any) error {
	// Telegram can report rate limits and other API errors in either successful or error HTTP responses.
	resp, err := t.client.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Telegram request failed")
	}
	defer resp.Body.Close()
	data, err := limitedRead(resp.Body, 4<<20)
	if err != nil {
		return err
	}
	var envelope struct {
		OK          *bool           `json:"ok"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &TelegramError{Code: resp.StatusCode}
		}
		return errors.New("invalid Telegram response")
	}
	if envelope.OK == nil {
		return errors.New("invalid Telegram response envelope")
	}
	if !*envelope.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := envelope.Code
		if code == 0 {
			code = resp.StatusCode
		}
		description := strings.ToLower(envelope.Description)
		return &TelegramError{Code: code, RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second,
			NotModified:    code == 400 && strings.Contains(description, "message is not modified"),
			MessageMissing: code == 400 && (strings.Contains(description, "message to delete not found") || strings.Contains(description, "message to edit not found"))}
	}
	if out != nil && json.Unmarshal(envelope.Result, out) != nil {
		return errors.New("invalid Telegram result")
	}
	return nil
}

type telegramFile struct {
	Duration int    `json:"duration"`
	ID       string `json:"file_id"`
	Name     string `json:"file_name"`
	MIME     string `json:"mime_type"`
	Size     int64  `json:"file_size"`
}
type telegramMessage struct {
	ID          int64                `json:"message_id"`
	ThreadID    int                  `json:"message_thread_id"`
	Date        int64                `json:"date"`
	Text        string               `json:"text"`
	Caption     string               `json:"caption"`
	ReplyTo     *telegramMessage     `json:"reply_to_message"`
	Quote       *domain.MessageQuote `json:"quote"`
	RichMessage json.RawMessage      `json:"rich_message"`
	From        *struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
		IsBot     bool   `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID    int64  `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
	} `json:"chat"`
	Photo    []telegramFile `json:"photo"`
	Document *telegramFile  `json:"document"`
	Voice    *telegramFile  `json:"voice"`
	Audio    *telegramFile  `json:"audio"`
	Video    *telegramFile  `json:"video"`
}

func telegramReplyContext(m *telegramMessage) *domain.ReplyContext {
	if m == nil {
		return nil
	}
	reply := &domain.ReplyContext{MessageID: m.ID, Text: m.Text}
	if reply.Text == "" {
		reply.Text = m.Caption
	}
	// Our own sendRichMessage replies may have blocks instead of a text field.
	if reply.Text == "" && len(m.RichMessage) > 0 && string(m.RichMessage) != "null" {
		reply.Text = "[Rich message blocks]\n" + string(m.RichMessage)
	}
	if m.From != nil {
		reply.AuthorID = m.From.ID
		reply.AuthorName = strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
		reply.AuthorUsername = m.From.Username
		reply.AuthorIsBot = m.From.IsBot
	}
	for _, media := range []struct {
		kind    string
		present bool
	}{
		{"image", len(m.Photo) > 0},
		{"document", m.Document != nil},
		{"voice", m.Voice != nil},
		{"audio", m.Audio != nil},
		{"video", m.Video != nil},
	} {
		if media.present {
			reply.MediaKinds = append(reply.MediaKinds, media.kind)
		}
	}
	return reply
}

func (t *Telegram) Poll(ctx context.Context, offset int64) ([]Update, error) {
	var raw []struct {
		ID      int64            `json:"update_id"`
		Message *telegramMessage `json:"message"`
	}
	err := t.api(ctx, "getUpdates", map[string]any{"offset": offset, "limit": 100, "timeout": int(t.config.PollTimeout / time.Second), "allowed_updates": []string{"message"}}, &raw)
	if err != nil {
		return nil, err
	}
	out := make([]Update, 0, len(raw))
	for _, u := range raw {
		update := Update{Offset: u.ID + 1}
		m := u.Message
		if m == nil || m.From == nil || m.From.IsBot || m.From.ID <= 0 {
			out = append(out, update)
			continue
		}
		private := m.Chat.Type == "private"
		if (!private && (!t.groups[m.Chat.ID] || (m.Chat.Type != "group" && m.Chat.Type != "supergroup"))) || (private && (m.Chat.ID != m.From.ID || !t.users[m.From.ID])) {
			out = append(out, update)
			continue
		}
		conv := domain.Conversation{Title: m.Chat.Title, ChatID: m.Chat.ID, ThreadID: m.ThreadID, OwnerID: m.From.ID, Private: private}
		if private {
			conv.ID = fmt.Sprintf("personal:%d", m.From.ID)
		} else {
			conv.ID = fmt.Sprintf("telegram:group:%d:topic:%d", m.Chat.ID, m.ThreadID)
		}
		incoming := domain.Incoming{AuthorName: strings.TrimSpace(m.From.FirstName + " " + m.From.LastName), AuthorUsername: m.From.Username, ID: fmt.Sprintf("telegram:%d", u.ID), Conversation: conv, Text: m.Text, CreatedAt: time.Unix(m.Date, 0).UTC()}
		if incoming.Text == "" {
			incoming.Text = m.Caption
		}
		incoming.ReplyTo = telegramReplyContext(m.ReplyTo)
		incoming.Quote = m.Quote
		add := func(f *telegramFile, kind, name, mime string) {
			if f == nil {
				return
			}
			if f.Name != "" {
				name = path.Base(f.Name)
			}
			if f.MIME != "" {
				mime = f.MIME
			}
			incoming.Attachments = append(incoming.Attachments, domain.Attachment{FileID: f.ID, Name: name, MIME: mime, Kind: kind, SizeBytes: f.Size, DurationSeconds: f.Duration})
		}
		if len(m.Photo) > 0 {
			photo := m.Photo[len(m.Photo)-1]
			add(&photo, "image", "photo.jpg", "image/jpeg")
		}
		add(m.Document, "document", "document.bin", "application/octet-stream")
		add(m.Voice, "voice", "voice.ogg", "audio/ogg")
		add(m.Audio, "audio", "audio.mp3", "audio/mpeg")
		add(m.Video, "video", "video.mp4", "video/mp4")
		if len(incoming.Attachments) == 0 {
			command, otherBot := t.command(m.Text)
			if otherBot {
				out = append(out, update)
				continue
			}
			incoming.Command = command
		}
		if incoming.Text != "" || len(incoming.Attachments) > 0 {
			update.Incoming = &incoming
		}
		out = append(out, update)
	}
	return out, nil
}
func (t *Telegram) allowed(c domain.Conversation) bool {
	if c.Private {
		return t.users[c.OwnerID] && c.ChatID == c.OwnerID
	}
	return c.ChatID < 0 && t.groups[c.ChatID]
}
func (t *Telegram) Send(ctx context.Context, c domain.Conversation, text string) (string, error) {
	return t.sendRich(ctx, c, text, "markdown")
}

// SendFormatted sends a durable outbox chunk without parsing its text again.
func (t *Telegram) SendFormatted(ctx context.Context, c domain.Conversation, text string, entities []domain.TextEntity) (string, error) {
	return t.sendRich(ctx, c, messagefmt.LegacyHTML(messagefmt.Message{Text: text, Entities: entities}), "html")
}

func (t *Telegram) sendRich(ctx context.Context, c domain.Conversation, text, format string) (string, error) {
	if !t.allowed(c) {
		return "", errors.New("Telegram recipient is not allowed")
	}
	if text == "" {
		return "", errors.New("Telegram message is empty")
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 32768 {
		return "", errors.New("Telegram rich text exceeds 32768 characters or is invalid UTF-8")
	}
	payload := map[string]any{"chat_id": c.ChatID, "rich_message": map[string]any{format: text}}
	if c.ThreadID > 0 {
		payload["message_thread_id"] = c.ThreadID
	}
	var result struct {
		ID int64 `json:"message_id"`
	}
	if err := t.api(ctx, "sendRichMessage", payload, &result); err != nil {
		return "", deliveryError(err)
	}
	if result.ID <= 0 {
		return "", &TelegramDeliveryError{Cause: errors.New("missing message ID"), Unknown: true}
	}
	return strconv.FormatInt(result.ID, 10), nil
}

// Typing is ephemeral and must never block or fail a user's actual response.
func (t *Telegram) Typing(ctx context.Context, c domain.Conversation) error {
	if !t.allowed(c) {
		return errors.New("Telegram recipient is not allowed")
	}
	payload := map[string]any{"chat_id": c.ChatID, "action": "typing"}
	if c.ThreadID > 0 {
		payload["message_thread_id"] = c.ThreadID
	}
	return t.api(ctx, "sendChatAction", payload, nil)
}
func (t *Telegram) SendFile(ctx context.Context, c domain.Conversation, name string, data []byte) (string, error) {
	return t.sendUpload(ctx, c, name, data, "document", messagefmt.Message{})
}

// SendArtifact sends one media message. Choose the method before attempting
// delivery; never resend as a document after an ambiguous photo acknowledgement.
func (t *Telegram) SendArtifact(ctx context.Context, c domain.Conversation, artifact domain.Artifact, caption messagefmt.Message) (string, error) {
	if !messagefmt.FitsCaption(caption) {
		return "", errors.New("Telegram media caption exceeds size limit or is invalid UTF-8")
	}
	field := "document"
	if len(artifact.Data) <= 10<<20 {
		config, format, err := image.DecodeConfig(bytes.NewReader(artifact.Data))
		if err == nil && (format == "jpeg" || format == "png") && config.Width > 0 && config.Height > 0 && config.Width+config.Height <= 10000 && max(config.Width, config.Height) <= 20*min(config.Width, config.Height) {
			field = "photo"
		}
	}
	return t.sendUpload(ctx, c, artifact.Name, artifact.Data, field, caption)
}

func (t *Telegram) sendUpload(ctx context.Context, c domain.Conversation, name string, data []byte, field string, caption messagefmt.Message) (string, error) {
	if !t.allowed(c) {
		return "", errors.New("Telegram recipient is not allowed")
	}
	if int64(len(data)) > t.config.MaxFileBytes {
		return "", errors.New("Telegram file exceeds size limit")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", strconv.FormatInt(c.ChatID, 10))
	if c.ThreadID > 0 {
		_ = w.WriteField("message_thread_id", strconv.Itoa(c.ThreadID))
	}
	if caption.Text != "" {
		_ = w.WriteField("caption", caption.Text)
	}
	if len(caption.Entities) > 0 {
		entities, err := json.Marshal(caption.Entities)
		if err != nil {
			return "", err
		}
		_ = w.WriteField("caption_entities", string(entities))
	}
	part, err := w.CreateFormFile(field, path.Base(name))
	if err != nil {
		return "", err
	}
	if _, err = part.Write(data); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	method := "sendDocument"
	if field == "photo" {
		method = "sendPhoto"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.config.BaseURL+"/bot"+t.config.Token+"/"+method, &body)
	if err != nil {
		return "", errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var out struct {
		ID int64 `json:"message_id"`
	}
	if err = t.response(ctx, req, &out); err != nil {
		return "", deliveryError(err)
	}
	if out.ID <= 0 {
		return "", &TelegramDeliveryError{Cause: errors.New("missing message ID"), Unknown: true}
	}
	return strconv.FormatInt(out.ID, 10), nil
}
func (t *Telegram) Download(ctx context.Context, fileID string) ([]byte, error) {
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := t.api(ctx, "getFile", map[string]string{"file_id": fileID}, &file); err != nil {
		return nil, err
	}
	if file.Size > t.config.MaxFileBytes {
		return nil, errors.New("Telegram file exceeds size limit")
	}
	if file.Path == "" || strings.HasPrefix(file.Path, "/") || strings.Contains(file.Path, "\\") {
		return nil, errors.New("invalid Telegram file path")
	}
	segments := strings.Split(file.Path, "/")
	for i, s := range segments {
		if s == "." || s == ".." || s == "" {
			return nil, errors.New("invalid Telegram file path")
		}
		segments[i] = url.PathEscape(s)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.config.BaseURL+"/file/bot"+t.config.Token+"/"+strings.Join(segments, "/"), nil)
	if err != nil {
		return nil, errors.New("invalid Telegram download request")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Telegram download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Telegram file HTTP status %d", resp.StatusCode)
	}
	return limitedRead(resp.Body, t.config.MaxFileBytes)
}

var _ domain.Messenger = (*Telegram)(nil)
