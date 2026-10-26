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
	Removing  bool      `json:"removing,omitempty"` // Finish an idempotent Telegram cleanup even if the route changes back.
	Version   int64     `json:"version"`
	State     string    `json:"state"` // ready, sending, suspended (desktop), done, disabled
	MessageID string    `json:"message_id"`
	LastText  string    `json:"last_text"`
	UpdatedAt time.Time `json:"updated_at"`
	RetryAt   time.Time `json:"retry_at"`
}

func (p Progress) Due(now time.Time) bool {
	activeRoute := p.Conversation.DeliveryChannel != "desktop"
	if p.Delivery.State != "ready" && !(p.Delivery.State == "suspended" && activeRoute) {
		return false
	}
	if now.Before(p.Delivery.RetryAt) {
		return false
	}
	// Route changes clean up an existing Telegram status immediately, even while
	// the answer is waiting in the outbox. No new Telegram status is created here.
	if !activeRoute || p.Delivery.Removing {
		return true
	}
	if p.Outcome == "waiting" {
		return false
	}
	return p.Outcome != "active" || (!now.Before(p.StartedAt.Add(ProgressDelay)) && !now.Before(p.Delivery.UpdatedAt.Add(ProgressInterval)))
}

// ConversationActivity is a public projection of the durable inbox and active
// progress. It never contains model messages, tool calls or reasoning.
type ConversationActivity struct {
	State string `json:"state"` // idle, queued, processing
	Text  string `json:"text,omitempty"`
}
