package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/knowledge"
	"openalbert/apps/backend/internal/storage"
)

type scriptedModel struct {
	mu    sync.Mutex
	calls int
	steps int
	// Most fixtures describe one delivered answer. Finish it naturally unless
	// the test explicitly scripts work after a reply-tool result.
	continueAfterReply bool
	run                func(int, []domain.Message) (domain.Message, error)
}

func (m *scriptedModel) Complete(ctx context.Context, msg []domain.Message, _ []domain.Tool) (domain.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if !m.continueAfterReply && len(msg) > 0 {
		last := msg[len(msg)-1]
		if last.Role == "tool" && !agent.ToolFailed(last.Content) {
			for i := len(msg) - 2; i >= 0; i-- {
				if msg[i].Role != "assistant" {
					continue
				}
				for _, call := range msg[i].ToolCalls {
					if call.ID == last.ToolCallID && domain.IsReplyTool(call.Name) {
						return domain.Completion{Message: finish(""), InputTokens: 10, OutputTokens: 5}, nil
					}
				}
				break
			}
		}
	}
	i := m.steps
	m.steps++
	out, err := m.run(i, msg)
	return domain.Completion{Message: out, InputTokens: 10, OutputTokens: 5}, err
}
func (m *scriptedModel) count() int { m.mu.Lock(); defer m.mu.Unlock(); return m.calls }

type fakeSearch struct{}

