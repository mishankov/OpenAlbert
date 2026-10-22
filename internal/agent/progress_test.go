package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jarvis/internal/domain"
)

func TestProgressToolIsNotAReplyAndRejectsInvalidOrBackgroundUpdates(t *testing.T) {
	a, repo, _, conv := fixture(t)
	for _, tc := range []struct {
		name, text                  string
		background, task, wantError bool
	}{
		{"valid", "Нашёл характеристики, проверяю цены.", false, false, false},
		{"empty", " ", false, false, true},
		{"invisible", "\u200b", false, false, true},
		{"too long", strings.Repeat("я", 601), false, false, true},
		{"initiative", "Проверяю", true, false, true},
		{"task", "Проверяю", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"text": tc.text})
			in := ToolInput{RunKey: tc.name, Conversation: conv, Background: tc.background, Call: domain.ToolCall{ID: tc.name, Name: "report_progress", Arguments: args}}
			if tc.task {
				in.TaskID = "task"
			}
			out, err := a.ToolCall(context.Background(), in)
			if err != nil || ToolFailed(out) != tc.wantError {
				t.Fatalf("unexpected progress result: %s %v", out, err)
			}
		})
	}
	rows, err := repo.PendingOutgoing(context.Background(), 100)
	if err != nil || len(rows) != 0 {
		t.Fatal("progress sent as a final reply")
	}
	for _, background := range []bool{true, false} {
		found := false
		for _, tool := range a.toolsForTurn(Turn{Conversation: conv, Input: RunInput{Initiative: background}}) {
			found = found || tool.Name == "report_progress"
		}
		if found == background {
			t.Fatal("incorrect progress tool availability")
		}
	}
}
