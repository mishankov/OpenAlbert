package domain

import (
	"encoding/json"
	"time"
)

// SearchTrace records safe operational metadata for one web_search attempt.
// Never store API keys, raw queries, domain filters, or upstream error bodies.
type SearchTrace struct {
	Provider     string     `json:"provider"`
	Mode         string     `json:"mode,omitempty"`
	ResolvedMode string     `json:"resolved_mode,omitempty"`
	CostUSD      *float64   `json:"cost_usd,omitempty"`
	HTTPStatus   int        `json:"http_status,omitempty"`
	RateLimited  bool       `json:"rate_limited,omitempty"`
	ResultCount  int        `json:"result_count,omitempty"`
	RequestID    string     `json:"request_id,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// Trace records a single real activity attempt. Input/output contain application
// payloads, never HTTP credentials or provider headers.
type Trace struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	TurnID     string          `json:"turn_id"`
	Trigger    string          `json:"trigger"`
	TaskID     string          `json:"task_id,omitempty"`
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	ModelRole  ModelRole       `json:"model_role,omitempty"`
	Profile    string          `json:"profile,omitempty"`
	Search     *SearchTrace    `json:"search,omitempty"`
	Step       int             `json:"step"`
	CallID     string          `json:"call_id,omitempty"`
	Attempt    int             `json:"attempt"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	Historical bool            `json:"historical,omitempty"`
}
