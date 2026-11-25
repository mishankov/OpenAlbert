package domain

import "time"

// MemoryConsolidationOperation records one model-proposed consolidation action
// and its verified outcome after Go-enforced safety and concurrency guards.
type MemoryConsolidationOperation struct {
	Phase      string   `json:"phase"`
	Op         string   `json:"op"`
	IDs        []string `json:"ids"`
	ResultID   string   `json:"result_id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Status     string   `json:"status"`
	SkipReason string   `json:"skip_reason,omitempty"`
}

// MemoryConsolidationReport is the durable audit receipt persisted in Postgres
// for each autonomous memory consolidation run.
type MemoryConsolidationReport struct {
	ID                 string                         `json:"id"`
	Scope              string                         `json:"scope"`
	Trigger            string                         `json:"trigger"`
	StartedAt          time.Time                      `json:"started_at"`
	CompletedAt        time.Time                      `json:"completed_at"`
	Status             string                         `json:"status"`
	BeforeCount        int                            `json:"before_count"`
	AfterCount         int                            `json:"after_count"`
	ProfileCharsBefore int                            `json:"profile_chars_before"`
	ProfileCharsAfter  int                            `json:"profile_chars_after"`
	ProfileCharsDelta  int                            `json:"profile_chars_delta"`
	BatchesProcessed   int                            `json:"batches_processed"`
	HasMore            bool                           `json:"has_more,omitempty"`
	Operations         []MemoryConsolidationOperation `json:"operations"`
	Error              string                         `json:"error,omitempty"`
}

// MemoryMutation describes one atomic compare-archive-mutate consolidation
// operation verified and applied under the knowledge store lock.
type MemoryMutation struct {
	Scope           string
	RunID           string
	Now             time.Time
	Expected        []MemoryEntry
	Update          *MemoryEntry
	DeleteIDs       []string
	MaxProfileChars int
}
