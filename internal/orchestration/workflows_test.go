package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/internal/agent"
	"openalbert/internal/domain"
	"openalbert/internal/knowledge"
	"openalbert/internal/storage"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

type quietLogger struct{}

func (quietLogger) Debug(string, ...any) {}
func (quietLogger) Info(string, ...any)  {}
func (quietLogger) Warn(string, ...any)  {}
func (quietLogger) Error(string, ...any) {}

type scriptedModel struct {
	mu    sync.Mutex
	calls int
	run   func(int, []domain.Message) (domain.Message, error)
}

func (m *scriptedModel) Complete(ctx context.Context, msg []domain.Message, _ []domain.Tool) (domain.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	out, err := m.run(i, msg)
	return domain.Completion{Message: out, InputTokens: 10, OutputTokens: 5}, err
}
func (m *scriptedModel) count() int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls }

type fakeSearch struct{}

func (fakeSearch) Search(context.Context, string) ([]domain.SearchResult, error) {
	return []domain.SearchResult{{Title: "Release", URL: "https://example.com", Snippet: "new release"}}, nil
}
func (fakeSearch) Read(context.Context, string) (string, error) { return "Official release page", nil }

type fakeMessenger struct{}

func (fakeMessenger) Send(context.Context, domain.Conversation, string) (string, error) {
	return "message", nil
}
func (fakeMessenger) SendFile(context.Context, domain.Conversation, string, []byte) (string, error) {
	return "file", nil
}
func (fakeMessenger) Download(context.Context, string) ([]byte, error) { return nil, nil }
func newFixture(t *testing.T) (*agent.Activities, *storage.Memory, *knowledge.Store, domain.Conversation) {
	t.Helper()
	repo := storage.NewMemory()
	know, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
	if _, err = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "first", Conversation: conv, Text: "Prefer morning meetings. Watch Go releases."}, 1); err != nil {
		t.Fatal(err)
	}
	return &agent.Activities{Repo: repo, Knowledge: know, Search: fakeSearch{}, Messenger: fakeMessenger{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MinTaskInterval: time.Minute}, repo, know, conv
}
func newEnv(a *agent.Activities) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(quietLogger{})
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(15 * time.Second)
	env.RegisterActivity(a)
	return env
}
func calls(c ...domain.ToolCall) domain.Message {
	return domain.Message{Role: "assistant", ToolCalls: c}
}
func call(id, name string, args any) domain.ToolCall {
	b, _ := json.Marshal(args)
	return domain.ToolCall{ID: id, Name: name, Arguments: b}
}
func finish(text string) domain.Message { return domain.Message{Role: "assistant", Content: text} }
func reply(text string) domain.Message {
	return calls(call("reply", "send_message", map[string]string{"text": text}))
}
func requireWorkflowOK(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
}

