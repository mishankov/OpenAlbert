package orchestration

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
			r := newTestRuntime(a, nil, time.Second, 1)
			r.hooks = &activityHooks{
				prepare: func(context.Context, agent.RunInput, func(context.Context, agent.RunInput) (agent.Turn, error)) (agent.Turn, error) {
					return prepared, nil
				},
			}
			_, err := r.runTurn(context.Background(), in, 1)
			if size > 750000 {
				if err == nil || (domain.ErrorCode(err) != "model_context_limit" && !strings.Contains(strings.ToLower(err.Error()), "budget")) || model.count() != 0 {
					t.Fatalf("oversized ambiguous prepared turn reached provider: %v, %d calls", err, model.count())
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
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
	if m.SourceMessageID == "first" {
		m.TurnID = ""
	}
	return r.Repository.AppendMessage(ctx, session, key, m)
}

func TestSteeringAcrossDeploymentProtectsOriginalSourceAndEvidence(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Repo = legacySteeringRepo{repo}
	ctx := context.Background()
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "steer", Conversation: conv, Text: "Only official sources"}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "next", Conversation: conv, Text: "Next independent request"}, 3); err != nil {
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
		if n == 2 {
			if original || evidence || followup {
				return domain.Message{}, fmt.Errorf("next turn retained a dependent part of a deployment-spanning turn")
			}
			return reply("Next request answered."), nil
		}
		if !original || !evidence || !followup {
			return domain.Message{}, fmt.Errorf("mixed-marker steering lost current inputs: original=%v evidence=%v followup=%v", original, evidence, followup)
		}
		return reply("Official sources retained."), nil
	}}
	a.Model = model
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 4)
	var signalled, nextSignalled atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, arg any) {
			if name != "ToolCall" {
				return
			}
			if in, ok := arg.(agent.ToolInput); ok && in.Call.Name == "web_read" && signalled.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "steer"})
			}
		},
		onComplete: func(name string, _ any, err error) {
			if name == "Finish" && err == nil && nextSignalled.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "next"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if !signalled.Load() || !nextSignalled.Load() || model.count() != 5 {
		t.Fatalf("steering didn't reuse active turn: signal=%v calls=%d", signalled.Load(), model.count())
	}
}
