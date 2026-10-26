package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

func TestDeepSeekReplaysReasoningAcrossToolsAndUserTurns(t *testing.T) {
	for _, modelID := range []string{"deepseek-v4.1-flash", "deepseek/deepseek-v4-pro", "DeepSeek-Chat"} {
		for _, reasoningField := range []string{"reasoning_content", "reasoning"} {
			t.Run(modelID+"/"+reasoningField, func(t *testing.T) {
				// Synthetic protocol values only; never use real model reasoning in tests.
				const first = "fixture reasoning\nwith Unicode: проверка"
				const second = "fixture reasoning for a text-only reply"
				requests := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Messages []struct {
							Role             string  `json:"role"`
							Content          string  `json:"content"`
							ReasoningContent *string `json:"reasoning_content"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					assistant := 0
					for _, message := range request.Messages {
						if message.Role != "assistant" {
							if message.ReasoningContent != nil {
								t.Error("reasoning sent with user, system or tool message")
							}
							continue
						}
						want := []string{first, second, ""}[assistant]
						if message.ReasoningContent == nil || *message.ReasoningContent != want {
							t.Error("assistant reasoning was lost or changed during replay")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if message.Content == want && want != "" {
							t.Error("reasoning copied into user-facing content")
						}
						assistant++
					}
					if assistant != requests {
						t.Error("incomplete assistant history")
					}
					message := map[string]any{"role": "assistant", "content": ""}
					finish := "tool_calls"
					switch requests {
					case 0:
						message[reasoningField] = first
					case 1:
						message[reasoningField] = second
						message["content"] = "First answer"
						finish = "stop"
					case 2:
						// A tool-call response can omit reasoning. The next request
						// must still send an explicit empty string, not omit the field.
					case 3:
						message["content"] = "Second answer"
						finish = "stop"
					default:
						t.Error("unexpected extra request")
					}
					if finish == "tool_calls" {
						message["tool_calls"] = []any{map[string]any{"id": "lookup", "type": "function", "function": map[string]string{"name": "lookup", "arguments": "{}"}}}
					}
					requests++
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
				}))
				defer s.Close()
				model, err := NewOpenAI(OpenAIConfig{BaseURL: s.URL, Model: modelID})
				if err != nil {
					t.Fatal(err)
				}
				messages := []domain.Message{{Role: "system", Content: "Fixture"}, {Role: "user", Content: "First question"}}
				tools := []domain.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
				for step := 0; step < 4; step++ {
					completion, err := model.Complete(context.Background(), messages, tools)
					if err != nil {
						t.Fatal(err)
					}
					messages = append(messages, completion.Message)
					if len(completion.Message.ToolCalls) > 0 {
						messages = append(messages, domain.Message{Role: "tool", ToolCallID: "lookup", Content: "Fixture result"})
					} else if step == 1 {
						messages = append(messages, domain.Message{Role: "user", Content: "Second question"})
					} else if completion.Message.Content != "Second answer" {
						t.Fatal("continuation did not finish")
					}
					// Exercise the same JSON round trip as saved messages/checkpoints,
					// especially the omitted empty domain.Message.Reasoning field.
					persisted, err := json.Marshal(messages)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(persisted, &messages); err != nil {
						t.Fatal(err)
					}
				}
				if requests != 4 {
					t.Fatal("incomplete multi-turn exchange")
				}
			})
		}
	}
}