func TestProgressToolDoesNotEndTheTurn(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	model := &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("progress", "report_progress", map[string]string{"text": "Проверяю характеристики."}), call("source", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		return reply("Готовое сравнение."), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if model.count() != 2 {
		t.Fatal("progress ended the research")
	}
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Text != "Готовое сравнение." {
		t.Fatalf("progress mixed with final replies: %+v %v", rows, err)
	}
	if err = acknowledgeProgressPart(context.Background(), repo, rows[0].ID, "sent"); err != nil {
		t.Fatal(err)
	}
	progress, err := repo.DueProgress(context.Background(), time.Now().Add(time.Hour), 10)
	if err != nil || len(progress) != 1 || progress[0].Outcome != "delivered" || progress[0].Text != "Проверяю характеристики." {
		t.Fatalf("Finish lost status/delivery state: %+v %v", progress, err)
	}
}
func twoSessions(ctx workflow.Context, in SessionInput) error {
	for _, id := range []string{"first-session", "second-session"} {
		child := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: id})
		if err := workflow.ExecuteChildWorkflow(child, SessionWorkflow, in).Get(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

func TestSessionsPreservePersonalMemoryAndIndependentTask(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "tomorrow", Conversation: conv, Text: "How is the Go release task?"}, 2); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{}
	model.run = func(n int, msg []domain.Message) (domain.Message, error) {
		switch n {
		case 0:
			return calls(call("pref", "memory_update", map[string]any{"content": "Prefers morning meetings", "kind": "preference"}), call("watch", "task_create", map[string]any{"title": "Go releases", "goal": "Watch official Go releases", "schedule_seconds": 300})), nil
		case 1:
			return calls(call("notify", "send_message", map[string]string{"text": "I will watch releases."})), nil
		case 2:
			if !strings.Contains(msg[0].Content, "morning") {
				return domain.Message{}, fmt.Errorf("personal preference absent in new session")
			}
			return calls(call("find", "task_find", map[string]string{"query": "Go releases"})), nil
		case 3:
			tasks, _ := repo.FindTasks(ctx, conv.ID, "")
			if len(tasks) != 1 {
				return domain.Message{}, fmt.Errorf("expected exactly one task")
			}
			if !strings.Contains(msg[len(msg)-1].Content, tasks[0].ID) {
				return domain.Message{}, fmt.Errorf("task_find omitted persisted task")
			}
			return calls(call("read", "task_read", map[string]string{"id": tasks[0].ID})), nil
		case 4:
			if !strings.Contains(msg[len(msg)-1].Content, `"status":"queued"`) {
				return domain.Message{}, fmt.Errorf("task status was not read")
			}
			return reply("The existing task is queued."), nil
		default:
			return domain.Message{}, fmt.Errorf("unexpected model call%d", n)
		}
	}
	a.Model = model
	env := newEnv(a)
	env.RegisterWorkflow(SessionWorkflow)
	env.RegisterDelayedCallback(func() {
		if err := env.SignalWorkflowByID("first-session", MessageSignal, Event{ID: "first"}); err != nil {
			t.Error(err)
		}
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() {
		if err := env.SignalWorkflowByID("second-session", MessageSignal, Event{ID: "tomorrow"}); err != nil {
			t.Error(err)
		}
	}, 2*time.Minute)
	env.ExecuteWorkflow(twoSessions, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 6})
	requireWorkflowOK(t, env)
	tasks, err := repo.FindTasks(ctx, conv.ID, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("task duplicated %+v %v", tasks, err)
	}
	prefs, err := k.Search(ctx, conv.ID, "morning")
	if err != nil || len(prefs) != 1 {
		t.Fatalf("preference missing %+v %v", prefs, err)
	}
	pending, err := repo.PendingAnalysis(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("sessions did not independently close %+v %v", pending, err)
	}
	if pending[0].ID == pending[1].ID {
		t.Fatal("new session reused old session ID")
	}
	inbox, _ := repo.PendingIncoming(ctx, 10)
	if len(inbox) != 0 {
		t.Fatalf("messages left pending %+v", inbox)
	}
	out, _ := repo.PendingOutgoing(ctx, 10)
	if len(out) != 2 || out[1].Text != "The existing task is queued." {
		t.Fatalf("explicit status reply not delivered %+v", out)
	}
	if model.count() != 5 {
		t.Fatalf("unexpected model calls%d", model.count())
	}
}
func TestTaskWaitsPastSessionCloseThenCompletes(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	start := time.Now().UTC()
	due := start.Add(24 * time.Hour)
	task, err := repo.CreateTask(ctx, domain.Task{ID: "reminder", ConversationID: conv.ID, Title: "Reminder", Goal: "Send tomorrow's reminder", Status: "queued", NextRunAt: due})
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.StartSession(ctx, domain.Session{ID: "old-session", ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	if err = a.CloseSession(ctx, "old-session"); err != nil {
		t.Fatal(err)
	}
	env := newEnv(a)
	env.SetStartTime(start)
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if env.Now().Before(due) {
			return domain.Message{}, fmt.Errorf("task ran before durable timer")
		}
		if n == 0 {
			return calls(call("source", "web_search", map[string]string{"query": "Go release"})), nil
		}
		if n == 1 {
			return reply("Your reminder is due."), nil
		}
		return domain.Message{}, fmt.Errorf("one-time task executed twice")
	}}
	a.Model = model
	env.ExecuteWorkflow(TaskWorkflow, TaskInput{ConversationID: conv.ID, ID: task.ID, MaxSteps: 4})
	requireWorkflowOK(t, env)
	saved, _, err := repo.Task(ctx, conv.ID, task.ID)
	if err != nil || saved.Status != "completed" {
		t.Fatalf("one-time task did not complete %+v %v", saved, err)
	}
	if model.count() != 2 {
		t.Fatalf("task repeated%d calls", model.count())
	}
	out, _ := repo.PendingOutgoing(ctx, 10)
	if len(out) != 1 || out[0].Text != "Your reminder is due." {
		t.Fatalf("reminder not delivered %+v", out)
	}
	closed, _ := repo.PendingAnalysis(ctx, 10)
	if len(closed) != 2 {
		t.Fatalf("background session did not close %+v", closed)
	}
}
func TestRepeatingTaskReschedulesAndCanCancelItself(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	start := time.Now().UTC()
	task, err := repo.CreateTask(ctx, domain.Task{ID: "repeat", ConversationID: conv.ID, Title: "Watch releases", Goal: "Check releases", Status: "queued", ScheduleSeconds: 300, NextRunAt: start})
	if err != nil {
		t.Fatal(err)
	}
	env := newEnv(a)
	env.SetStartTime(start)
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if n == 0 {
			return reply("First useful result"), nil
		}
		if n == 1 {
			if env.Now().Before(start.Add(300 * time.Second)) {
				return domain.Message{}, fmt.Errorf("repeat did not wait")
			}
			return calls(call("cancel", "task_update", map[string]string{"id": task.ID, "status": "cancelled", "result": "No longer needed"})), nil
		}
		return domain.Message{}, fmt.Errorf("cancelled task kept running")
	}}
	a.Model = model
	env.ExecuteWorkflow(TaskWorkflow, TaskInput{ConversationID: conv.ID, ID: task.ID, MaxSteps: 4})
	requireWorkflowOK(t, env)
	saved, _, _ := repo.Task(ctx, conv.ID, task.ID)
	if saved.Status != "cancelled" || saved.Result != "No longer needed" {
		t.Fatalf("cancellation lost %+v", saved)
	}
	if model.count() != 2 {
		t.Fatalf("cancelled task called model%d times", model.count())
	}
}
func TestTaskPauseOrCancellationInterruptsFutureTimer(t *testing.T) {
	for _, status := range []string{"paused", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			start := time.Now().UTC()
			task, err := repo.CreateTask(ctx, domain.Task{ID: "waiting", ConversationID: conv.ID, Title: "Later", Goal: "Wait", Status: "queued", NextRunAt: start.Add(24 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			model := &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) {
				return domain.Message{}, fmt.Errorf("stopped task executed")
			}}
			a.Model = model
			env := newEnv(a)
			env.SetStartTime(start)
			env.RegisterDelayedCallback(func() {
				current, _, e := repo.Task(ctx, conv.ID, task.ID)
				if e != nil {
					t.Error(e)
					return
				}
				current.Status = status
				if _, e = repo.UpdateTask(ctx, current); e != nil {
					t.Error(e)
				}
				env.SignalWorkflow(TaskSignal, "changed")
			}, time.Minute)
			env.ExecuteWorkflow(TaskWorkflow, TaskInput{ConversationID: conv.ID, ID: task.ID, MaxSteps: 4})
			requireWorkflowOK(t, env)
			if model.count() != 0 {
				t.Fatal("model ran despite stopped timer")
			}
			if !env.Now().Before(start.Add(24 * time.Hour)) {
				t.Fatal("stop did not interrupt timer")
			}
		})
	}
}
func TestStepBudgetStopsRunAndNotifiesUser(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		return calls(call(fmt.Sprintf("loop-%d", n), "web_search", map[string]string{"query": "same query"})), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 2})
	requireWorkflowOK(t, env)
	if model.count() != 2 {
		t.Fatalf("step budget ignored: %d", model.count())
	}
	out, _ := repo.PendingOutgoing(context.Background(), 10)
	if len(out) != 1 || !strings.Contains(out[0].Text, "Не удалось") {
		t.Fatalf("no budget failure notification %+v", out)
	}
	inbox, _ := repo.PendingIncoming(context.Background(), 10)
	if len(inbox) != 0 {
		t.Fatal("failed turn retried forever")
	}
}
func TestNewMessageSteersActiveRunBeforeNextModelCall(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "steer", Conversation: conv, Text: "Actually, only official sources"}, 2); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		if n == 1 {
			found := false
			for _, m := range msg {
				found = found || strings.Contains(m.Content, "only official sources")
			}
			if !found {
				return domain.Message{}, fmt.Errorf("steering message missing")
			}
			return reply("Using only official sources."), nil
		}
		return domain.Message{}, fmt.Errorf("steering started a duplicate turn")
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
	if !signalled || model.count() != 2 {
		t.Fatalf("steering did not join active run, calls%d", model.count())
	}
	pending, _ := repo.PendingIncoming(ctx, 10)
	if len(pending) != 0 {
		t.Fatalf("steering message left pending %+v", pending)
	}
}

