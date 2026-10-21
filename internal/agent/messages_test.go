package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"jarvis/internal/domain"
)

func TestBackgroundPlaceholdersNeverReachOutbox(t *testing.T) {
	for _, task := range []bool{false, true} {
		for _, text := range []string{"placeholder", " Placeholder. ", "`placeholder`", "[NO_REPLY]", "NO_MESSAGE", "NO_OP", "\u200bplaceholder\ufeff"} {
			t.Run(fmt.Sprintf("task=%t/%s", task, text), func(t *testing.T) {
				a, repo, _, conv := fixture(t)
				args, _ := json.Marshal(map[string]string{"text": text})
				in := ToolInput{RunKey: "check", Background: !task, Conversation: conv, Call: domain.ToolCall{ID: "send", Name: "send_message", Arguments: args}}
				if task {
					in.TaskID = "monitor"
				}
				// Both the original execution and a retry must stay out of the outbox.
				for range 2 {
					out, err := a.ToolCall(context.Background(), in)
					if err != nil || !ToolFailed(out) {
						t.Fatalf("placeholder accepted: %s %v", out, err)
					}
				}
				outgoing, err := repo.PendingOutgoing(context.Background(), 10)
				if err != nil || len(outgoing) != 0 {
					t.Fatalf("placeholder queued: %+v %v", outgoing, err)
				}
			})
		}
	}
}

func TestMessagesRequireVisibleTextOrArtifact(t *testing.T) {
	for _, text := range []string{"", " \t\n", "\u200b\u200d\ufeff", "\u2060\u00ad", "\x00\x08", "\u0301\ufe0f"} {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			args, _ := json.Marshal(map[string]string{"text": text})
			if out := tool(t, a, conv, "empty", "send", "send_message", string(args)); !ToolFailed(out) {
				t.Fatalf("invisible message accepted: %s", out)
			}
			if err := repo.SaveArtifact(ctx, domain.Artifact{ID: "result", ConversationID: conv.ID, Name: "result.txt", Data: []byte("result")}); err != nil {
				t.Fatal(err)
			}
			args, _ = json.Marshal(map[string]string{"text": text, "artifact_id": "result"})
			if out := tool(t, a, conv, "artifact", "send", "send_message", string(args)); ToolFailed(out) {
				t.Fatal(out)
			}
			outgoing, err := repo.PendingOutgoing(ctx, 10)
			if err != nil || len(outgoing) != 1 || outgoing[0].Text != "" || outgoing[0].ArtifactID != "result" || outgoing[0].PartCount != 1 {
				t.Fatalf("artifact acquired invisible text part: %+v %v", outgoing, err)
			}
		})
	}
}

func TestMessageValidationPreservesSubstantiveAndInteractiveText(t *testing.T) {
	for _, tc := range []struct {
		text       string
		background bool
	}{
		{text: "placeholder"}, // An explicit user request can legitimately ask for this word.
		{text: "Новое обновление: https://example.com", background: true},
		{text: "Напоминание: встреча через 10 минут.", background: true},
		{text: "В отчёте найден `placeholder`.", background: true},
		{text: "👨‍👩‍👧‍👦", background: true},
		{text: "Привет! e\u0301 你好", background: true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			args, _ := json.Marshal(map[string]string{"text": tc.text})
			out, err := a.ToolCall(context.Background(), ToolInput{RunKey: "send", Background: tc.background, Conversation: conv, Call: domain.ToolCall{ID: "send", Name: "send_message", Arguments: args}})
			if err != nil || ToolFailed(out) {
				t.Fatalf("valid message rejected: %s %v", out, err)
			}
			outgoing, err := repo.PendingOutgoing(context.Background(), 10)
			if err != nil || len(outgoing) != 1 || outgoing[0].Text != tc.text {
				t.Fatalf("valid message changed: %+v %v", outgoing, err)
			}
		})
	}
}
