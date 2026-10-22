package domain

import "time"

const ProgressDelay = 30 * time.Second
const ProgressInterval = time.Minute

// Progress keeps the agent's latest public status separate from Telegram delivery.
// A finished turn waits for all of its reply parts before the status is removed.
type Progress struct {
	TurnID       string           `json:"turn_id"`
	Conversation Conversation     `json:"conversation"`
	StartedAt    time.Time        `json:"started_at"`
	Step         int              `json:"step"`
	Text         string           `json:"text"`
	Status       string           `json:"status"`
	Delivery     ProgressDelivery `json:"delivery"`
	Outcome      string           `json:"outcome,omitempty"` // Derived from the durable outbox, never written by the agent.
}

type ProgressDelivery struct {
	Version   int64     `json:"version"`
	State     string    `json:"state"` // ready, sending, done, disabled
	MessageID string    `json:"message_id"`
	LastText  string    `json:"last_text"`
	UpdatedAt time.Time `json:"updated_at"`
	RetryAt   time.Time `json:"retry_at"`
}

func (p Progress) Due(now time.Time) bool {
	if p.Delivery.State != "ready" || now.Before(p.Delivery.RetryAt) || p.Outcome == "waiting" {
		return false
	}
	return p.Outcome != "active" || (!now.Before(p.StartedAt.Add(ProgressDelay)) && !now.Before(p.Delivery.UpdatedAt.Add(ProgressInterval)))
}
