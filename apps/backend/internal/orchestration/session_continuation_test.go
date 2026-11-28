package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
)

func TestLongMultiTurnSessionStaysOpenUntilIdleTimeout(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	const turns = 35
	a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		return reply(fmt.Sprintf("Reply %d", n)), nil
	}}
	for i := 1; i < turns; i++ {
		id := fmt.Sprintf("message-%d", i)
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv, Text: id}, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, 30*time.Minute, 2)
	doneTurn := make(chan struct{}, turns)
	r.hooks = &activityHooks{
		onComplete: func(name string, _ any, err error) {
			if name == "Finish" && err == nil {
				doneTurn <- struct{}{}
			}
		},
	}

	for i := 0; i < turns; i++ {
		id := "first"
		if i > 0 {
			id = fmt.Sprintf("message-%d", i)
		}
		if err := r.SignalSession(ctx, conv.ID, Event{ID: id}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-doneTurn:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out on turn %d", i)
		}
		clk.Advance(time.Second)
	}

	active, err := repo.ActiveSessions(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("long session closed prematurely: %+v %v", active, err)
	}
	analysis, _ := repo.PendingAnalysis(ctx, 100)
	if len(analysis) != 0 {
		t.Fatal("active long session scheduled analysis prematurely")
	}

	// Advancing past IdleTimeout closes the single session once.
	waitWithFakeClock(t, r, clk)
	analysis, _ = repo.PendingAnalysis(ctx, 100)
	if len(analysis) != 1 || analysis[0].ID != active[0].ID {
		t.Fatalf("long session split or failed to close: %+v", analysis)
	}
	outgoing, _ := repo.PendingOutgoing(ctx, 100)
	if len(outgoing) != turns {
		t.Fatalf("expected %d replies, got %d", turns, len(outgoing))
	}
}

func TestSessionRestartResumesContextAndMonotonicTurnKeys(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	const session = "stable-conversation-session"
	if err := repo.StartSession(ctx, domain.Session{ID: session, ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	rawCP, _ := json.Marshal(sessionRuntimeState{SessionID: session, NextTurn: 30, UpdatedAt: time.Now().UTC()})
	if err := repo.SaveCheckpoint(ctx, sessionRuntimeKey(conv.ID), rawCP); err != nil {
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
				return domain.Message{}, fmt.Errorf("context lost across restart")
			}
			return reply("Second answer"), nil
		}
		return reply("First answer"), nil
	}}

	// First runtime processes turn 30, then shuts down before idle timeout (simulating process restart).
	clk1 := NewFakeClock(time.Now().UTC())
	r1 := newTestRuntime(a, clk1, 30*time.Minute, 2)
	turn1Done := make(chan struct{}, 1)
	r1.hooks = &activityHooks{
		onComplete: func(name string, _ any, err error) {
			if name == "Finish" && err == nil {
				turn1Done <- struct{}{}
			}
		},
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	if err := r1.SignalSession(ctx1, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	<-turn1Done
	waitUntil(t, func() bool { return clk1.ActiveTimers() > 0 })
	cancel1()
	r1.Wait()

	// Fresh runtime resumes the exact same SessionID at NextTurn=31.
	clk2 := NewFakeClock(clk1.Now().Add(time.Minute))
	r2 := newTestRuntime(a, clk2, 30*time.Minute, 2)
	if err := r2.SignalSession(ctx, conv.ID, Event{ID: "second"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r2, clk2)

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
	if len(pending) != 0 || a.Model.(*scriptedModel).count() != 4 {
		t.Fatalf("messages lost or processed twice: %+v", pending)
	}
}

func TestSessionPreservesPendingCommandsAtTurnBoundary(t *testing.T) {
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
			clk := NewFakeClock(time.Now().UTC())
			r := newTestRuntime(a, clk, time.Minute, 2)
			var sent atomic.Bool
			r.hooks = &activityHooks{
				onStart: func(name string, _ any) {
					if name == "Finish" && sent.CompareAndSwap(false, true) {
						_ = r.SignalSession(ctx, conv.ID, Event{ID: command, Stop: command == "stop", NewSession: command == "new"})
						_ = r.SignalSession(ctx, conv.ID, Event{ID: "after"})
					}
				},
			}
			if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
				t.Fatal(err)
			}
			waitWithFakeClock(t, r, clk)
			pending, _ := repo.PendingIncoming(ctx, 10)
			if len(pending) > 0 || a.Model.(*scriptedModel).count() != 4 {
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
