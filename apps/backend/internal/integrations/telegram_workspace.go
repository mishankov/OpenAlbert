package integrations

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
)

var (
	ErrCallbackUnauthorized = errors.New("callback query is not authorized")
	ErrCallbackUnhandled    = errors.New("no handler registered for callback query")
)

func callbackQuery(b *telegramCallbackQuery) domain.CallbackQuery {
	q := domain.CallbackQuery{
		ID:      b.ID,
		UserID:  b.From.ID,
		FromBot: b.From.IsBot,
		Data:    b.Data,
	}
	if b.Message != nil {
		q.ChatID = b.Message.Chat.ID
		q.MessageID = b.Message.ID
		q.Private = b.Message.Chat.Type == "private"
		if b.Message.IsTopicMessage {
			q.ThreadID = b.Message.ThreadID
		}
	}
	return q
}

func (t *Telegram) chatAllowed(chatID int64) bool {
	if chatID < 0 {
		return t.groups[chatID]
	}
	return chatID > 0 && t.users[chatID]
}

func validateForumTopicName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || utf8.RuneCountInString(name) > 128 {
		return "", errors.New("forum topic name must be valid UTF-8 and 1–128 characters")
	}
	return name, nil
}

func validateInlineKeyboard(markup *domain.InlineKeyboardMarkup) error {
	if markup == nil {
		return nil
	}
	total := 0
	for _, row := range markup.InlineKeyboard {
		if len(row) == 0 {
			return errors.New("inline keyboard row must not be empty")
		}
		for _, btn := range row {
			total++
			if total > 100 {
				return errors.New("inline keyboard exceeds 100 buttons")
			}
			text := strings.TrimSpace(btn.Text)
			if text == "" || !utf8.ValidString(btn.Text) || utf8.RuneCountInString(btn.Text) > 64 {
				return errors.New("inline button text must be valid UTF-8 and 1–64 characters")
			}
			hasData := btn.CallbackData != ""
			hasURL := btn.URL != ""
			if hasData == hasURL {
				return errors.New("inline button must specify exactly one of callback_data or url")
			}
			if hasData && (!utf8.ValidString(btn.CallbackData) || len(btn.CallbackData) > 64) {
				return errors.New("inline button callback_data must be valid UTF-8 and 1–64 bytes")
			}
			if hasURL {
				u, err := url.Parse(btn.URL)
				if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
					return errors.New("inline button url must be an HTTP(S) URL")
				}
			}
		}
	}
	return nil
}

// CreateForumTopic creates a new forum topic in an allowed Telegram supergroup.
func (t *Telegram) CreateForumTopic(ctx context.Context, chatID int64, name string, iconColor int) (domain.ForumTopic, error) {
	if chatID >= 0 || !t.groups[chatID] {
		return domain.ForumTopic{}, errors.New("Telegram forum topic chat is not allowed")
	}
	cleanName, err := validateForumTopicName(name)
	if err != nil {
		return domain.ForumTopic{}, err
	}
	if iconColor < 0 {
		return domain.ForumTopic{}, errors.New("forum topic icon_color must be nonnegative")
	}
	payload := map[string]any{"chat_id": chatID, "name": cleanName}
	if iconColor > 0 {
		payload["icon_color"] = iconColor
	}
	var topic domain.ForumTopic
	if err := t.api(ctx, "createForumTopic", payload, &topic); err != nil {
		return domain.ForumTopic{}, deliveryError(err)
	}
	if topic.ThreadID <= 0 {
		return domain.ForumTopic{}, &TelegramDeliveryError{Cause: errors.New("missing forum topic thread ID"), Unknown: true}
	}
	if topic.Name == "" {
		topic.Name = cleanName
	}
	return topic, nil
}

