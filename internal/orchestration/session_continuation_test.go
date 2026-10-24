package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"jarvis/internal/agent"
	"jarvis/internal/domain"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func continuedSessionInput(t *testing.T, env *testsuite.TestWorkflowEnvironment) SessionInput {
	t.Helper()
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatalf("expected ContinueAsNew, got %v", env.GetWorkflowError())
	}
	var in SessionInput
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.Continuation == nil || in.Continuation.SessionID == "" {
		t.Fatal("continuation lost its conversation session")
	}
	return in
}

func TestSessionHistoryLimitsContinueWithoutClosingConversation(t *testing.T) {
	for _, limit := range []string{"suggested", "event-count", "turn-count"} {
		t.Run(limit, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
				return reply(fmt.Sprintf("Reply %d", n)), nil
			}}
			env := newEnv(a)
			turns := 1
			switch limit {
			case "suggested":
				env.SetContinueAsNewSuggested(true)
			case "event-count":
				env.SetCurrentHistoryLength(12001)
			case "turn-count":
				turns = 30
			}
			for i := 0; i < turns; i++ {
				id := "first"
				if i > 0 {
					id = fmt.Sprintf("message-%d", i)
					if _, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: id, Conversation: conv, Text: id}, int64(i+1)); err != nil {
						t.Fatal(err)
					}
				}
				env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: id}) }, time.Duration(i)*time.Second)
			}
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: 30 * time.Minute, MaxSteps: 2})
			in := continuedSessionInput(t, env)
			if in.Continuation.NextTurn != turns || !in.Continuation.SessionStarted || len(in.Continuation.SeenIDs) != turns {
				t.Fatalf("lost turn/deduplication state: %+v", in.Continuation)
			}
			active, err := repo.ActiveSessions(context.Background())
			if err != nil || len(active) != 1 || active[0].ID != in.Continuation.SessionID {
				t.Fatalf("rollover closed the conversation: %+v %v", active, err)
			}
			analysis, _ := repo.PendingAnalysis(context.Background(), 100)
			if len(analysis) != 0 {
				t.Fatal("technical rollover scheduled conversation analysis")
			}
			if !in.Continuation.IdleDeadline.Equal(env.Now().Add(in.IdleTimeout)) {
				t.Fatal("idle deadline did not start after processing")
			}
		})
	}
}

func TestSessionContinuationPreservesContextQueuedMessagesAndTurnKeys(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	const session = "stable-conversation-session"
	if err := repo.StartSession(ctx, domain.Session{ID: session, ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "second", Conversation: conv, Text: "Continue the same topic"}, 2); err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n == 1 {
			var old, current, answer bool
			for _, m := range messages {
				old = old || strings.Contains(m.Content, "morning meetings")
				current = current || strings.Contains(m.Content, "Continue the same topic")
				for _, tool := range m.ToolCalls {
					answer = answer || strings.Contains(string(tool.Arguments), "First answer")
				}
			}
			if !old || !current || !answer {
				return domain.Message{}, fmt.Errorf("context lost across ContinueAsNew")
			}
			return reply("Second answer"), nil
		}
		return reply("First answer"), nil
	}}
	env := newEnv(a)
	env.SetContinueAsNewSuggested(true)
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "Finish" {
			// These signals arrive after steering is over, immediately before rollover.
			env.SignalWorkflow(MessageSignal, Event{ID: "first"})
			env.SignalWorkflow(MessageSignal, Event{ID: "second"})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: 30 * time.Minute, MaxSteps: 2,
		Continuation: &SessionContinuation{SessionID: session, NextTurn: 30, SessionStarted: true}})
	in := continuedSessionInput(t, env)
	if in.Continuation.NextTurn != 31 || len(in.Continuation.Pending) != 1 || in.Continuation.Pending[0].ID != "second" {
		t.Fatalf("queued events lost or repeated: %+v", in.Continuation)
	}
	resumed := newEnv(a)
	resumed.SetStartTime(env.Now())
	resumed.RegisterDelayedCallback(func() {
		resumed.SignalWorkflow(MessageSignal, Event{ID: "first"})
		resumed.SignalWorkflow(MessageSignal, Event{ID: "second"})
	}, 0)
	resumed.ExecuteWorkflow(SessionWorkflow, in)
	requireWorkflowOK(t, resumed)
	outgoing, _ := repo.PendingOutgoing(ctx, 10)
	if len(outgoing) != 2 || outgoing[0].SessionID != session || outgoing[1].SessionID != session || outgoing[0].TurnID == outgoing[1].TurnID || outgoing[1].Text != "Second answer" {
		t.Fatalf("session/turn IDs or replies lost: %+v", outgoing)
	}
	for _, next := range []int{30, 31} {
		if _, found, err := repo.LoadCheckpoint(ctx, "turn-done/"+agent.ID(session, fmt.Sprint(next))); err != nil || !found {
			t.Fatalf("missing unique turn %d: %v", next, err)
		}
	}
	analysis, _ := repo.PendingAnalysis(ctx, 10)
	if len(analysis) != 1 || analysis[0].ID != session {
		t.Fatalf("context split into multiple sessions: %+v", analysis)
	}
	pending, _ := repo.PendingIncoming(ctx, 10)
	if len(pending) != 0 || a.Model.(*scriptedModel).count() != 2 {
		t.Fatalf("messages lost or processed twice: %+v", pending)
	}
}

