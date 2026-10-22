package inspector

import (
	"context"
	"encoding/json"
	"jarvis/internal/domain"
	"time"
)

type Filter struct {
	Query, Status, Trigger, Conversation string
	Before                               int64
	Limit                                int
}
type Session struct {
	domain.Session
	Seq          int64               `json:"seq"`
	Conversation domain.Conversation `json:"conversation"`
	Trigger      string              `json:"trigger"`
	Analyzed     bool                `json:"analyzed"`
	MessageCount int                 `json:"message_count"`
	ToolCount    int                 `json:"tool_count"`
	TraceCount   int                 `json:"trace_count"`
	Preview      string              `json:"preview"`
}
type SavedMessage struct {
	Seq        int64          `json:"seq"`
	Key        string         `json:"key"`
	RecordedAt *time.Time     `json:"recorded_at,omitempty"`
	Message    domain.Message `json:"message"`
}
type Detail struct {
	Audio     []AudioResult     `json:"audio"`
	Analysis  json.RawMessage   `json:"analysis,omitempty"`
	Session   Session           `json:"session"`
	Messages  []SavedMessage    `json:"messages"`
	NextAfter int64             `json:"next_after,omitempty"`
	Traces    []domain.Trace    `json:"traces"`
	Turns     []json.RawMessage `json:"turns"`
	Outgoing  []domain.Outgoing `json:"outgoing"`
	Artifacts []domain.Artifact `json:"artifacts"`
}

// Source text is read from the scoped inbox, never reconstructed by stripping
// markers from the model's prepared context (which may contain the same text).
type AudioResult struct {
	domain.AudioResult
	OriginalText *string    `json:"original_text,omitempty"`
	ReceivedAt   *time.Time `json:"received_at,omitempty"`
}
type Store interface {
	InspectSessions(context.Context, Filter) ([]Session, error)
	InspectSession(context.Context, string, int64) (Detail, bool, error)
	InspectTrace(context.Context, string, string) (domain.Trace, bool, error)
}
