package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
)

func TestWorkflowCompactionPreservesUnmarkedPreparedTurn(t *testing.T) {
	for _, size := range []int{40000, 800000} {
		t.Run(map[bool]string{true: "overflow", false: "fits"}[size > 750000], func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			evidence := strings.Repeat("x", size)
			model := &scriptedModel{run: func(_ int, messages []domain.Message) (domain.Message, error) {
				if len(messages) != 5 || messages[1].Content != "Original request" || messages[3].Content != evidence {
					t.Error("unmarked prepared request or evidence was removed/clipped")
				}
				return finish(""), nil
			}}
			a.Model = model
			in := agent.RunInput{Key: "old-prepared", SessionID: "old-session", ConversationID: conv.ID}
			if err := repo.StartSession(context.Background(), domain.Session{ID: in.SessionID, ConversationID: conv.ID}); err != nil {
				t.Fatal(err)
			}
			prepared := agent.Turn{Input: in, Conversation: conv, Messages: []domain.Message{
				{Role: "system", Content: "Rules"},
				{Role: "user", Content: "Original request"},
				{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "read", Name: "web_read", Arguments: []byte(`{}`)}}},
				{Role: "tool", ToolCallID: "read", Content: evidence},
				{Role: "user", Content: "Artifact view or finalization instruction"},
			}}
			env := newEnv(a)
			env.OnActivity("Prepare", mock.Anything, mock.Anything).Return(prepared, nil).Once()
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				_, err := runTurn(options(ctx), in, 1)
				return err
			})
			if size > 750000 {
				err := env.GetWorkflowError()
				if err == nil || !strings.Contains(err.Error(), "Temporal payload budget") || model.count() != 0 {
					t.Fatalf("oversized ambiguous prepared turn reached provider: %v, %d calls", err, model.count())
				}
			} else {
				requireWorkflowOK(t, env)
				if model.count() != 1 {
					t.Fatalf("expected preserved request to reach provider once, got %d", model.count())
				}
			}
		})
	}
}