func TestStopCancelsActiveTurnAndNextMessageHasValidHistory(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	for i, in := range []domain.Incoming{{ID: "stop", Conversation: conv, Text: "/stop"}, {ID: "continue", Conversation: conv, Text: "Continue with a different question"}} {
		if _, err := repo.AcceptIncoming(ctx, in, int64(i+2)); err != nil {
			t.Fatal(err)
		}
	}
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("interrupted-read", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		if n == 1 {
			pending := map[string]bool{}
			found := false
			for _, m := range msg {
				if m.Role != "tool" && len(pending) > 0 {
					return domain.Message{}, fmt.Errorf("interrupted tool call poisoned next user turn")
				}
				for _, c := range m.ToolCalls {
					pending[c.ID] = true
				}
				if m.Role == "tool" {
					delete(pending, m.ToolCallID)
				}
				found = found || strings.Contains(m.Content, "different question")
			}
			if !found {
				return domain.Message{}, fmt.Errorf("next message lost after stop")
			}
			return reply("Recovered after stopping."), nil
		}
		return domain.Message{}, fmt.Errorf("stopped run continued unexpectedly")
	}}
	a.Model = model
	env := newEnv(a)
	sentStop := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name != "ToolCall" || sentStop {
			return
		}
		var in agent.ToolInput
		if args.Get(&in) == nil && in.Call.Name == "web_read" {
			sentStop = true
			env.SignalWorkflow(MessageSignal, Event{ID: "stop", Stop: true})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "continue"}) }, 100*time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if !sentStop || model.count() != 2 {
		t.Fatalf("unexpected stop model calls%d", model.count())
	}
	inbox, _ := repo.PendingIncoming(ctx, 10)
	if len(inbox) != 0 {
		t.Fatalf("stop left messages pending %+v", inbox)
	}
	out, _ := repo.PendingOutgoing(ctx, 10)
	if len(out) != 2 || out[1].Text != "Recovered after stopping." {
		t.Fatalf("stop did not recover cleanly %+v", out)
	}
}

