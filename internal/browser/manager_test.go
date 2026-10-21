package browser

import (
	"context"
	"errors"
	"fmt"
	"jarvis/internal/domain"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDriver struct {
	id        string
	calls     atomic.Int32
	closed    atomic.Bool
	run       func(context.Context) error
	closeGate <-chan struct{}
}

func (d *fakeDriver) Execute(ctx context.Context, _ domain.BrowserRequest) (domain.BrowserResult, error) {
	d.calls.Add(1)
	var err error
	if d.run != nil {
		err = d.run(ctx)
	}
	return domain.BrowserResult{InstanceID: d.id}, err
}
func (d *fakeDriver) Close() {
	if d.closeGate != nil {
		<-d.closeGate
	}
	d.closed.Store(true)
}
func owner(id string, bg bool) domain.BrowserOwner {
	return domain.BrowserOwner{ConversationID: "personal:1", SessionID: id, Background: bg}
}
func testManager(t *testing.T, start factory) *Manager {
	t.Helper()
	m, err := newManager(context.Background(), 5, 1, start)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}
func perform(m *Manager, o domain.BrowserOwner) (domain.BrowserResult, error) {
	return m.Execute(context.Background(), o, domain.BrowserRequest{Action: "open"}, nil)
}
func TestPoolReservesForegroundCapacityAndRetainsBrowsersUntilSessionCloses(t *testing.T) {
	var created atomic.Int32
	m := testManager(t, func(context.Context) (driver, error) { return &fakeDriver{id: fmt.Sprint(created.Add(1))}, nil })
	for i := range 4 {
		if _, err := perform(m, owner(fmt.Sprint(i), true)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := perform(m, owner("fifth-background", true)); !errors.Is(err, domain.ErrBrowserCapacity) {
		t.Fatalf("background used reserved slot: %v", err)
	}
	first, err := perform(m, owner("interactive", false))
	if err != nil {
		t.Fatal(err)
	}
	again, err := perform(m, owner("interactive", false))
	if err != nil || first.InstanceID != again.InstanceID || again.Fresh {
		t.Fatal("browser was replaced between conversational turns")
	}
	if _, err := perform(m, owner("sixth", false)); !errors.Is(err, domain.ErrBrowserCapacity) {
		t.Fatal("pool exceeded five browsers")
	}
	if created.Load() != 5 {
		t.Fatalf("launched %d processes", created.Load())
	}
	m.CloseSession("0")
	if _, err := perform(m, owner("fifth-background", true)); err != nil {
		t.Fatal("released slot not reusable", err)
	}
}
func TestClosingBrowserKeepsSlotUntilProcessExit(t *testing.T) {
	gate := make(chan struct{})
	m := testManager(t, func(context.Context) (driver, error) { return &fakeDriver{closeGate: gate}, nil })
	for i := range 5 {
		if _, err := perform(m, owner(fmt.Sprint(i), false)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() { m.CloseSession("0"); close(done) }()
	deadline := time.After(time.Second)
	for {
		m.mu.Lock()
		closing := m.entries["0"].closing
		m.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("close not started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := perform(m, owner("new", false)); !errors.Is(err, domain.ErrBrowserCapacity) {
		t.Fatal("slot freed before process stopped")
	}
	close(gate)
	<-done
	if _, err := perform(m, owner("new", false)); err != nil {
		t.Fatal(err)
	}
}
func TestOwnersAreIsolatedAndActionsSerializePerBrowser(t *testing.T) {
	var current, peak atomic.Int32
	enter := make(chan struct{}, 4)
	release := make(chan struct{})
	m := testManager(t, func(context.Context) (driver, error) {
		return &fakeDriver{run: func(ctx context.Context) error {
			n := current.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			enter <- struct{}{}
			defer current.Add(-1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})
	var wg sync.WaitGroup
	for _, id := range []string{"one", "one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := perform(m, owner(id, false)); err != nil {
				t.Error(err)
			}
		}(id)
	}
	for range 2 {
		select {
		case <-enter:
		case <-time.After(time.Second):
			t.Fatal("separate browser operations did not run concurrently")
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("same browser executed concurrently: %d", peak.Load())
	}
	close(release)
	wg.Wait()
	wrong := owner("one", false)
	wrong.ConversationID = "group:secret"
	if _, err := perform(m, wrong); err == nil {
		t.Fatal("browser leaked across scopes")
	}
	m.Reconcile(map[string]bool{"one": true}, time.Now())
	m.mu.Lock()
	count := len(m.entries)
	m.mu.Unlock()
	if count != 1 {
		t.Fatal("closed session browser was not reaped")
	}
}
func TestStartupFailureAndOperationTimeoutReleaseSlots(t *testing.T) {
	m := testManager(t, func(context.Context) (driver, error) { return nil, errors.New("launch failed") })
	if _, err := perform(m, owner("fail", false)); err == nil {
		t.Fatal("expected startup error")
	}
	m.mu.Lock()
	count := len(m.entries)
	m.mu.Unlock()
	if count != 0 {
		t.Fatal("failed startup leaked capacity")
	}
	d := &fakeDriver{run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	m.start = func(context.Context) (driver, error) { return d, nil }
	m.timeout = time.Millisecond
	if _, err := perform(m, owner("timeout", false)); err == nil {
		t.Fatal("expected timeout")
	}
	if !d.closed.Load() {
		t.Fatal("timed out process left running")
	}
}
func TestCapacityDoesNotRecordAnAttemptAndCancellationStopsLaunch(t *testing.T) {
	m := testManager(t, func(ctx context.Context) (driver, error) { return &fakeDriver{}, nil })
	for i := range 5 {
		perform(m, owner(fmt.Sprint(i), false))
	}
	called := false
	_, err := m.Execute(context.Background(), owner("wait", false), domain.BrowserRequest{Action: "open"}, func() error { called = true; return nil })
	if !errors.Is(err, domain.ErrBrowserCapacity) || called {
		t.Fatal("capacity wait recorded an action that never ran")
	}
	m.CloseSession("0")
	m.start = func(ctx context.Context) (driver, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.Execute(ctx, owner("cancel", false), domain.BrowserRequest{Action: "open"}, nil); err == nil {
		t.Fatal("cancelled launch succeeded")
	}
	m.mu.Lock()
	count := len(m.entries)
	m.mu.Unlock()
	if count != 4 {
		t.Fatal("cancelled startup leaked a slot")
	}
}

func TestReconciliationDoesNotCloseSessionsCreatedAfterDatabaseSnapshot(t *testing.T) {
	observed := time.Now().Add(-time.Second)
	d := &fakeDriver{}
	m := testManager(t, func(context.Context) (driver, error) { return d, nil })
	if _, err := perform(m, owner("new-session", false)); err != nil {
		t.Fatal(err)
	}
	m.Reconcile(map[string]bool{}, observed)
	if d.closed.Load() {
		t.Fatal("stale database snapshot closed a newly created session")
	}
	m.Reconcile(map[string]bool{}, time.Now())
	if !d.closed.Load() {
		t.Fatal("orphaned browser was not closed on fresh observation")
	}
}

func TestReadOfMissingPageAndDownloadDoNotLaunchUnusedBrowser(t *testing.T) {
	var started atomic.Int32
	m := testManager(t, func(context.Context) (driver, error) { started.Add(1); return &fakeDriver{}, nil })
	o := owner("session", false)
	out, err := m.Execute(context.Background(), o, domain.BrowserRequest{Action: "tabs"}, nil)
	if err != nil || len(out.Pages) != 0 {
		t.Fatal("empty tab list failed", err)
	}
	_, err = m.Execute(context.Background(), o, domain.BrowserRequest{Action: "snapshot", PageID: "expired"}, nil)
	if err == nil {
		t.Fatal("old page ID accepted")
	}
	_, err = m.Execute(context.Background(), o, domain.BrowserRequest{Action: "download", URL: "http://127.0.0.1/private"}, nil)
	if err == nil {
		t.Fatal("private download accepted")
	}
	if started.Load() != 0 {
		t.Fatal("non-browser operation launched Chromium")
	}
}

type crashableDriver struct {
	fakeDriver
	alive atomic.Bool
}

func (d *crashableDriver) Alive() bool { return d.alive.Load() }
func TestIdleCrashedBrowserReleasesSlotEvenWithActiveSession(t *testing.T) {
	d := &crashableDriver{}
	d.alive.Store(true)
	m := testManager(t, func(context.Context) (driver, error) { return d, nil })
	if _, err := perform(m, owner("idle", false)); err != nil {
		t.Fatal(err)
	}
	d.alive.Store(false)
	m.Reconcile(map[string]bool{"idle": true}, time.Now())
	m.mu.Lock()
	count := len(m.entries)
	m.mu.Unlock()
	if count != 0 || !d.closed.Load() {
		t.Fatal("dead browser retained capacity")
	}
}
