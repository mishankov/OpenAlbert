package agent

import (
	"openalbert/apps/backend/internal/domain"
	"testing"
)

func TestMissingOriginalSourceProtectsAmbiguousPrefix(t *testing.T) {
	messages := []domain.Message{{Role: "user", Content: "Original without source metadata"}, {Role: "tool", Content: "Evidence"}, {Role: "user", SourceMessageID: "followup", TurnID: "current", Content: "Follow-up"}}
	marked := markCurrentHistory(messages, RunInput{Key: "current", OriginalIncomingID: "original", IncomingIDs: []string{"original", "followup"}})
	if start := domain.ModelTurnStart(marked, "current"); start != 0 {
		t.Fatalf("missing original source allowed prefix eviction: %d", start)
	}
	if messages[0].TurnID != "" {
		t.Fatal("marking current context mutated stored history")
	}
}