func TestStopBatchPreservesNewRequestAfterLastStop(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	for i, in := range []domain.Incoming{{ID: "stop-batch", Conversation: conv, Text: "/stop"}, {ID: "after-stop", Conversation: conv, Text: "New independent request after stopping"}} {
		if _, err := repo.AcceptIncoming(ctx, in, int64(i+2)); err != nil {
			t.Fatal(err)
		}
	}
	afterSeen := false
	model := &scriptedModel{run: func(_ int, msg []domain.Message) (domain.Message, error) {
		for _, m := range msg {
			if m.Role == "user" && strings.Contains(m.Content, "independent request") {
				afterSeen = true
				return reply("Handled the new request."), nil
			}
		}
		// A signal can race an already-started model call. Its result must not prevent
		// the later request from being processed, regardless of whether it was stopped.
		return reply("Earlier request response"), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(MessageSignal, Event{ID: "first"})
		env.SignalWorkflow(MessageSignal, Event{ID: "stop-batch", Stop: true})
		env.SignalWorkflow(MessageSignal, Event{ID: "after-stop"})
	}, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if !afterSeen {
		t.Fatal("request after last stop was dropped")
	}
	out, _ := repo.PendingOutgoing(ctx, 10)
	found := false
	for _, message := range out {
		found = found || message.Text == "Handled the new request."
	}
	if !found {
		t.Fatalf("new request after stop not delivered %+v", out)
	}
	pending, _ := repo.PendingIncoming(ctx, 10)
	if len(pending) != 0 {
		t.Fatalf("stop batch left pending %+v", pending)
	}
}
func TestInputDuringModelCallDiscardsStaleSideEffect(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "new-scope", Conversation: conv, Text: "Actually do not create the task. Just answer."}, 2); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("stale-create", "task_create", map[string]string{"title": "Stale task", "goal": "This should never run"})), nil
		}
		if n == 1 {
			found := false
			for _, m := range msg {
				found = found || strings.Contains(m.Content, "do not create the task")
			}
			if !found {
				return domain.Message{}, fmt.Errorf("new instruction not loaded")
			}
			return reply("I have not created a task."), nil
		}
		return domain.Message{}, fmt.Errorf("unexpected model call%d", n)
	}}
	a.Model = model
	env := newEnv(a)
	steered := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "ModelCall" && !steered {
			steered = true
			env.SignalWorkflow(MessageSignal, Event{ID: "new-scope"})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if model.count() != 2 {
		t.Fatalf("stale completion not discarded, calls%d", model.count())
	}
	tasks, err := repo.FindTasks(ctx, conv.ID, "")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("stale tool call caused side effect %+v %v", tasks, err)
	}
	pending, _ := repo.PendingIncoming(ctx, 10)
	if len(pending) != 0 {
		t.Fatalf("steering message left pending %+v", pending)
	}
}

