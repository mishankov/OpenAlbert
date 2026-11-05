package orchestration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
)

func TestReplyToolsChooseTelegramFormatAndDeliverOnce(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"send_message", "😀 **Привет** — [ссылка](https://example.com)"},
		{"send_message", "```go\n" + strings.Repeat("😀 **literal** <tag>\n", 450) + "```"},
		{"send_rich_message", "# Таблица\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n$x^2$\n\n<details><summary>Ещё</summary>Текст</details>"},
		{"send_rich_message", strings.Repeat("😀", 4000)},
	} {
		t.Run(tc.name+"/"+tc.source[:min(12, len(tc.source))], func(t *testing.T) {
			requests := 0
			var joined strings.Builder
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				requests++
				var p struct {
					Text      string              `json:"text"`
					Entities  []domain.TextEntity `json:"entities"`
					Rich      map[string]string   `json:"rich_message"`
					ParseMode string              `json:"parse_mode"`
				}
				if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
					t.Error(err)
				}
				if tc.name == "send_rich_message" {
					if !strings.HasSuffix(req.URL.Path, "/sendRichMessage") || p.Rich["markdown"] != tc.source || p.Text != "" || len(p.Entities) != 0 {
						t.Errorf("rich reply changed: %+v", p)
					}
				} else {
					units := len(utf16.Encode([]rune(p.Text)))
					if !strings.HasSuffix(req.URL.Path, "/sendMessage") || units > messagefmt.TextLimit || len(p.Rich) != 0 || p.ParseMode != "" || len(p.Entities) == 0 {
						t.Errorf("ordinary reply lost formatting or used rich delivery: %+v", p)
					}
					for _, entity := range p.Entities {
						if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > units {
							t.Error("entity outside chunk")
						}
					}
					joined.WriteString(p.Text)
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			})
			a := &agent.Activities{Repo: repo, Logger: r.Logger}
			args, _ := json.Marshal(map[string]string{"text": tc.source})
			in := agent.ToolInput{RunKey: "reply", Conversation: conv, Call: domain.ToolCall{ID: "send", Name: tc.name, Arguments: args}}
			for range 2 {
				result, err := a.ToolCall(context.Background(), in)
				if err != nil || agent.ToolFailed(result) {
					t.Fatalf("tool failed: %s %v", result, err)
				}
			}
			wantParts := 1
			if tc.name == "send_message" {
				wantParts = len(messagefmt.Split(messagefmt.Markdown(tc.source), messagefmt.TextLimit))
			}
			rows, err := repo.PendingOutgoing(context.Background(), 50)
			if err != nil || len(rows) != wantParts {
				t.Fatalf("wrong durable parts: %+v %v", rows, err)
			}
			for i, row := range rows {
				if row.PartIndex != i || row.PartCount != wantParts {
					t.Errorf("wrong part order: %+v", row)
				}
			}
			for range 2 {
				if err := r.DeliverOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if requests != wantParts {
				t.Fatalf("reply duplicated or missing: %d, want %d", requests, wantParts)
			}
			if tc.name == "send_message" && joined.String() != messagefmt.Markdown(tc.source).Text {
				t.Fatal("split lost rendered text")
			}
		})
	}
}

func TestRichReplyFinishesNaturallyOrAtBudget(t *testing.T) {
	for _, maxSteps := range []int{1, 4} {
		t.Run(map[int]string{1: "budget finalization", 4: "normal turn"}[maxSteps], func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			model := &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
				if n == 0 {
					return calls(call("research", "web_read", map[string]string{"url": "https://example.com"})), nil
				}
				if n != 1 {
					t.Error("model called again after rich reply")
				}
				return calls(call("rich", "send_rich_message", map[string]string{"text": "| A | B |\n|---|---|\n| 1 | 2 |"})), nil
			}}
			a.Model = model
			env := newEnv(a)
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: maxSteps})
			requireWorkflowOK(t, env)
			rows, err := repo.PendingOutgoing(context.Background(), 10)
			wantCalls := 3 // The model finishes after observing the queued reply.
			if maxSteps == 1 {
				wantCalls = 2 // Budget finalization has no further model step.
			}
			if err != nil || len(rows) != 1 || rows[0].Formatted || rows[0].Trigger == "failure" || model.count() != wantCalls {
				t.Fatalf("rich reply did not finish turn: %+v %v", rows, err)
			}
		})
	}
}

func TestOrdinaryReplyRetriesOnlyUndeliveredChunks(t *testing.T) {
	ctx := context.Background()
	attempts, delivered := 0, 0
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		attempts++
		if attempts == 2 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)
			return
		}
		delivered++
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	a := &agent.Activities{Repo: repo, Logger: r.Logger}
	args, _ := json.Marshal(map[string]string{"text": strings.Repeat("я", 9000)})
	in := agent.ToolInput{RunKey: "long", Conversation: conv, Call: domain.ToolCall{ID: "send", Name: "send_message", Arguments: args}}
	result, err := a.ToolCall(ctx, in)
	if err != nil || agent.ToolFailed(result) {
		t.Fatalf("queue: %s %v", result, err)
	}
	timeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := r.DeliverOnce(timeout); err != context.DeadlineExceeded {
		t.Fatalf("rate limit did not retain pending chunks: %v", err)
	}
	// A new runtime resumes the remaining durable chunks after a process restart.
	restarted := &Runtime{Repo: repo, Telegram: r.Telegram, Logger: r.Logger}
	for range 2 {
		if err := restarted.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if attempts != 4 || delivered != 3 {
		t.Fatalf("chunks repeated or lost: %d attempts, %d delivered", attempts, delivered)
	}
}

func TestBothReplyToolsPreserveUnknownDeliveryWithoutAutomaticRetry(t *testing.T) {
	for _, name := range []string{"send_message", "send_rich_message"} {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				attempts++
				_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
			})
			a := &agent.Activities{Repo: repo, Logger: r.Logger}
			result, err := a.ToolCall(context.Background(), agent.ToolInput{RunKey: "unknown", Conversation: conv, Call: domain.ToolCall{ID: "send", Name: name, Arguments: json.RawMessage(`{"text":"**Ответ**"}`)}})
			if err != nil || agent.ToolFailed(result) {
				t.Fatalf("queue: %s %v", result, err)
			}
			for range 2 {
				if err := r.DeliverOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if attempts != 1 || len(repo.states) != 2 || !strings.HasSuffix(repo.states[1], ":unknown") {
				t.Fatalf("unknown delivery repeated: %d attempts, %v", attempts, repo.states)
			}
		})
	}
}
