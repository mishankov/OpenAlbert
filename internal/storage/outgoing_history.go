package storage

import (
	"encoding/json"
	"openalbert/internal/domain"
)

// A cross-chat post records only the public reply request in the destination.
// It is excluded from session analysis; no private request/context is copied.
func outgoingHistory(out domain.Outgoing) (domain.Session, domain.Message) {
	session := domain.Session{ID: "outgoing/" + out.ID, ConversationID: out.Conversation.ID, Status: "closed", CreatedAt: nowIfZero(out.EnqueuedAt)}
	args, _ := json.Marshal(map[string]string{"text": out.Text})
	message := domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: out.ID, Name: "send_message", Arguments: args}}}
	return session, message
}
