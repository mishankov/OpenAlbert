package integrations

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/libz"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

type telegramWidgetQuery struct {
	ID   string `json:"id"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	MessageID int64 `json:"message_id"`
	Type      struct {
		Type string `json:"type"`
	} `json:"type"`
	Data            string `json:"data"`
	WidgetID        string `json:"widget_id"`
	InlineMessageID string `json:"inline_message_id"`
}

type telegramCallbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}

func widgetQuery(w *telegramWidgetQuery, b *telegramCallbackQuery) domain.WidgetQuery {
	if w != nil {
		return domain.WidgetQuery{ID: w.ID, UserID: w.From.ID, ChatID: w.Chat.ID, MessageID: w.MessageID, Private: w.Chat.Type == "private", Data: w.Data, DataType: w.Type.Type == "data" && !w.From.IsBot && w.WidgetID == "" && w.InlineMessageID == ""}
	}
	q := domain.WidgetQuery{ID: b.ID, UserID: b.From.ID, Data: b.Data, Callback: true, DataType: !b.From.IsBot}
	if b.Message != nil {
		q.ChatID = b.Message.Chat.ID
		q.MessageID = b.Message.ID
		q.Private = b.Message.Chat.Type == "private"
	}
	return q
}

// ValidateWidget checks byte budgets before an API call. Telegram compresses HTML
// with zlib level 9; rune counts are not sufficient for this limit.
func ValidateWidget(w domain.Widget) error {
	if !utf8.ValidString(w.HTML) || strings.ContainsRune(w.HTML, 0) || len(w.HTML) == 0 || len(w.HTML) > 32768 {
		return errors.New("widget HTML must be valid UTF-8 and contain 1–32768 bytes")
	}
	size, err := widgetCompressedSize(w.HTML)
	if err != nil {
		return err
	}
	if size > 4096 {
		return errors.New("widget HTML exceeds 4096 bytes after zlib level 9 compression")
	}
	if w.Width != 0 && w.Width < 200 || w.Height != 0 && (w.Height < 50 || w.Height > 600) {
		return errors.New("widget width must be at least 200 and height 50–600, or omitted")
	}
	if !utf8.ValidString(w.Caption) || len(utf16.Encode([]rune(w.Caption))) > 1024 {
		return errors.New("widget caption must be valid UTF-8 and at most 1024 UTF-16 units")
	}
	return validateWidgetData(domain.WidgetData{PublicData: &w.PublicData, ChatData: &w.ChatData})
}

// Use the original zlib algorithm, translated to Go, to match Telegram's
// zlib.compress(html, 9) reference. compress/zlib produces different sizes.
// All buffers belong to libc's allocator; no Go pointers cross the uintptr API.
func widgetCompressedSize(html string) (int, error) {
	tls := libc.NewTLS()
	defer tls.Close()
	source, err := libc.CString(html)
	if err != nil {
		return 0, err
	}
	defer libc.Xfree(tls, source)
	capacity := uintptr(2*len(html) + 64) // More than zlib's compressBound for the <=32KiB input.
	dest := libc.Xmalloc(tls, libc.Tsize_t(capacity))
	if dest == 0 {
		return 0, errors.New("cannot allocate widget compression buffer")
	}
	defer libc.Xfree(tls, dest)
	length := libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(libz.Tz_size_t(0))))
	if length == 0 {
		return 0, errors.New("cannot allocate widget compression length")
	}
	defer libc.Xfree(tls, length)
	libc.AtomicStorePUintptr(length, capacity)
	if code := libz.Xcompress2_z(tls, dest, length, source, libz.Tz_size_t(len(html)), 9); code != 0 {
		return 0, errors.New("widget zlib compression failed")
	}
	return int(libc.AtomicLoadPUintptr(length)), nil
}

func validateWidgetData(d domain.WidgetData) error {
	if d.PublicData == nil && d.ChatData == nil {
		return errors.New("provide chat_data or public_data; an empty string clears it")
	}
	for _, value := range []*string{d.PublicData, d.ChatData} {
		if value != nil && (!utf8.ValidString(*value) || strings.ContainsRune(*value, 0) || len(*value) > 1024) {
			return errors.New("widget state must be valid UTF-8 and at most 1024 bytes per field")
		}
	}
	return nil
}

func (t *Telegram) widgetAllowed(c domain.Conversation) error {
	if !c.Private {
		return errors.New("в проде виджеты работают только в личке")
	}
	if !t.allowed(c) {
		return errors.New("Telegram recipient is not allowed")
	}
	return nil
}

func widgetMarkup(buttons [][]domain.WidgetButton) map[string]any {
	if buttons == nil {
		buttons = [][]domain.WidgetButton{}
	}
	return map[string]any{"inline_keyboard": buttons}
}

func (t *Telegram) SendWidget(ctx context.Context, c domain.Conversation, w domain.Widget, buttons [][]domain.WidgetButton) (int64, error) {
	if err := t.widgetAllowed(c); err != nil {
		return 0, err
	}
	if err := ValidateWidget(w); err != nil {
		return 0, err
	}
	if t.config.Widgets == "off" {
		return 0, &WidgetFallbackError{Reason: "disabled"}
	}
	if t.config.Widgets == "auto" {
		t.widgetMu.Lock()
		cached, until := t.widgetUnavailable, t.widgetUnavailableUntil
		t.widgetMu.Unlock()
		if cached != nil && time.Now().Before(until) {
			return 0, cached
		}
	}
	event := telemetry.Start(ctx, t.config.Logger, "telegram.widget.send")
	event.Set("method", "sendWidget")
	event.Set("chat_id", c.ChatID)
	event.Set("html_bytes", len(w.HTML))
	compressed, _ := widgetCompressedSize(w.HTML) // Validation above already checked compression.
	event.Set("html_compressed_bytes", compressed)
	payload := map[string]any{"chat_id": c.ChatID, "html": w.HTML, "caption": w.Caption, "allow_queries": w.AllowQueries, "public_data": w.PublicData, "chat_data": w.ChatData, "reply_markup": widgetMarkup(buttons)}
	if c.ThreadID > 0 {
		payload["message_thread_id"] = c.ThreadID
	}
	if w.Width != 0 {
		payload["width"] = w.Width
	}
	if w.Height != 0 {
		payload["height"] = w.Height
	}
	id, err := t.sendWidgetMessage(ctx, "sendWidget", payload)
	var api *TelegramError
	if errors.As(err, &api) {
		event.Set("api_http_status", api.Status)
		event.Set("api_error_code", api.Code)
		event.Set("api_description", api.Description)
	}
	event.End(err)
	if err != nil && api != nil && api.Rejected && api.Status < 500 && (api.Code == 400 || api.Code == 404) {
		reason := "api_rejected"
		switch {
		case api.Code == 404, strings.Contains(api.Description, "INPUT_MEDIA_WIDGET_NOT_ALLOWED"):
			reason = "api_not_supported"
		case strings.Contains(api.Description, "INPUT_MEDIA_WIDGET_"):
			reason = "validation"
		}
		rejected := &WidgetFallbackError{Reason: reason, APIError: &TelegramAPIDiagnostic{HTTPStatus: api.Status, Code: api.Code, Description: api.Description}}
		if reason == "api_not_supported" && t.config.Widgets == "auto" {
			t.widgetMu.Lock()
			t.widgetUnavailable, t.widgetUnavailableUntil = rejected, time.Now().Add(time.Hour)
			t.widgetMu.Unlock()
		}
		return 0, rejected
	}
	return id, err
}

func (t *Telegram) SendWidgetFallback(ctx context.Context, c domain.Conversation, text string, buttons [][]domain.WidgetButton) (int64, error) {
	if !t.allowed(c) {
		return 0, errors.New("Telegram recipient is not allowed")
	}
	payload := map[string]any{"chat_id": c.ChatID, "text": text, "reply_markup": widgetMarkup(buttons)}
	if c.ThreadID > 0 {
		payload["message_thread_id"] = c.ThreadID
	}
	return t.sendWidgetMessage(ctx, "sendMessage", payload)
}

func (t *Telegram) sendWidgetMessage(ctx context.Context, method string, payload any) (int64, error) {
	var result struct {
		ID int64 `json:"message_id"`
	}
	if err := t.api(ctx, method, payload, &result); err != nil {
		return 0, deliveryError(err)
	}
	if result.ID <= 0 {
		return 0, &TelegramDeliveryError{Cause: errors.New("missing message ID"), Unknown: true}
	}
	return result.ID, nil
}

func (t *Telegram) EditWidgetData(ctx context.Context, c domain.Conversation, id int64, d domain.WidgetData) error {
	if err := t.widgetAllowed(c); err != nil {
		return err
	}
	if id <= 0 {
		return errors.New("widget message_id must be positive")
	}
	if err := validateWidgetData(d); err != nil {
		return err
	}
	payload := map[string]any{"chat_id": c.ChatID, "message_id": id}
	if d.PublicData != nil {
		payload["public_data"] = *d.PublicData
	}
	if d.ChatData != nil {
		payload["chat_data"] = *d.ChatData
	}
	return t.api(ctx, "editMessageWidgetData", payload, nil)
}

func (t *Telegram) EditWidgetControls(ctx context.Context, c domain.Conversation, id int64, widget bool, text string, buttons [][]domain.WidgetButton) error {
	if err := t.widgetAllowed(c); err != nil {
		return err
	}
	method, field := "editMessageText", "text"
	if widget {
		method, field = "editMessageCaption", "caption"
	}
	err := t.api(ctx, method, map[string]any{"chat_id": c.ChatID, "message_id": id, field: text, "reply_markup": widgetMarkup(buttons)}, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.NotModified {
		return nil
	}
	return err
}

func (t *Telegram) AnswerWidgetQuery(ctx context.Context, q domain.WidgetQuery, data string) error {
	if q.ID == "" || !utf8.ValidString(data) || len(data) > 1024 {
		return errors.New("invalid widget query answer")
	}
	if q.Callback {
		// Callback text has a smaller 200-character budget.
		runes := []rune(data)
		if len(runes) > 200 {
			data = string(runes[:200])
		}
		return t.api(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID, "text": data}, nil)
	}
	return t.api(ctx, "answerWidgetQuery", map[string]any{"widget_query_id": q.ID, "data": data}, nil)
}

// TelegramAPIDiagnostic is returned only as metadata for a delivered text
// fallback. It is not a tool failure and contains no request payload.
type TelegramAPIDiagnostic struct {
	HTTPStatus  int    `json:"http_status"`
	Code        int    `json:"error_code"`
	Description string `json:"description"`
}

// WidgetFallbackError is an explicit rejection or local policy decision for
// which replacing the widget with a text reply is safe.
type WidgetFallbackError struct {
	Reason   string
	APIError *TelegramAPIDiagnostic
}

func (e *WidgetFallbackError) Error() string { return "Telegram widget unavailable: " + e.Reason }