func TestInitiativeUsesInterestsAndCreatesIndependentTaskWithoutNotificationSpam(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	if err := k.Update(ctx, domain.MemoryEntry{ID: "interest", Scope: conv.ID, Content: "Interested in Go release announcements", Kind: "preference", Source: "first"}); err != nil {
		t.Fatal(err)
	}
	env := newEnv(a)
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		switch n {
		case 0:
			return calls(call("mem", "memory_search", map[string]string{"query": "Go"})), nil
		case 1:
			if !strings.Contains(msg[len(msg)-1].Content, "Go release") {
				return domain.Message{}, fmt.Errorf("initiative did not retrieve interest")
			}
			return calls(call("tasks", "task_find", map[string]string{})), nil
		case 2:
			return calls(call("new", "task_create", map[string]any{"title": "Go release watch", "goal": "Check Go announcements and notify only on a new release", "reason": "User follows Go releases", "schedule_seconds": 300})), nil
		default:
			return finish("Checked existing tasks; nothing useful to notify. Keeping silent."), nil
		}
	}}
	a.Model = model
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 90*time.Minute)
	env.ExecuteWorkflow(InitiativeWorkflow, InitiativeInput{ConversationID: conv.ID, Interval: time.Hour, MaxSteps: 6})
	if !env.IsWorkflowCompleted() {
		t.Fatal("initiative cancellation did not finish")
	}
	tasks, err := repo.FindTasks(ctx, conv.ID, "")
	if err != nil || len(tasks) != 1 || tasks[0].Reason == "" {
		t.Fatalf("independent initiative task missing: %+v %v", tasks, err)
	}
	pending, _ := repo.PendingOutgoing(ctx, 20)
	if len(pending) != 0 {
		t.Fatalf("initiative sent noise: %+v", pending)
	}
	if model.count() != 4 {
		t.Fatalf("unexpected repeated initiative work: %d", model.count())
	}
	analyses, _ := repo.PendingAnalysis(ctx, 20)
	if len(analyses) != 1 {
		t.Fatalf("initiative session not closed: %+v", analyses)
	}
}

