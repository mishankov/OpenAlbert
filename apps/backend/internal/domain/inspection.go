package domain

import (
	"encoding/json"
	"time"
)

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
