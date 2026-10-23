package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"jarvis/internal/agent"
	"jarvis/internal/domain"
	"jarvis/internal/storage"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

func TestNewSessionInterruptsTurnAndKeepsFollowingMessages(t *testing.T) {
	a, repo, knowledge, conv := newFixture(t)
	ctx := context.Background()
	if err := knowledge.Update(ctx, domain.MemoryEntry{ID: "profile", Scope: conv.ID, Kind: "profile", Content: "Lives in Moscow"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTask(ctx, domain.Task{ID: "independent", ConversationID: conv.ID, Title: "Reminder", Goal: "Keep waiting", Status: "queued", NextRunAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for i, in := range []domain.Incoming{
		{ID: "new", Conversation: conv, Text: "/new"},
		{ID: "after", Conversation: conv, Text: "Fresh question"},
		{ID: "during", Conversation: conv, Text: "And another detail"},
	} {
		if _, err := repo.AcceptIncoming(ctx, in, int64(i+2)); err != nil {
			t.Fatal(err)
		}
	}
	model := &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("interrupted", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		fresh, detail := false, false
		if !strings.Contains(messages[0].Content, "Lives in Moscow") {
			return domain.Message{}, fmt.Errorf("profile lost after new session")
		}
		for _, m := range messages {
			if strings.Contains(m.Content, "morning meetings") || m.Content == "/new" || len(m.ToolCalls) > 0 {
				return domain.Message{}, fmt.Errorf("old context leaked into new session: %+v", m)
			}
			fresh = fresh || strings.Contains(m.Content, "Fresh question")
			detail = detail || strings.Contains(m.Content, "another detail")
		}
		if !fresh || !detail {
			return domain.Message{}, fmt.Errorf("message at session boundary lost")
		}
		return reply("Fresh reply"), nil
	}}
	a.Model = model
	env := newEnv(a)
	reset, during := false, false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name == "ToolCall" && !reset {
			var in agent.ToolInput
			if args.Get(&in) == nil && in.Call.Name == "web_read" {
				reset = true
				env.SignalWorkflow(MessageSignal, Event{ID: "new", NewSession: true})
				env.SignalWorkflow(MessageSignal, Event{ID: "new", NewSession: true})
				env.SignalWorkflow(MessageSignal, Event{ID: "after"})
			}
		}
		if info.ActivityType.Name == "StartNewSession" && !during {
			during = true
			env.SignalWorkflow(MessageSignal, Event{ID: "during"})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if !reset || !during || model.count() != 2 {
		t.Fatalf("unexpected reset flow: reset=%v during=%v model calls=%d", reset, during, model.count())
	}
	sessions, _ := repo.PendingAnalysis(ctx, 10)
	if len(sessions) != 2 || sessions[0].ID == sessions[1].ID {
		t.Fatalf("expected two independent closed sessions: %+v", sessions)
	}
	old, _ := repo.Messages(ctx, sessions[0].ID)
	newMessages, _ := repo.Messages(ctx, sessions[1].ID)
	if len(old) == 0 || len(newMessages) == 0 {
		t.Fatal("session history missing")
	}
	out, _ := repo.PendingOutgoing(ctx, 10)
	if len(out) != 2 || out[0].Trigger != "new_session" || out[1].Text != "Fresh reply" || out[1].SessionID != sessions[1].ID {
		t.Fatalf("confirmation or fresh reply duplicated/lost: %+v", out)
	}
	pending, _ := repo.PendingIncoming(ctx, 10)
	if len(pending) != 0 {
		t.Fatalf("messages left pending: %+v", pending)
	}
	task, found, err := repo.Task(ctx, conv.ID, "independent")
	if err != nil || !found || task.Status != "queued" {
		t.Fatalf("independent task changed: %+v %v", task, err)
	}
}

func TestNewCommandFirstNeedsNoModelAndIsRetrySafe(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	if err := repo.MarkIncomingHandled(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "new-first", Conversation: conv, Text: "/new"}, 2); err != nil {
		t.Fatal(err)
	}
	env := newEnv(a) // A nil model makes any accidental model call fail.
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "new-first", NewSession: true}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	sessions, _ := repo.PendingAnalysis(ctx, 10)
	if len(sessions) != 1 {
		t.Fatalf("first command created an empty previous session: %+v", sessions)
	}
	raw, _, err := repo.LoadCheckpoint(ctx, "new-session/new-first")
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	if err := json.Unmarshal(raw, &previous); err != nil {
		t.Fatal(err)
	}
	if started, err := a.StartNewSession(ctx, conv.ID, previous, sessions[0].ID, []string{"new-first"}); err != nil || !started {
		t.Fatalf("activity retry failed: %v %v", started, err)
	}
	// A stale signal in a later workflow must not reset its context again.
	started, err := a.StartNewSession(ctx, conv.ID, "later-session", "unexpected-session", []string{"new-first"})
	if err != nil || started {
		t.Fatalf("stale command reset a later session: %v %v", started, err)
	}
	out, _ := repo.PendingOutgoing(ctx, 10)
	if len(out) != 1 {
		t.Fatalf("confirmation repeated: %+v", out)
	}
	// The later workflow may receive only that stale command.
	later := newEnv(a)
	later.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "later-workflow"})
	later.RegisterDelayedCallback(func() { later.SignalWorkflow(MessageSignal, Event{ID: "new-first", NewSession: true}) }, 0)
	later.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 4})
	requireWorkflowOK(t, later)
	sessions, _ = repo.PendingAnalysis(ctx, 10)
	if len(sessions) != 1 {
		t.Fatalf("stale signal created another session: %+v", sessions)
	}
}

type commandClient struct {
	client.Client
	events []Event
}

func (c *commandClient) SignalWithStartWorkflow(_ context.Context, _ string, _ string, arg any, _ client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
	c.events = append(c.events, arg.(Event))
	return nil, nil
}

func TestDispatchRecognizesOnlyExplicitNewCommands(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMemory()
	conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
	inputs := []domain.Incoming{
		{Text: "/new"}, {Text: "/new@JarvisBot", Command: "new"},
		{Text: "explain /new"}, {Text: "/newspaper"},
		{Text: "/new", Attachments: []domain.Attachment{{FileID: "doc"}}},
	}
	for i, in := range inputs {
		in.ID, in.Conversation = fmt.Sprint(i), conv
		if _, err := repo.AcceptIncoming(ctx, in, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	c := &commandClient{}
	r := Runtime{Repo: repo, Client: c, TaskQueue: "test"}
	if err := r.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != len(inputs) {
		t.Fatalf("lost events: %+v", c.events)
	}
	for i, ev := range c.events {
		if ev.NewSession != (i < 2) || ev.Stop {
			t.Fatalf("wrong command dispatch: %+v", ev)
		}
	}
}