func TestReplyEndsTurnEvenWhenModelWouldKeepSending(t *testing.T) {
	for _, maxSteps := range []int{1, 5} {
		t.Run(fmt.Sprint(maxSteps), func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			model := &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
				return calls(call(fmt.Sprintf("reply-%d", n), "send_message", map[string]string{"text": "Hello; ready to help."})), nil
			}}
			a.Model = model
			env := newEnv(a)
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: maxSteps})
			requireWorkflowOK(t, env)
			outgoing, err := repo.PendingOutgoing(context.Background(), 100)
			if err != nil || len(outgoing) != 1 || outgoing[0].Text != "Hello; ready to help." {
				t.Fatalf("reply repeated or followed by failure notification: %+v %v", outgoing, err)
			}
			if model.count() != 1 {
				t.Fatalf("model called %d times after final reply", model.count())
			}
			pending, _ := repo.PendingIncoming(context.Background(), 100)
			if len(pending) != 0 {
				t.Fatal("answered request left pending")
			}
		})
	}
}

func finishOnlyTurn(ctx workflow.Context, in agent.RunInput) (agent.Turn, error) {
	ctx = options(ctx)
	turn, err := runTurn(ctx, in, 2)
	if err != nil {
		return turn, err
	}
	err = workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, nil)).Get(ctx, nil)
	return turn, err
}

func TestFinalTextIsSavedWithoutDelivery(t *testing.T) {
	for _, trigger := range []string{"message", "task", "initiative"} {
		t.Run(trigger, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			in := agent.RunInput{Key: "internal-conclusion", SessionID: "internal-session", ConversationID: conv.ID}
			switch trigger {
			case "message":
				in.IncomingIDs = []string{"first"}
			case "task":
				in.TaskID = "checked-task"
				if _, err := repo.CreateTask(ctx, domain.Task{ID: in.TaskID, ConversationID: conv.ID, Status: "running", Goal: "Check for updates"}); err != nil {
					t.Fatal(err)
				}
			case "initiative":
				in.Initiative = true
			}
			const conclusion = "Проверил: ничего нового. Молчу."
			a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) {
				return finish(conclusion), nil
			}}
			env := newEnv(a)
			env.ExecuteWorkflow(finishOnlyTurn, in)
			requireWorkflowOK(t, env)
			var turn agent.Turn
			if err := env.GetWorkflowResult(&turn); err != nil {
				t.Fatal(err)
			}
			if turn.Sent || turn.LastText != conclusion || turn.Stats.FinishReason != "no_reply" || turn.Stats.ToolCalls != 0 || turn.Stats.ModelCalls != 1 {
				t.Fatalf("internal conclusion treated as delivery: %+v", turn)
			}
			outgoing, err := repo.PendingOutgoing(ctx, 10)
			if err != nil || len(outgoing) != 0 {
				t.Fatalf("final text leaked to outbox: %+v %v", outgoing, err)
			}
			messages, err := repo.Messages(ctx, in.SessionID)
			if err != nil || len(messages) == 0 || messages[len(messages)-1].Content != conclusion {
				t.Fatalf("final text missing from run history: %+v %v", messages, err)
			}
			raw, ok, err := repo.LoadCheckpoint(ctx, "turn-done/"+in.Key)
			var saved agent.Turn
			if err != nil || !ok || json.Unmarshal(raw, &saved) != nil || !saved.Done || saved.Sent || saved.LastText != conclusion {
				t.Fatalf("silent completion was not checkpointed: %+v %v", saved, err)
			}
		})
	}
}

