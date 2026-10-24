package domain

import (
	"context"
	"time"
)

// LogFilter contains exact filters, never caller-supplied LogsQL or an endpoint.
// ConversationID is authorized by the agent before the reader is called.
type LogFilter struct {
	ConversationID, SessionID, TurnID, Operation, Tool, Result, ErrorCode string
	Since, Until                                                          time.Time
	Limit                                                                 int
}

type LogCount struct {
	Operation string `json:"operation"`
	Result    string `json:"result"`
	ErrorCode string `json:"error_code,omitempty"`
	Count     int64  `json:"count"`
}

type LogSearch struct {
	ConversationID   string              `json:"conversation_id"`
	Since            time.Time           `json:"since"`
	Until            time.Time           `json:"until"`
	Summary          []LogCount          `json:"summary"`
	SummaryTruncated bool                `json:"summary_truncated,omitempty"`
	Events           []map[string]string `json:"events"`
	Truncated        bool                `json:"truncated"`
	Note             string              `json:"note"`
}

type LogReader interface {
	Search(context.Context, LogFilter) (LogSearch, error)
}