func (fakeSearch) Search(context.Context, domain.SearchQuery) (domain.SearchResponse, error) {
	return domain.SearchResponse{Provider: "yandex", Results: []domain.SearchResult{{Title: "Release", URL: "https://example.com", Snippet: "new release"}}}, nil
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

func newTestRuntime(a *agent.Activities, clk *FakeClock, idleTimeout time.Duration, maxSteps int) *Runtime {
	if clk == nil {
		clk = NewFakeClock(time.Now().UTC())
	}
	return &Runtime{
		Repo:        a.Repo,
		Activities:  a,
		IdleTimeout: idleTimeout,
		MaxSteps:    maxSteps,
		Clock:       clk,
		Logger:      a.Logger,
	}
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

func finishOnlyTurn(ctx context.Context, r *Runtime, in agent.RunInput) (agent.Turn, error) {
	turn, err := r.runTurn(ctx, in, 2)
	if err != nil {
		return turn, err
	}
	err = r.actFinish(ctx, finishInput(turn, nil))
	return turn, err
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 4)
	if err := r.SignalSession(context.Background(), conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if model.count() != 3 {
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 6)
	for _, incomingID := range []string{"first", "tomorrow"} {
		if err := r.SignalSession(ctx, conv.ID, Event{ID: incomingID}); err != nil {
			t.Fatal(err)
		}
		waitWithFakeClock(t, r, clk)
	}
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
	if model.count() != 7 {
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
	clk := NewFakeClock(start)
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if clk.Now().Before(due) {
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
	r := newTestRuntime(a, clk, time.Minute, 4)

	schedCtx, stopSched := context.WithCancel(ctx)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.runTaskScheduler(schedCtx)
	}()

	waitUntil(t, func() bool { return clk.ActiveTimers() > 0 })
	if model.count() != 0 {
		t.Fatal("task ran early")
	}
	clk.Set(due)
	_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)
	waitUntil(t, func() bool {
		saved, _, _ := repo.Task(ctx, conv.ID, task.ID)
		return saved.Status == "completed"
	})
	stopSched()
	r.Wait()

	saved, _, err := repo.Task(ctx, conv.ID, task.ID)
	if err != nil || saved.Status != "completed" {
		t.Fatalf("one-time task did not complete %+v %v", saved, err)
	}
	if model.count() != 3 {
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
	clk := NewFakeClock(start)
	model := &scriptedModel{run: func(n int, msg []domain.Message) (domain.Message, error) {
		if n == 0 {
			return reply("First useful result"), nil
		}
		if n == 1 {
			if clk.Now().Before(start.Add(300 * time.Second)) {
				return domain.Message{}, fmt.Errorf("repeat did not wait")
			}
			return calls(call("cancel", "task_update", map[string]string{"id": task.ID, "status": "cancelled", "result": "No longer needed"})), nil
		}
		return domain.Message{}, fmt.Errorf("cancelled task kept running")
	}}
	a.Model = model
	advanced := make(chan struct{}, 4)
	r := newTestRuntime(a, clk, time.Minute, 4)
	r.hooks = &activityHooks{
		onComplete: func(name string, _ any, err error) {
			if name == "AdvanceTask" && err == nil {
				advanced <- struct{}{}
			}
		},
	}

	schedCtx, stopSched := context.WithCancel(ctx)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.runTaskScheduler(schedCtx)
	}()

	select {
	case <-advanced:
	case <-time.After(5 * time.Second):
		stopSched()
		t.Fatal("timed out waiting for first repeat")
	}
	waitUntil(t, func() bool {
		r.mu.Lock()
		active := r.activeTasks[task.ID]
		r.mu.Unlock()
		return !active
	})
	clk.Set(start.Add(300 * time.Second))
	_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)

	select {
	case <-advanced:
	case <-time.After(5 * time.Second):
		stopSched()
		t.Fatal("timed out waiting for second repeat")
	}
	waitUntil(t, func() bool {
		r.mu.Lock()
		active := r.activeTasks[task.ID]
		r.mu.Unlock()
		return !active
	})
	clk.Advance(10 * time.Minute)
	_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)
	stopSched()
	r.Wait()

	saved, _, _ := repo.Task(ctx, conv.ID, task.ID)
	if saved.Status != "cancelled" || saved.Result != "No longer needed" {
		t.Fatalf("cancellation lost %+v", saved)
	}
	if model.count() != 3 {
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
			clk := NewFakeClock(start)
			r := newTestRuntime(a, clk, time.Minute, 4)

			schedCtx, stopSched := context.WithCancel(ctx)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.runTaskScheduler(schedCtx)
			}()

			waitUntil(t, func() bool { return clk.ActiveTimers() > 0 })
			clk.Advance(time.Minute)
			current, _, e := repo.Task(ctx, conv.ID, task.ID)
			if e != nil {
				t.Fatal(e)
			}
			current.Status = status
			if _, e = repo.UpdateTask(ctx, current); e != nil {
				t.Fatal(e)
			}
			_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)
			clk.Set(start.Add(25 * time.Hour))
			_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)
			time.Sleep(10 * time.Millisecond)
			stopSched()
			r.Wait()

			if model.count() != 0 {
				t.Fatal("model ran despite stopped timer")
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 2)
	if err := r.SignalSession(context.Background(), conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if model.count() != 3 {
		t.Fatalf("research budget plus one finalization call ignored: %d", model.count())
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 4)
	var signalled atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, arg any) {
			if name != "ToolCall" {
				return
			}
			if in, ok := arg.(agent.ToolInput); ok && in.Call.Name == "web_read" && signalled.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "steer"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if !signalled.Load() || model.count() != 3 {
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 4)
	var sentStop, sentContinue atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, arg any) {
			if name != "ToolCall" {
				return
			}
			if in, ok := arg.(agent.ToolInput); ok && in.Call.Name == "web_read" && sentStop.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "stop", Stop: true})
			}
		},
		onComplete: func(name string, _ any, err error) {
			if name == "AcknowledgeStop" && err == nil && sentContinue.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "continue"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if !sentStop.Load() || model.count() != 3 {
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
	var afterSeen atomic.Bool
	model := &scriptedModel{run: func(_ int, msg []domain.Message) (domain.Message, error) {
		for _, m := range msg {
			if m.Role == "user" && strings.Contains(m.Content, "independent request") {
				afterSeen.Store(true)
				return reply("Handled the new request."), nil
			}
		}
		// A signal can race an already-started model call. Its result must not prevent
		// the later request from being processed, regardless of whether it was stopped.
		return reply("Earlier request response"), nil
	}}
	a.Model = model
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 4)
	_ = r.SignalSession(ctx, conv.ID, Event{ID: "first"})
	_ = r.SignalSession(ctx, conv.ID, Event{ID: "stop-batch", Stop: true})
	_ = r.SignalSession(ctx, conv.ID, Event{ID: "after-stop"})
	waitWithFakeClock(t, r, clk)
	if !afterSeen.Load() {
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 4)
	var steered atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, _ any) {
			if name == "ModelCall" && steered.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "new-scope"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if model.count() != 3 {
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
	if err := repo.MarkIncomingHandled(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "interest", Scope: conv.ID, Content: "Interested in Go release announcements", Kind: "preference", Source: "first"}); err != nil {
		t.Fatal(err)
	}
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
	clk := NewFakeClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	r := newTestRuntime(a, clk, time.Minute, 6)
	r.InitiativeInterval = time.Hour

	// First dispatch seeds the baseline timestamp without immediately firing.
	if err := r.dispatchInitiative(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if model.count() != 0 {
		t.Fatalf("initiative fired before interval elapsed: %d", model.count())
	}

	// After 1 hour, exactly one initiative run executes.
	clk.Advance(time.Hour)
	if err := r.dispatchInitiative(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	// At 90 minutes (30 minutes after the run), no second run starts.
	clk.Advance(30 * time.Minute)
	if err := r.dispatchInitiative(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()

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
			r := newTestRuntime(a, nil, time.Second, 2)
			turn, err := finishOnlyTurn(ctx, r, in)
			if err != nil {
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

func TestBackgroundTaskPersistsProgressAndInitiativePreservesIt(t *testing.T) {
	for _, initiative := range []bool{false, true} {
		t.Run(fmt.Sprintf("initiative=%t", initiative), func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			const taskID = "monitor"
			nextRun := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
			if _, err := repo.CreateTask(ctx, domain.Task{ID: taskID, ConversationID: conv.ID, Status: "running", Goal: "Check for updates", Result: "Last useful result"}); err != nil {
				t.Fatal(err)
			}
			model := &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
				switch n {
				case 0:
					return calls(call("progress", "task_update", map[string]string{"id": taskID, "status": "waiting", "result": "Checked: unchanged", "next_run_at": nextRun.Format(time.RFC3339)})), nil
				case 1:
					if agent.ToolFailed(messages[len(messages)-1].Content) != initiative {
						t.Fatal("unexpected task update authorization")
					}
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
			r := newTestRuntime(a, nil, time.Second, 2)
			turn, err := finishOnlyTurn(ctx, r, in)
			if err != nil {
				t.Fatal(err)
			}
			wantErrors := 0
			if initiative {
				wantErrors = 1
			}
			if turn.Sent || turn.Stats.FinishReason != "no_reply" || turn.Stats.ToolErrors != wantErrors || turn.Stats.ToolCalls != 1 || model.count() != 2 {
				t.Fatalf("silent completion failed: %+v", turn)
			}
			outgoing, err := repo.PendingOutgoing(ctx, 10)
			if err != nil || len(outgoing) != 0 {
				t.Fatalf("silent check sent a message: %+v %v", outgoing, err)
			}
			task, found, err := repo.Task(ctx, conv.ID, taskID)
			if err != nil || !found {
				t.Fatalf("task missing: %v", err)
			}
			if initiative {
				if task.Status != "running" || task.Result != "Last useful result" || !task.NextRunAt.IsZero() {
					t.Fatalf("initiative overwrote task progress: %+v", task)
				}
			} else if task.Status != "waiting" || task.Result != "Checked: unchanged" || !task.NextRunAt.Equal(nextRun) {
				t.Fatalf("silent completion lost progress: %+v %v", task, err)
			}
			raw, found, err := repo.LoadCheckpoint(ctx, "turn-done/"+in.Key)
			var saved agent.Turn
			if err != nil || !found || json.Unmarshal(raw, &saved) != nil || !saved.Done || saved.Sent || saved.Stats.FinishReason != "no_reply" || saved.LastText != turn.LastText || saved.LastText == "" {
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
	r := newTestRuntime(a, nil, time.Second, 2)
	turn, err := finishOnlyTurn(context.Background(), r, agent.RunInput{Key: "placeholder-check", SessionID: "placeholder-session", ConversationID: conv.ID, Initiative: true})
	if err != nil {
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

func TestBrowserActivityUsesSeparateLaneAndRetriesCapacity(t *testing.T) {
	a, _, _, conv := newFixture(t)
	a.Model = &scriptedModel{run: func(i int, _ []domain.Message) (domain.Message, error) {
		if i == 0 {
			return calls(call("open", "browser_open", map[string]string{"url": "https://example.com"})), nil
		}
		return finish("Page read."), nil
	}}
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 4)
	var tries atomic.Int32
	r.hooks = &activityHooks{
		browserCall: func(_ context.Context, lane string, _ agent.ToolInput, _ func(context.Context, agent.ToolInput) (string, error)) (string, error) {
			if lane != ".browser-ui" {
				t.Errorf("browser used wrong lane: %s", lane)
			}
			if tries.Add(1) == 1 {
				return "", domain.NewRetryableError("full", "browser_capacity", time.Second, nil)
			}
			return `{"page_id":"page-one","text":"Example"}`, nil
		},
	}
	if err := r.SignalSession(context.Background(), conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if tries.Load() != 2 {
		t.Fatalf("capacity retry did not resume, got %d calls", tries.Load())
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Minute, 4)
	if err := r.SignalSession(context.Background(), owner.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	outgoing, err := repo.PendingOutgoing(context.Background(), 100)
	if err != nil || len(outgoing) != 2 || outgoing[0].Conversation.ID != group.ID || outgoing[1].Conversation.ID != owner.ID {
		t.Fatalf("group post ended private turn or misrouted acknowledgement: %v %v", outgoing, err)
	}
}
