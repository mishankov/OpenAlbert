package domain

import "context"

// InlineButton is one standard Telegram Bot API inline keyboard button.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// InlineKeyboardMarkup is the standard Telegram Bot API reply_markup payload
// for inline keyboards.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

// ForumTopic describes a Telegram forum topic created in a supergroup.
type ForumTopic struct {
	ThreadID  int    `json:"message_thread_id"`
	Name      string `json:"name"`
	IconColor int    `json:"icon_color,omitempty"`
}

// CallbackQuery represents a parsed Telegram inline-keyboard callback_query update.
type CallbackQuery struct {
	ID        string `json:"id"`
	UserID    int64  `json:"user_id"`
	ChatID    int64  `json:"chat_id"`
	ThreadID  int    `json:"thread_id,omitempty"`
	MessageID int64  `json:"message_id"`
	Private   bool   `json:"private"`
	Data      string `json:"data"`
	FromBot   bool   `json:"from_bot,omitempty"`
}

// CallbackResult specifies the optional feedback passed to answerCallbackQuery.
type CallbackResult struct {
	Text      string
	ShowAlert bool
}

// CallbackHandler processes an authorized Telegram callback query.
type CallbackHandler interface {
	HandleCallback(ctx context.Context, q CallbackQuery) (CallbackResult, error)
}

// CallbackHandlerFunc adapts a function to CallbackHandler.
type CallbackHandlerFunc func(ctx context.Context, q CallbackQuery) (CallbackResult, error)

func (f CallbackHandlerFunc) HandleCallback(ctx context.Context, q CallbackQuery) (CallbackResult, error) {
	return f(ctx, q)
}

// WorkspaceTransport exposes standard Telegram Bot API primitives needed by the
// owner workspace supergroup tools and controller.
type WorkspaceTransport interface {
	CreateForumTopic(ctx context.Context, chatID int64, name string, iconColor int) (ForumTopic, error)
	EditForumTopic(ctx context.Context, chatID int64, threadID int, name string) error
	CloseForumTopic(ctx context.Context, chatID int64, threadID int) error
	ReopenForumTopic(ctx context.Context, chatID int64, threadID int) error
	SendMessageWithButtons(ctx context.Context, conv Conversation, text string, entities []TextEntity, replyMarkup *InlineKeyboardMarkup) (int64, error)
	EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, replyMarkup *InlineKeyboardMarkup) error
	PinChatMessage(ctx context.Context, chatID int64, messageID int64, disableNotification bool) error
}