func TestFinalResponseFinishesBackgroundTurnAndPersistsTaskProgress(t *testing.T) {
	for _, initiative := range []bool{false, true} {
		t.Run(fmt.Sprintf("initiative=%t", initiative), func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			const taskID = "monitor"
			nextRun := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
			if _, err := repo.CreateTask(ctx, domain.Task{ID: taskID, ConversationID: conv.ID, Status: "running", Goal: "Check for updates"}); err != nil {
				t.Fatal(err)
			}
			model := &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
				switch n {
				case 0:
					return calls(call("progress", "task_update", map[string]string{"id": taskID, "status": "waiting", "result": "Checked: unchanged", "next_run_at": nextRun.Format(time.RFC3339)})), nil
				case 1:
					return finish("Ничего не отправляю — проверка без новостей."), nil
				default:
					t.Fatal("final response did not end the turn")
					return domain.Message{}, nil
				}
			}}
			a.Model = model
			in := agent.RunInput{Key: "quiet-check", SessionID: "quiet-session", ConversationID: conv.ID, Initiative: initiative}
			if !initiative {
				in.TaskID = taskID
			}
			env := newEnv(a)
			env.ExecuteWorkflow(finishOnlyTurn, in)
			requireWorkflowOK(t, env)
			var turn agent.Turn
			if err := env.GetWorkflowResult(&turn); err != nil {
				t.Fatal(err)
			}
			if turn.Sent || turn.Stats.FinishReason != "no_reply" || turn.Stats.ToolErrors != 0 || turn.Stats.ToolCalls != 1 || model.count() != 2 {
				t.Fatalf("silent completion failed: %+v", turn)
			}
			outgoing, err := repo.PendingOutgoing(ctx, 10)
			if err != nil || len(outgoing) != 0 {
				t.Fatalf("silent check sent a message: %+v %v", outgoing, err)
			}
			task, found, err := repo.Task(ctx, conv.ID, taskID)
			if err != nil || !found || task.Status != "waiting" || task.Result != "Checked: unchanged" || !task.NextRunAt.Equal(nextRun) {
				t.Fatalf("silent completion lost progress: %+v %v", task, err)
			}
			raw, found, err := repo.LoadCheckpoint(ctx, "turn-done/"+in.Key)
			var saved agent.Turn
			if err != nil || !found || json.Unmarshal(raw, &saved) != nil || !saved.Done || saved.Sent || saved.Stats.FinishReason != "no_reply" {
				t.Fatalf("silent completion was not checkpointed: %+v %v", saved, err)
			}
		})
	}
}

func TestBackgroundPlaceholderCanRecoverWithoutDelivery(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	model := &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n == 0 {
			return reply("placeholder"), nil
		}
		if n != 1 || messages[len(messages)-1].Role != "tool" || !agent.ToolFailed(messages[len(messages)-1].Content) {
			t.Fatal("placeholder rejection was not returned to the model")
		}
		return finish("Проверка завершена, новых событий нет."), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.ExecuteWorkflow(finishOnlyTurn, agent.RunInput{Key: "placeholder-check", SessionID: "placeholder-session", ConversationID: conv.ID, Initiative: true})
	requireWorkflowOK(t, env)
	var turn agent.Turn
	if err := env.GetWorkflowResult(&turn); err != nil {
		t.Fatal(err)
	}
	if turn.Sent || turn.Stats.FinishReason != "no_reply" || turn.Stats.ToolErrors != 1 || model.count() != 2 {
		t.Fatalf("placeholder did not recover silently: %+v", turn)
	}
	outgoing, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(outgoing) != 0 {
		t.Fatalf("placeholder reached outbox: %+v %v", outgoing, err)
	}
}

