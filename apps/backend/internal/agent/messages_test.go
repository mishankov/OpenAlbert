package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
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

func TestAttachmentCaptionPlanningAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		combined   bool
	}{
		{"formatted", "😀 **RG DS** — [характеристики](https://example.com)", true},
		{"caption boundary", strings.Repeat("я", 1024), true},
		{"rendered boundary", "**" + strings.Repeat("я", 1024) + "**", true},
		{"long description", strings.Repeat("я", 1025), false},
		{"emoji boundary", strings.Repeat("😀", 512), true},
		{"long emoji description", strings.Repeat("😀", 513), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			if err := repo.SaveArtifact(ctx, domain.Artifact{ID: "photo", ConversationID: conv.ID, Name: "photo.png", MIME: "image/png", Data: []byte("fixture")}); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"text": tc.text, "artifact_id": "photo"})
			for range 2 {
				if output := tool(t, a, conv, "caption", "send", "send_message", string(args)); ToolFailed(output) {
					t.Fatal(output)
				}
			}
			rows, err := repo.PendingOutgoing(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if tc.combined {
				caption := messagefmt.Markdown(tc.text)
				if len(rows) != 1 || rows[0].ArtifactID != "photo" || rows[0].Text != caption.Text || !rows[0].Formatted || rows[0].PartCount != 1 || rows[0].PartIndex != 0 {
					t.Fatalf("caption/image were split or duplicated: %+v", rows)
				}
				if len(rows[0].Entities) != len(caption.Entities) {
					t.Fatal("caption formatting lost")
				}
			} else if len(rows) != 2 || rows[0].Text != tc.text || rows[0].ArtifactID != "" || rows[1].ArtifactID != "photo" || rows[1].Text != "" || rows[0].PartCount != 2 || rows[1].PartCount != 2 {
				t.Fatalf("long description was truncated, lost or duplicated: %+v", rows)
			}
		})
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
