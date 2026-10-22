package integrations

import (
	"context"
	"errors"
	"strconv"
	"unicode/utf8"

	"jarvis/internal/domain"
)

// Progress uses short plain text, separate from the user's Rich Markdown reply.
func (t *Telegram) SendProgress(ctx context.Context, c domain.Conversation, text string) (string, error) {
	if !t.allowed(c) {
		return "", errors.New("Telegram recipient is not allowed")
	}
	if !validProgressText(text) {
		return "", errors.New("invalid Telegram progress text")
	}
	payload := map[string]any{"chat_id": c.ChatID, "text": text, "disable_notification": true, "link_preview_options": map[string]bool{"is_disabled": true}}
	if c.ThreadID > 0 {
		payload["message_thread_id"] = c.ThreadID
	}
	var result struct {
		ID int64 `json:"message_id"`
	}
	if err := t.api(ctx, "sendMessage", payload, &result); err != nil {
		return "", deliveryError(err)
	}
	if result.ID <= 0 {
		return "", &TelegramDeliveryError{Cause: errors.New("missing progress message ID"), Unknown: true}
	}
	return strconv.FormatInt(result.ID, 10), nil
}

func validProgressText(text string) bool {
	return utf8.ValidString(text) && text != "" && utf8.RuneCountInString(text) <= 600
}

func (t *Telegram) EditProgress(ctx context.Context, c domain.Conversation, id, text string) error {
	if !t.allowed(c) {
		return errors.New("Telegram recipient is not allowed")
	}
	messageID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || messageID <= 0 || !validProgressText(text) {
		return errors.New("invalid Telegram progress edit")
	}
	err = t.api(ctx, "editMessageText", map[string]any{"chat_id": c.ChatID, "message_id": messageID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.NotModified {
		return nil
	}
	return err
}

func (t *Telegram) DeleteProgress(ctx context.Context, c domain.Conversation, id string) error {
	if !t.allowed(c) {
		return errors.New("Telegram recipient is not allowed")
	}
	messageID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || messageID <= 0 {
		return errors.New("invalid Telegram progress deletion")
	}
	err = t.api(ctx, "deleteMessage", map[string]any{"chat_id": c.ChatID, "message_id": messageID}, nil)
	var apiErr *TelegramError
	if errors.As(err, &apiErr) && apiErr.MessageMissing {
		return nil
	}
	return err
}