func TestExplicitDeliveryAppliesToNextTurnOfExistingWorkflow(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		return finish(fmt.Sprintf("conclusion-%d", n)), nil
	}}
	env := newEnv(a)
	// Old commands must still replay, but must not enable the fallback for new turns.
	env.OnGetVersion("explicit-message-delivery/legacy", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.ExecuteWorkflow(func(ctx workflow.Context) ([]agent.Turn, error) {
		turns := []agent.Turn{}
		for _, key := range []string{"legacy", "current"} {
			turn, err := finishOnlyTurn(ctx, agent.RunInput{Key: key, SessionID: "existing-session", ConversationID: conv.ID})
			if err != nil {
				return turns, err
			}
			turns = append(turns, turn)
		}
		return turns, nil
	})
	requireWorkflowOK(t, env)
	env.AssertExpectations(t)
	var turns []agent.Turn
	if err := env.GetWorkflowResult(&turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || !turns[0].Sent || turns[1].Sent || turns[1].Stats.FinishReason != "no_reply" {
		t.Fatalf("delivery version leaked across turns: %+v", turns)
	}
	outgoing, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(outgoing) != 1 || outgoing[0].Text != "conclusion-0" {
		t.Fatalf("new turn inherited old automatic delivery: %+v %v", outgoing, err)
	}
}

func TestBrowserActivityUsesSeparateQueueAndRetriesOnlyCapacity(t *testing.T) {
	a, _, _, conv := newFixture(t)
	a.Model = &scriptedModel{run: func(i int, _ []domain.Message) (domain.Message, error) {
		if i == 0 {
			return calls(call("open", "browser_open", map[string]string{"url": "https://example.com"})), nil
		}
		return finish("Page read."), nil
	}}
	env := newEnv(a)
	tries := 0
	env.RegisterActivityWithOptions(func(ctx context.Context, in agent.ToolInput) (string, error) {
		info := activity.GetInfo(ctx)
		if !strings.HasSuffix(info.TaskQueue, ".browser-ui") {
			t.Errorf("browser used main queue: %s", info.TaskQueue)
		}
		tries++
		if tries == 1 {
			return "", temporal.NewApplicationErrorWithOptions("full", "browser_capacity", temporal.ApplicationErrorOptions{NextRetryDelay: time.Second})
		}
		return `{"page_id":"page-one","text":"Example"}`, nil
	}, activity.RegisterOptions{Name: "BrowserToolCall", DisableAlreadyRegisteredCheck: true})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 4})
	requireWorkflowOK(t, env)
	if tries != 2 {
		t.Fatalf("capacity retry did not resume, got %d calls", tries)
	}
}

func TestGroupPostContinuesTurnForPrivateAcknowledgement(t *testing.T) {
	a, repo, _, owner := newFixture(t)
	a.OwnerUserID = owner.OwnerID
	a.AllowedGroupIDs = []int64{-20}
	group := domain.Conversation{ID: "group-post-target", ChatID: -20, ThreadID: 3, OwnerID: 8}
	if _, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "group-seed", Conversation: group, Text: "seed"}, 2); err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("group-post", "send_group_message", map[string]string{"conversation_id": group.ID, "text": "Встреча в 15:00"})), nil
		}
		return reply("Сообщение для группы поставлено в очередь."), nil
	}}
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: owner.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	outgoing, err := repo.PendingOutgoing(context.Background(), 100)
	if err != nil || len(outgoing) != 2 || outgoing[0].Conversation.ID != group.ID || outgoing[1].Conversation.ID != owner.ID {
		t.Fatalf("group post ended private turn or misrouted acknowledgement: %v %v", outgoing, err)
	}
}
