package agent

import "openalbert/apps/backend/internal/domain"

// Recover the current turn from explicit source identity rather than infer it
// from a follow-up. Old queued activities may contain only steering inputs.
func markCurrentHistory(messages []domain.Message, in RunInput) []domain.Message {
	if len(in.IncomingIDs) == 0 {
		return messages
	}
	// Missing provenance or source metadata makes the prefix ambiguous. Protect
	// all of it; transport fitting will report overflow if it cannot be carried.
	start := 0
	if in.OriginalIncomingID != "" {
		for i, m := range messages {
			if m.SourceMessageID == in.OriginalIncomingID {
				start = i
				break
			}
		}
	}
	out := append([]domain.Message(nil), messages...)
	for i := start; i < len(out); i++ {
		out[i].TurnID = in.Key
	}
	return out
}
