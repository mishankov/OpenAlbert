package orchestration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

// FakeClock is a thread-safe virtual clock for deterministic orchestration tests.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	changed chan struct{}
}

type fakeTimer struct {
	clk      *FakeClock
	deadline time.Time
	ch       chan time.Time
	active   bool
}

func NewFakeClock(start time.Time) *FakeClock {
	if start.IsZero() {
		start = time.Now().UTC()
	}
	return &FakeClock{
		now:     start,
		changed: make(chan struct{}),
	}
}

func (c *FakeClock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *FakeClock) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{
		clk: c,
		ch:  make(chan time.Time, 1),
	}
	if d <= 0 {
		t.ch <- c.now
	} else {
		t.deadline = c.now.Add(d)
		t.active = true
		c.timers = append(c.timers, t)
	}
	c.notifyLocked()
	return t
}

func (c *FakeClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
	c.fireLocked()
	c.notifyLocked()
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fireLocked()
	c.notifyLocked()
}

func (c *FakeClock) fireLocked() {
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.active {
			continue
		}
		if !t.deadline.After(c.now) {
			t.active = false
			select {
			case t.ch <- c.now:
			default:
			}
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

func (c *FakeClock) ActiveTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}

func (c *FakeClock) HasTimerAt(deadline time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if t.active && t.deadline.Equal(deadline) {
			return true
		}
	}
	return false
}

func (c *FakeClock) NextDeadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var earliest time.Time
	found := false
	for _, t := range c.timers {
		if !t.active {
			continue
		}
		if !found || t.deadline.Before(earliest) {
			earliest = t.deadline
			found = true
		}
	}
	return earliest, found
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	c := t.clk
	c.mu.Lock()
	defer c.mu.Unlock()
	was := t.active
	t.active = false
	if was {
		kept := c.timers[:0]
		for _, item := range c.timers {
			if item != t {
				kept = append(kept, item)
			}
		}
		c.timers = kept
		c.notifyLocked()
	}
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	c := t.clk
	c.mu.Lock()
	defer c.mu.Unlock()
	was := t.active
	select {
	case <-t.ch:
	default:
	}
	if d <= 0 {
		t.active = false
		if was {
			kept := c.timers[:0]
			for _, item := range c.timers {
				if item != t {
					kept = append(kept, item)
				}
			}
			c.timers = kept
		}
		t.ch <- c.now
	} else {
		t.deadline = c.now.Add(d)
		if !was {
			c.timers = append(c.timers, t)
		}
		t.active = true
	}
	c.notifyLocked()
	return was
}

// waitWithFakeClock advances clk to each registered deadline until r.Wait()
// completes, enabling virtual-time tests to run deterministically in ms.
func waitWithFakeClock(t *testing.T, r *Runtime, clk *FakeClock) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		r.Wait()
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-done:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for runtime to settle")
		}
		if next, ok := clk.NextDeadline(); ok {
			clk.Set(next)
			time.Sleep(time.Millisecond)
			continue
		}
		select {
		case <-done:
			return
		case <-clk.Changed():
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func TestFakeClockTimersAdvanceStopAndReset(t *testing.T) {
	start := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	clk := NewFakeClock(start)

	t1 := clk.NewTimer(5 * time.Second)
	t2 := clk.NewTimer(10 * time.Second)
	if clk.ActiveTimers() != 2 {
		t.Fatalf("active timers = %d, want 2", clk.ActiveTimers())
	}
	if !t2.Stop() || t2.Stop() {
		t.Fatal("Stop return value incorrect")
	}
	if next, ok := clk.NextDeadline(); !ok || !next.Equal(start.Add(5*time.Second)) {
		t.Fatalf("next deadline = %v %v", next, ok)
	}
	clk.Advance(5 * time.Second)
	select {
	case fired := <-t1.C():
		if !fired.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("unexpected fire time: %v", fired)
		}
	default:
		t.Fatal("timer did not fire")
	}
	t1.Reset(3 * time.Second)
	clk.Advance(3 * time.Second)
	select {
	case <-t1.C():
	default:
		t.Fatal("reset timer did not fire")
	}
}

func TestRuntimeStartRecoversRunningTasksReapsSessionsAndPrunesLogs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, repo, _, conv := newFixture(t)
	if err := repo.MarkIncomingHandled(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clk := NewFakeClock(now)

	// Crashed task left in "running" must be reset to "waiting" keeping NextRunAt & Result.
	// Keep due strictly beyond the 1-hour log-prune advance below.
	due := now.Add(2 * time.Hour)
	if _, err := repo.CreateTask(ctx, domain.Task{
		ID: "crashed", ConversationID: conv.ID, Status: "running",
		Goal: "Check", Result: "Prior result", NextRunAt: due,
	}); err != nil {
		t.Fatal(err)
	}

	// Orphaned old private session should be reaped; fresh private session & coding session stay active.
	if err := repo.StartSession(ctx, domain.Session{ID: "stale-priv", ConversationID: conv.ID, CreatedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "personal:8", OwnerID: 8, ChatID: 8, Private: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartSession(ctx, domain.Session{ID: "fresh-priv", ConversationID: "personal:8", CreatedAt: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "coding:c1", OwnerID: 7, Private: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartSession(ctx, domain.Session{ID: "c1", ConversationID: "coding:c1", Trigger: "coding", CreatedAt: now.Add(-10 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var pruned atomic.Int32
	r := &Runtime{
		Repo:        repo,
		Activities:  a,
		IdleTimeout: 30 * time.Minute,
		MaxSteps:    4,
		Clock:       clk,
		OnPruneLogs: func(context.Context) error {
			pruned.Add(1)
			return nil
		},
	}
	r.Start(ctx)

	savedTask, ok, err := repo.Task(ctx, conv.ID, "crashed")
	if err != nil || !ok || savedTask.Status != "waiting" || savedTask.Result != "Prior result" || !savedTask.NextRunAt.Equal(due) {
		t.Fatalf("crashed task not recovered: %+v ok=%v err=%v", savedTask, ok, err)
	}
	if s, _, _ := repo.Session(ctx, "stale-priv"); s.Status != "closed" {
		t.Fatalf("stale private session not reaped: %+v", s)
	}
	if s, _, _ := repo.Session(ctx, "fresh-priv"); s.Status != "active" {
		t.Fatalf("fresh private session wrongly reaped: %+v", s)
	}
	if s, _, _ := repo.Session(ctx, "c1"); s.Status != "active" {
		t.Fatalf("coding session wrongly reaped: %+v", s)
	}

	// Advance clock by 1 hour to trigger hourly OnPruneLogs once its timer is registered.
	waitUntil(t, func() bool { return clk.HasTimerAt(now.Add(pruneLogsInterval)) })
	clk.Advance(time.Hour)
	waitUntil(t, func() bool { return pruned.Load() >= 1 })

	cancel()
	r.Wait()
	_ = storage.NewMemory()
}
