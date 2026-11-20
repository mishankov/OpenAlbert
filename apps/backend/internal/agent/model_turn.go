package agent

import "openalbert/apps/backend/internal/domain"

// Recover the start of an active turn whose original messages may predate
// message-level turn markers. Steering preparation receives every current input,
// so a newer follow-up marker cannot hide the original request and its evidence.
func markCurrentHistory(messages []domain.Message, in RunInput) []domain.Message {
	if len(in.IncomingIDs) == 0 {
		return messages
	}
	missing := make(map[string]bool, len(in.IncomingIDs))
	for _, id := range in.IncomingIDs {
		missing[id] = true
	}
	start := len(messages)
	for i, m := range messages {
		if missing[m.SourceMessageID] {
			delete(missing, m.SourceMessageID)
			start = min(start, i)
		}
	}
	if len(missing) > 0 {
		// A missing original source could precede every identifiable follow-up.
		// Keep the ambiguous prefix rather than guess from the latest marker.
		start = 0
	}
	out := append([]domain.Message(nil), messages...)
	for i := start; i < len(out); i++ {
		out[i].TurnID = in.Key
	}
	return out
}