func TestSessionContinuationKeepsIdleDeadline(t *testing.T) {
	for _, delay := range []time.Duration{10 * time.Minute, 35 * time.Minute} {
		t.Run(delay.String(), func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) { return reply("Done"), nil }}
			env := newEnv(a)
			env.SetContinueAsNewSuggested(true)
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: 30 * time.Minute, MaxSteps: 2})
			in := continuedSessionInput(t, env)
			resumed := newEnv(a)
			resumed.SetStartTime(env.Now().Add(delay))
			resumed.ExecuteWorkflow(SessionWorkflow, in)
			requireWorkflowOK(t, resumed)
			want := in.Continuation.IdleDeadline
			if delay > in.IdleTimeout {
				want = env.Now().Add(delay)
			}
			if !resumed.Now().Equal(want) {
				t.Fatalf("rollover reset idle timeout: closed at %s, want %s", resumed.Now(), want)
			}
			analysis, _ := repo.PendingAnalysis(context.Background(), 10)
			if len(analysis) != 1 || analysis[0].ID != in.Continuation.SessionID {
				t.Fatalf("session did not close on idle timeout: %+v", analysis)
			}
		})
	}
}

func TestSessionContinuationPreservesPendingCommands(t *testing.T) {
	for _, command := range []string{"new", "stop"} {
		t.Run(command, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			for i, in := range []domain.Incoming{
				{ID: command, Conversation: conv, Text: "/" + command},
				{ID: "after", Conversation: conv, Text: "Question after command"},
			} {
				if _, err := repo.AcceptIncoming(ctx, in, int64(i+2)); err != nil {
					t.Fatal(err)
				}
			}
			a.Model = &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
				if n > 0 {
					old := false
					for _, m := range messages {
						old = old || strings.Contains(m.Content, "morning meetings")
					}
					if old != (command == "stop") {
						return domain.Message{}, fmt.Errorf("/%s has wrong context", command)
					}
				}
				return reply(fmt.Sprintf("Answer %d", n)), nil
			}}
			env := newEnv(a)
			env.SetContinueAsNewSuggested(true)
			env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
				if info.ActivityType.Name == "Finish" {
					env.SignalWorkflow(MessageSignal, Event{ID: command, Stop: command == "stop", NewSession: command == "new"})
					env.SignalWorkflow(MessageSignal, Event{ID: "after"})
				}
			})
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 2})
			in := continuedSessionInput(t, env)
			if len(in.Continuation.Pending) != 2 {
				t.Fatalf("command or following message lost: %+v", in.Continuation)
			}
			resumed := newEnv(a)
			// Durable pending work takes priority even if the worker was down
			// longer than the idle timeout during the technical handoff.
			resumed.SetStartTime(in.Continuation.IdleDeadline.Add(time.Minute))
			resumed.ExecuteWorkflow(SessionWorkflow, in)
			requireWorkflowOK(t, resumed)
			pending, _ := repo.PendingIncoming(ctx, 10)
			if len(pending) > 0 || a.Model.(*scriptedModel).count() != 2 {
				t.Fatalf("command boundary lost messages: %+v", pending)
			}
			sessions, _ := repo.PendingAnalysis(ctx, 10)
			want := 1
			if command == "new" {
				want = 2
			}
			if len(sessions) != want {
				t.Fatalf("/%s closed wrong sessions: %+v", command, sessions)
			}
		})
	}
}

func TestSessionNewCommandRolloverKeepsNewContext(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	for i, in := range []domain.Incoming{
		{ID: "new", Conversation: conv, Text: "/new"},
		{ID: "after", Conversation: conv, Text: "Fresh question"},
	} {
		if _, err := repo.AcceptIncoming(ctx, in, int64(i+2)); err != nil {
			t.Fatal(err)
		}
	}
	a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) { return reply("Fresh answer"), nil }}
	env := newEnv(a)
	env.SetContinueAsNewSuggested(true)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(MessageSignal, Event{ID: "first"})
		env.SignalWorkflow(MessageSignal, Event{ID: "new", NewSession: true})
		env.SignalWorkflow(MessageSignal, Event{ID: "after"})
	}, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 2})
	in := continuedSessionInput(t, env)
	active, _ := repo.ActiveSessions(ctx)
	if len(active) != 1 || active[0].ID != in.Continuation.SessionID || !in.Continuation.SessionStarted || len(in.Continuation.Pending) != 1 {
		t.Fatalf("new session closed during technical rollover: %+v %+v", active, in.Continuation)
	}
	resumed := newEnv(a)
	resumed.ExecuteWorkflow(SessionWorkflow, in)
	requireWorkflowOK(t, resumed)
	outgoing, _ := repo.PendingOutgoing(ctx, 10)
	if len(outgoing) != 2 || outgoing[0].Trigger != "new_session" || outgoing[1].Text != "Fresh answer" || outgoing[1].SessionID != in.Continuation.SessionID {
		t.Fatalf("new session confirmation/reply lost or repeated: %+v", outgoing)
	}
}

func TestSessionLegacyHistoryKeepsOriginalClosingSequence(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) { return reply("Done"), nil }}
	env := newEnv(a)
	env.SetContinueAsNewSuggested(true)
	env.OnGetVersion("session-continuation-v1", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 2})
	requireWorkflowOK(t, env)
	env.AssertExpectations(t)
	sessions, _ := repo.PendingAnalysis(context.Background(), 10)
	if len(sessions) != 1 {
		t.Fatalf("legacy history no longer closes: %+v", sessions)
	}
}