// EditForumTopic renames a forum topic in an allowed Telegram supergroup.
func (t *Telegram) EditForumTopic(ctx context.Context, chatID int64, threadID int, name string) error {
	if chatID >= 0 || !t.groups[chatID] {
		return errors.New("Telegram forum topic chat is not allowed")
	}
	if threadID <= 0 {
		return errors.New("forum topic thread_id must be positive")
	}
	cleanName, err := validateForumTopicName(name)
	if err != nil {
		return err
	}
	err = t.api(ctx, "editForumTopic", map[string]any{
		"chat_id":           chatID,
		"message_thread_id": threadID,
		"name":              cleanName,
	}, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.NotModified {
		return nil
	}
	return err
}

// CloseForumTopic closes an open forum topic in an allowed Telegram supergroup.
func (t *Telegram) CloseForumTopic(ctx context.Context, chatID int64, threadID int) error {
	if chatID >= 0 || !t.groups[chatID] {
		return errors.New("Telegram forum topic chat is not allowed")
	}
	if threadID <= 0 {
		return errors.New("forum topic thread_id must be positive")
	}
	err := t.api(ctx, "closeForumTopic", map[string]any{
		"chat_id":           chatID,
		"message_thread_id": threadID,
	}, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.NotModified {
		return nil
	}
	return err
}

// ReopenForumTopic reopens a closed forum topic in an allowed Telegram supergroup.
func (t *Telegram) ReopenForumTopic(ctx context.Context, chatID int64, threadID int) error {
	if chatID >= 0 || !t.groups[chatID] {
		return errors.New("Telegram forum topic chat is not allowed")
	}
	if threadID <= 0 {
		return errors.New("forum topic thread_id must be positive")
	}
	err := t.api(ctx, "reopenForumTopic", map[string]any{
		"chat_id":           chatID,
		"message_thread_id": threadID,
	}, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.NotModified {
		return nil
	}
	return err
}

// SendMessageWithButtons sends a standard Telegram message with optional UTF-16
// formatting entities and an optional inline keyboard markup.
func (t *Telegram) SendMessageWithButtons(ctx context.Context, c domain.Conversation, text string, entities []domain.TextEntity, replyMarkup *domain.InlineKeyboardMarkup) (int64, error) {
	if !t.allowed(c) {
		return 0, errors.New("Telegram recipient is not allowed")
	}
	if text == "" || !utf8.ValidString(text) || len(utf16.Encode([]rune(text))) > messagefmt.TextLimit {
		return 0, errors.New("Telegram text must contain 1–4096 UTF-16 units and valid UTF-8")
	}
	if err := validateInlineKeyboard(replyMarkup); err != nil {
		return 0, err
	}
	payload := map[string]any{"chat_id": c.ChatID, "text": text}
	if len(entities) > 0 {
		payload["entities"] = entities
	}
	if c.ThreadID > 0 {
		payload["message_thread_id"] = c.ThreadID
	}
	if replyMarkup != nil && len(replyMarkup.InlineKeyboard) > 0 {
		payload["reply_markup"] = replyMarkup
	}
	var result struct {
		ID int64 `json:"message_id"`
	}
	if err := t.api(ctx, "sendMessage", payload, &result); err != nil {
		return 0, deliveryError(err)
	}
	if result.ID <= 0 {
		return 0, &TelegramDeliveryError{Cause: errors.New("missing message ID"), Unknown: true}
	}
	return result.ID, nil
}

// EditMessageText edits a Telegram message's text and optional inline keyboard markup.
func (t *Telegram) EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, replyMarkup *domain.InlineKeyboardMarkup) error {
	return t.EditFormattedMessageText(ctx, chatID, messageID, text, nil, replyMarkup)
}

// EditFormattedMessageText edits a Telegram message with optional UTF-16 entities
// and optional inline keyboard markup.
func (t *Telegram) EditFormattedMessageText(ctx context.Context, chatID int64, messageID int64, text string, entities []domain.TextEntity, replyMarkup *domain.InlineKeyboardMarkup) error {
	if !t.chatAllowed(chatID) {
		return errors.New("Telegram recipient is not allowed")
	}
	if messageID <= 0 {
		return errors.New("Telegram message_id must be positive")
	}
	if text == "" || !utf8.ValidString(text) || len(utf16.Encode([]rune(text))) > messagefmt.TextLimit {
		return errors.New("Telegram text must contain 1–4096 UTF-16 units and valid UTF-8")
	}
	if err := validateInlineKeyboard(replyMarkup); err != nil {
		return err
	}
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if len(entities) > 0 {
		payload["entities"] = entities
	}
	if replyMarkup != nil {
		if replyMarkup.InlineKeyboard == nil {
			payload["reply_markup"] = map[string]any{"inline_keyboard": [][]domain.InlineButton{}}
		} else {
			payload["reply_markup"] = replyMarkup
		}
	}
	err := t.api(ctx, "editMessageText", payload, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.NotModified {
		return nil
	}
	return err
}

// PinChatMessage pins a message in an allowed Telegram chat.
func (t *Telegram) PinChatMessage(ctx context.Context, chatID int64, messageID int64, disableNotification bool) error {
	if !t.chatAllowed(chatID) {
		return errors.New("Telegram recipient is not allowed")
	}
	if messageID <= 0 {
		return errors.New("Telegram message_id must be positive")
	}
	return t.api(ctx, "pinChatMessage", map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"disable_notification": disableNotification,
	}, nil)
}

// AnswerCallbackQuery acknowledges an inline keyboard callback_query to dismiss
// the client spinner and optionally show a toast or alert.
func (t *Telegram) AnswerCallbackQuery(ctx context.Context, callbackID string, text string, showAlert bool) error {
	if callbackID == "" || !utf8.ValidString(text) {
		return errors.New("invalid callback query answer")
	}
	runes := []rune(text)
	if len(runes) > 200 {
		text = string(runes[:200])
	}
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	if showAlert {
		payload["show_alert"] = true
	}
	return t.api(ctx, "answerCallbackQuery", payload, nil)
}

var _ domain.WorkspaceTransport = (*Telegram)(nil)

// CallbackRouter routes Telegram inline-keyboard callback queries by data prefix
// while enforcing owner-only authorization for workspace and owner callbacks.
type CallbackRouter struct {
	OwnerUserID      int64
	WorkspaceGroupID int64
	Answer           func(ctx context.Context, callbackID, text string, showAlert bool) error

	mu       sync.RWMutex
	handlers map[string]domain.CallbackHandler
}

func NewCallbackRouter(ownerUserID, workspaceGroupID int64, answer func(ctx context.Context, callbackID, text string, showAlert bool) error) *CallbackRouter {
	return &CallbackRouter{
		OwnerUserID:      ownerUserID,
		WorkspaceGroupID: workspaceGroupID,
		Answer:           answer,
		handlers:         map[string]domain.CallbackHandler{},
	}
}

// RegisterPrefix subscribes a handler for callback_data matching prefix (e.g. "t3:").
// Longer prefixes take precedence when multiple prefixes match.
func (r *CallbackRouter) RegisterPrefix(prefix string, handler domain.CallbackHandler) {
	if prefix == "" || handler == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[string]domain.CallbackHandler{}
	}
	r.handlers[prefix] = handler
}

// RegisterPrefixFunc is a convenience wrapper around RegisterPrefix.
func (r *CallbackRouter) RegisterPrefixFunc(prefix string, fn func(ctx context.Context, q domain.CallbackQuery) (domain.CallbackResult, error)) {
	if fn == nil {
		return
	}
	r.RegisterPrefix(prefix, domain.CallbackHandlerFunc(fn))
}

func (r *CallbackRouter) match(data string) (domain.CallbackHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best domain.CallbackHandler
	bestLen := 0
	for prefix, h := range r.handlers {
		if strings.HasPrefix(data, prefix) && len(prefix) > bestLen {
			best = h
			bestLen = len(prefix)
		}
	}
	return best, best != nil
}

// Handles reports whether this router owns the callback query: either because a
// prefix handler is registered for its data or because it originated in the
// configured owner workspace supergroup.
func (r *CallbackRouter) Handles(q domain.CallbackQuery) bool {
	if r == nil {
		return false
	}
	if r.WorkspaceGroupID < 0 && q.ChatID == r.WorkspaceGroupID {
		return true
	}
	_, ok := r.match(q.Data)
	return ok
}

func (r *CallbackRouter) authorized(q domain.CallbackQuery) bool {
	if r.OwnerUserID <= 0 || q.UserID != r.OwnerUserID || q.FromBot {
		return false
	}
	if q.Private && q.ChatID == r.OwnerUserID {
		return true
	}
	return r.WorkspaceGroupID < 0 && !q.Private && q.ChatID == r.WorkspaceGroupID
}

// Dispatch verifies owner authorization, invokes the matching prefix handler,
// and always answers the callback query to release the Telegram client spinner.
func (r *CallbackRouter) Dispatch(ctx context.Context, q domain.CallbackQuery) error {
	answer := func(text string, showAlert bool) error {
		if r.Answer == nil || q.ID == "" {
			return nil
		}
		answerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return r.Answer(answerCtx, q.ID, text, showAlert)
	}
	if !r.authorized(q) {
		return errors.Join(ErrCallbackUnauthorized, answer("Unauthorized", false))
	}
	handler, ok := r.match(q.Data)
	if !ok {
		return errors.Join(ErrCallbackUnhandled, answer("Action unavailable", false))
	}
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	res, err := handler.HandleCallback(opCtx, q)
	cancel()
	text := res.Text
	if err != nil && text == "" {
		text = "Action failed"
	}
	return errors.Join(err, answer(text, res.ShowAlert))
}
