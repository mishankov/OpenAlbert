package orchestration

import (
	"context"
	"fmt"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"strings"
	"testing"
	"time"

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

// Simulate a request and its evidence recorded by the pre-marker deployment.
// A large historical turn stays in storage even after transport compaction.
type legacySteeringRepo struct{ domain.Repository }

func (r legacySteeringRepo) AppendMessage(ctx context.Context, session, key string, m domain.Message) error {
	if m.SourceMessageID == "first" {
		if err := r.Repository.AppendMessage(ctx, session, "older-pressure", domain.Message{Role: "user", TurnID: "older", Content: strings.Repeat("old", 260000)}); err != nil {
			return err
		}
	}
	if m.SourceMessageID != "steer" {
		m.TurnID = ""
	}
	return r.Repository.AppendMessage(ctx, session, key, m)
}

func TestSteeringAcrossDeploymentProtectsOriginalSourceAndEvidence(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Repo = legacySteeringRepo{repo}
	if _, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "steer", Conversation: conv, Text: "Only official sources"}, 2); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		original, evidence, followup := false, false, false
		for _, m := range messages {
			original = original || (m.SourceMessageID == "first" && strings.Contains(m.Content, "morning meetings"))
			evidence = evidence || (m.Role == "tool" && strings.Contains(m.Content, "Official release page"))
			followup = followup || m.SourceMessageID == "steer"
		}
		if !original || !evidence || !followup {
			return domain.Message{}, fmt.Errorf("mixed-marker steering lost current inputs: original=%v evidence=%v followup=%v", original, evidence, followup)
		}
		return reply("Official sources retained."), nil
	}}
	a.Model = model
	env := newEnv(a)
	signalled := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name != "ToolCall" || signalled {
			return
		}
		var in agent.ToolInput
		if args.Get(&in) == nil && in.Call.Name == "web_read" {
			signalled = true
			env.SignalWorkflow(MessageSignal, Event{ID: "steer"})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if !signalled || model.count() != 3 {
		t.Fatalf("steering didn't reuse active turn: signal=%v calls=%d", signalled, model.count())
	}
}
