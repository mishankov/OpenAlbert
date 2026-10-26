package domain

import (
	"context"
	"errors"
	"time"
)

var ErrBrowserCapacity = errors.New("all browser slots are occupied")

type BrowserOwner struct {
	ConversationID string
	SessionID      string
	Background     bool
	TaskID         string
}
type BrowserRequest struct {
	Action       string `json:"action"`
	PageID       string `json:"page_id,omitempty"`
	URL          string `json:"url,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Text         string `json:"text,omitempty"`
	Milliseconds int    `json:"milliseconds,omitempty"`
}
type BrowserElement struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}
type BrowserPage struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}
type BrowserResult struct {
	Truncated  bool             `json:"observation_truncated,omitempty"`
	InstanceID string           `json:"instance_id"`
	Fresh      bool             `json:"fresh,omitempty"`
	PageID     string           `json:"page_id,omitempty"`
	URL        string           `json:"url,omitempty"`
	Title      string           `json:"title,omitempty"`
	Text       string           `json:"text,omitempty"`
	Elements   []BrowserElement `json:"elements,omitempty"`
	Pages      []BrowserPage    `json:"pages,omitempty"`
	ArtifactID string           `json:"artifact_id,omitempty"`
	MIME       string           `json:"mime,omitempty"`
	Filename   string           `json:"filename,omitempty"`
	Data       []byte           `json:"-"`
}

// Before is called after admission and before any effect. It saves the durable
// attempt marker, preventing blind replay after a crash between effect and receipt.
type Browser interface {
	Execute(context.Context, BrowserOwner, BrowserRequest, func() error) (BrowserResult, error)
	CloseSession(string)
	Reconcile(map[string]bool, time.Time)
	CloseTask(string)
	Close() error
}
