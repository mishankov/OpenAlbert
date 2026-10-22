package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/integrations"
)

// Real model, isolated knowledge and outbox: this evaluation never sends Telegram messages.
func TestLiveDeliveryPrompts(t *testing.T) {
	if os.Getenv("JARVIS_LIVE_DELIVERY_EVAL") != "1" {
		t.Skip("set JARVIS_LIVE_DELIVERY_EVAL=1 and LLM_* to evaluate delivery decisions")
	}
	model, err := integrations.NewOpenAI(integrations.OpenAIConfig{BaseURL: os.Getenv("LLM_BASE_URL"), APIKey: os.Getenv("LLM_API_KEY"), Model: os.Getenv("LLM_MODEL")})
	if err != nil {
		t.Fatal("invalid evaluation model configuration")
	}
	for _, tc := range []struct {
		name, question, mode string
		wantReply            bool
	}{
		{"direct question", "Что такое RetroArch?", "message", true},
		{"greeting", "Эй", "message", true},
		{"status question", "Есть новости?", "message", true},
		{"requested silence", "Это тест. Не отвечай на это сообщение.", "message", false},
		{"unchanged task", "", "task", false},
		{"initiative without news", "", "initiative", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			a.Model = model
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			in := RunInput{Key: "delivery-eval", SessionID: "delivery-session", ConversationID: conv.ID}
			if _, err := repo.CreateTask(ctx, domain.Task{ID: "monitor", ConversationID: conv.ID, Title: "Проверка обновлений", Goal: "Проверять обновления. Уведомлять только при новых данных. Последняя проверка: новых данных нет.", Result: "Новых данных нет.", Status: "waiting", NextRunAt: time.Now().Add(24 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			switch tc.mode {
			case "message":
				if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "question", Conversation: conv, Text: tc.question}, 2); err != nil {
					t.Fatal(err)
				}
				in.IncomingIDs = []string{"question"}
			case "task":
				in.TaskID = "monitor"
			case "initiative":
				in.Initiative = true
			}
			turn, err := a.Prepare(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			finished := false
			for step := 0; step < 6 && !finished; step++ {
				response, err := a.ModelCall(ctx, ModelInput{Turn: turn, Step: step})
				if err != nil {
					t.Fatalf("evaluation model call failed (%T)", err)
				}
				turn.Messages = append(turn.Messages, response.Message)
				if len(response.Message.ToolCalls) == 0 {
					finished = true
				}
				for _, call := range response.Message.ToolCalls {
					output, err := a.ToolCall(ctx, ToolInput{RunKey: in.Key, SessionID: in.SessionID, Conversation: conv, TaskID: in.TaskID, Background: in.Initiative, Step: step, Call: call})
					if err != nil || ToolFailed(output) {
						t.Fatalf("evaluation tool failed: %s", call.Name)
					}
					turn.Messages = append(turn.Messages, domain.Message{Role: "tool", ToolCallID: call.ID, Content: output})
					if call.Name == "send_message" {
						finished = true
					}
				}
			}
			if !finished {
				t.Fatal("evaluation exceeded step limit")
			}
			outgoing, err := repo.PendingOutgoing(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantReply && (len(outgoing) != 1 || !visibleMessageText(outgoing[0].Text)) {
				t.Fatal("expected one explicitly queued reply; internal final text is not delivery")
			}
			if !tc.wantReply && len(outgoing) != 0 {
				t.Fatal("silent completion unexpectedly queued a notification")
			}
		})
	}
}
