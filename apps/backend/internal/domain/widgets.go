package domain

import "context"

// Widget is a static Telegram widget. State is opaque UTF-8, not HTML.
type Widget struct {
	HTML         string `json:"html"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	PublicData   string `json:"public_data,omitempty"`
	ChatData     string `json:"chat_data,omitempty"`
	Caption      string `json:"caption"`
	AllowQueries bool   `json:"allow_queries"`
}

type WidgetData struct {
	PublicData *string `json:"public_data,omitempty"`
	ChatData   *string `json:"chat_data,omitempty"`
}

type WidgetButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// WidgetQuery also represents an inline-keyboard callback, after transport parsing.
type WidgetQuery struct {
	ID        string
	UserID    int64
	ChatID    int64
	MessageID int64
	Private   bool
	Data      string
	Callback  bool
	DataType  bool
}

type WidgetTransport interface {
	SendWidget(context.Context, Conversation, Widget, [][]WidgetButton) (int64, error)
	SendWidgetFallback(context.Context, Conversation, string, [][]WidgetButton) (int64, error)
	EditWidgetData(context.Context, Conversation, int64, WidgetData) error
	EditWidgetControls(context.Context, Conversation, int64, bool, string, [][]WidgetButton) error
	AnswerWidgetQuery(context.Context, WidgetQuery, string) error
}
