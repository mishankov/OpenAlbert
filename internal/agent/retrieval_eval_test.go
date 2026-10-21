package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/integrations"
)

// Opt-in evaluation of the actual prompts and tool schemas against a configured
// model. All knowledge, tasks and outgoing messages are isolated test fixtures;
// nothing is delivered to Telegram or written to the installation's data.
func TestLiveRetrievalPrompts(t *testing.T) {
	if os.Getenv("JARVIS_LIVE_RETRIEVAL_EVAL") != "1" {
		t.Skip("set JARVIS_LIVE_RETRIEVAL_EVAL=1 and LLM_* to evaluate retrieval prompts")
	}
	model, err := integrations.NewOpenAI(integrations.OpenAIConfig{BaseURL: os.Getenv("LLM_BASE_URL"), APIKey: os.Getenv("LLM_API_KEY"), Model: os.Getenv("LLM_MODEL")})
	if err != nil {
		t.Fatal("invalid evaluation model configuration")
	}
	for _, tc := range []struct {
		name, question, kind, memory, want string
		wantMemory, wantEmpty, wantTask    bool
	}{
		{"provided profile", "Как называется мой проект?", "profile", "Денис — автор проекта Polka.", "Polka", false, false, false},
		{"specific fact", "Какую базу мы выбрали для локального индекса в Polka?", "fact", "Для локального индекса проекта Polka выбрали SQLite.", "SQLite", true, false, false},
		{"alternative spelling", "Какой источник статистики мы выбрали для команды Лейкерс?", "fact", "Для статистики команды Los Angeles Lakers выбрали ESPN.", "ESPN", true, false, false},
		{"recent entries", "Покажи последние сохранённые записи памяти.", "fact", "В прошлой поездке остановились в отеле Вега.", "Вега", true, true, false},
		{"task schedule", "Когда стоит напоминание о встрече?", "fact", "Обсуждали поездку.", "15:00", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, knowledge, conv := fixture(t)
			a.Model = model
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := knowledge.Update(ctx, domain.MemoryEntry{ID: "fixture", Scope: conv.ID, Kind: tc.kind, Content: tc.memory, Source: "fixture-session", UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if tc.wantTask {
				_, err := repo.CreateTask(ctx, domain.Task{ID: "meeting", ConversationID: conv.ID, Title: "Напоминание о встрече", Goal: "Напомнить про встречу в 15:00 МСК 23 октября 2026 года.", Status: "waiting", NextRunAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "question", Conversation: conv, Text: tc.question}, 2); err != nil {
				t.Fatal(err)
			}
			turn, err := a.Prepare(ctx, RunInput{Key: "eval", SessionID: "eval-session", ConversationID: conv.ID, IncomingIDs: []string{"question"}})
			if err != nil {
				t.Fatal(err)
			}
			memoryCalls, taskCalls, emptyCalls, memoryHits := 0, 0, 0, 0
			finished := false
			finalText := ""
			for step := 0; step < 6 && !finished; step++ {
				response, err := a.ModelCall(ctx, ModelInput{Turn: turn, Step: step})
				if err != nil {
					t.Fatalf("evaluation model call failed (%T)", err)
				}
				turn.Messages = append(turn.Messages, response.Message)
				if len(response.Message.ToolCalls) == 0 {
					finalText = response.Message.Content
					break
				}
				for _, call := range response.Message.ToolCalls {
					var args toolArgs
					if err := json.Unmarshal(call.Arguments, &args); err != nil {
						t.Fatal(err)
					}
					switch call.Name {
					case "memory_search":
						memoryCalls++
						if args.Query == "" {
							emptyCalls++
						}
						t.Logf("memory query: %q", args.Query)
					case "memory_read":
					case "conversation_history":
					case "task_find", "task_read":
						taskCalls++
					case "send_message":
						finished = true
					default:
						t.Fatalf("unexpected tool in retrieval evaluation: %s", call.Name)
					}
					output, err := a.ToolCall(ctx, ToolInput{RunKey: "eval", SessionID: "eval-session", Conversation: conv, Step: step, Call: call})
					if err != nil || ToolFailed(output) {
						t.Fatalf("evaluation tool failed: %s %v", output, err)
					}
					if call.Name == "memory_search" && output != "[]" {
						memoryHits++
					}
					turn.Messages = append(turn.Messages, domain.Message{Role: "tool", ToolCallID: call.ID, Content: output})
				}
			}
			if tc.wantMemory && (memoryCalls == 0 || memoryHits == 0) {
				t.Fatal("relevant memory was not retrieved")
			}
			if !tc.wantMemory && memoryCalls != 0 {
				t.Fatal("unnecessary memory search instead of supplied profile/task")
			}
			if tc.wantEmpty && emptyCalls != 1 {
				t.Fatal("recent-memory overview did not use one empty query")
			}
			if tc.wantTask && taskCalls == 0 {
				t.Fatal("task schedule not retrieved from tasks")
			}
			pending, err := repo.PendingOutgoing(ctx, 10)
			if len(pending) > 0 {
				finalText = pending[0].Text
			}
			if err != nil || !strings.Contains(finalText, tc.want) {
				t.Fatalf("expected grounded answer containing %q; answer=%q", tc.want, finalText)
			}
		})
	}
}
