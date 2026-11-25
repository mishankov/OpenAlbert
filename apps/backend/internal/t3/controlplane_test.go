package t3

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/scheduling"
	"openalbert/apps/backend/internal/storage"
)

type spyObserver struct {
	mu     sync.Mutex
	events []domain.T3Event
	hosts  []domain.T3Host
}

func (s *spyObserver) OnThreadEvent(_ context.Context, ev domain.T3Event) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}

func (s *spyObserver) OnHostUpdated(_ context.Context, h domain.T3Host) error {
	s.mu.Lock()
	s.hosts = append(s.hosts, h)
	s.mu.Unlock()
	return nil
}

func (s *spyObserver) eventCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

type flakyInboxRepo struct {
	*storage.Memory
	failAccept atomic.Bool
	failThread atomic.Bool
}

func (r *flakyInboxRepo) AcceptIncoming(ctx context.Context, in domain.Incoming, offset int64) (bool, error) {
	if r.failAccept.Load() {
		return false, errors.New("transient inbox write failure")
	}
	return r.Memory.AcceptIncoming(ctx, in, offset)
}

func (r *flakyInboxRepo) T3Thread(ctx context.Context, threadID string) (domain.T3Thread, bool, error) {
	if r.failThread.Load() {
		return domain.T3Thread{}, false, errors.New("transient thread lookup failure")
	}
	return r.Memory.T3Thread(ctx, threadID)
}

func TestDynamicOnlineAndOfflineQueuingVsLiveSync(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemory()
	var clockMu sync.Mutex
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	obs := &spyObserver{}
	cp := New(mem, mem, Config{
		OwnerUserID:     42,
		OnlineThreshold: 3 * time.Minute,
		DispatchTimeout: 2 * time.Second,
		Now:             getNow,
		Observers:       []EventObserver{obs},
	})

	host, err := cp.RegisterHost(ctx, RegisterHostRequest{
		EnvironmentID: "env-mac",
		Label:         "MacBook Air",
		Projects:      []domain.T3Project{{ID: "proj-1", Title: "Jarvis", WorkspaceRoot: "/code/jarvis"}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !host.Online {
		t.Fatal("freshly registered host should be online")
	}

	// Advance past staleness threshold -> offline.
	advance(4 * time.Minute)
	hosts, err := cp.ListHosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].Online {
		t.Fatalf("stale host should be offline: %+v err=%v", hosts, err)
	}

	// Offline DispatchAndWait returns queued immediately without waiting 2s.
	start := time.Now()
	queued, err := cp.DispatchAndWait(ctx, "", "env-mac", domain.T3ActionThreadLaunch, map[string]any{
		"project_id":      "proj-1",
		"title":           "Offline task",
		"message":         "run tests",
		"conversation_id": "personal:42",
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("offline dispatch: %v", err)
	}
	if queued.Status != "queued" || queued.CommandID == "" || queued.HostOnline {
		t.Fatalf("unexpected offline dispatch result: %+v", queued)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("offline dispatch blocked too long: %v", time.Since(start))
	}

	// Connecting an SSE stream makes host online even with stale last_seen_at!
	advance(10 * time.Minute)
	wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-mac")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	hosts, _ = cp.ListHosts(ctx)
	if !hosts[0].Online {
		t.Fatal("host with active SSE stream must be online")
	}
	// Drain the previously queued command.
	drained, err := cp.ClaimPendingCommands(ctx, "env-mac", 10)
	if err != nil || len(drained) != 1 || drained[0].ID != queued.CommandID {
		t.Fatalf("expected queued command on reconnect: %+v err=%v", drained, err)
	}

	// Live sync DispatchAndWait unblocks instantaneously when host posts result.
	done := make(chan DispatchResult, 1)
	go func() {
		res, err := cp.DispatchAndWait(ctx, "", "env-mac", domain.T3ActionThreadLaunch, map[string]any{
			"project_id":      "proj-1",
			"title":           "Live task",
			"message":         "implement feature",
			"model_selection": map[string]any{"provider_instance_id": "ag-1", "model": "gemini-4-neon"},
			"conversation_id": "personal:42",
		}, 2*time.Second)
		if err != nil {
			t.Errorf("live dispatch: %v", err)
			return
		}
		done <- res
	}()

	select {
	case <-wakeup:
	case <-time.After(time.Second):
		t.Fatal("SSE subscriber did not wake up on new command")
	}
	liveCmds, err := cp.ClaimPendingCommands(ctx, "env-mac", 10)
	if err != nil || len(liveCmds) != 1 {
		t.Fatalf("claim live command: %+v err=%v", liveCmds, err)
	}
	if err := cp.RecordCommandResult(ctx, liveCmds[0].ID, CommandResultRequest{
		EnvironmentID: "env-mac",
		OK:            true,
		Result:        json.RawMessage(`{"thread_id":"th-live-1","link":"t3://thread/th-live-1","status":"running","branch":"feat/live"}`),
	}); err != nil {
		t.Fatalf("record result: %v", err)
	}

	select {
	case res := <-done:
		if res.Status != domain.T3CommandStatusSucceeded || res.Thread == nil || res.Thread.ThreadID != "th-live-1" {
			t.Fatalf("unexpected live dispatch result: %+v", res)
		}
		if res.Thread.ConversationID != "personal:42" || res.Thread.Model != "gemini-4-neon" || res.Thread.ProviderInstanceID != "ag-1" {
			t.Fatalf("thread metadata not populated from payload: %+v", res.Thread)
		}
	case <-time.After(time.Second):
		t.Fatal("DispatchAndWait did not unblock")
	}

	// Disconnect stream and advance past threshold -> offline again.
	unsub()
	advance(5 * time.Minute)
	hosts, _ = cp.ListHosts(ctx)
	if hosts[0].Online {
		t.Fatal("host should return to offline after stream closes and threshold elapses")
	}
}

func TestEventBeforeLaunchResultWakesOnceOwnershipEstablished(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemory()
	flaky := &flakyInboxRepo{Memory: mem}
	ownerConv := domain.Conversation{ID: "personal:55", ChatID: 55, OwnerID: 55, Private: true}
	if err := mem.SaveConversation(ctx, ownerConv); err != nil {
		t.Fatal(err)
	}
	cp := New(flaky, flaky, Config{OwnerUserID: 55})
	if _, err := cp.RegisterHost(ctx, RegisterHostRequest{EnvironmentID: "env-fast", Label: "Fast Mac"}); err != nil {
		t.Fatal(err)
	}
	launchCmd, err := cp.EnqueueCommand(ctx, "cmd-fast-launch", "env-fast", domain.T3ActionThreadLaunch, map[string]any{
		"project_id":      "proj-1",
		"title":           "Fast Thread",
		"message":         "quick task",
		"conversation_id": ownerConv.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fast thread finishes BEFORE launch result POST arrives (with spoofed owner fields that must be ignored!).
	dup, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:        "ev-fast-done",
		EnvironmentID:  "env-fast",
		ThreadID:       "th-fast-1",
		Kind:           domain.T3EventThreadCompleted,
		Status:         "completed",
		Summary:        "Finished in 100ms",
		ConversationID: "personal:999",
		TaskID:         "spoofed-task",
	}, json.RawMessage(`{"event_id":"ev-fast-done","kind":"thread_completed","summary":"Finished in 100ms","conversation_id":"personal:999","task_id":"spoofed-task"}`))
	if err != nil || dup {
		t.Fatalf("fast completed event: dup=%v err=%v", dup, err)
	}
	// Owner not yet known -> no inbox message yet.
	if pending, _ := mem.PendingIncoming(ctx, 10); len(pending) != 0 {
		t.Fatalf("unexpected early wake before ownership: %+v", pending)
	}

	// Simulate transient wake failure when launch result first commits:
	// launch commits, RecordCommandResult returns error so bridge retries.
	flaky.failAccept.Store(true)
	if err := cp.RecordCommandResult(ctx, launchCmd.ID, CommandResultRequest{
		EnvironmentID: "env-fast",
		OK:            true,
		Result:        json.RawMessage(`{"thread_id":"th-fast-1","status":"running","branch":"feat/fast"}`),
	}); err == nil {
		t.Fatal("expected error when deferred wake fails on initial launch result")
	}
	flaky.failAccept.Store(false)

	// On bridge retry, if thread lookup transiently fails, RecordCommandResult MUST propagate the error (S3).
	flaky.failThread.Store(true)
	if err := cp.RecordCommandResult(ctx, launchCmd.ID, CommandResultRequest{
		EnvironmentID: "env-fast",
		OK:            true,
		Result:        json.RawMessage(`{"thread_id":"th-fast-1","status":"running","branch":"feat/fast"}`),
	}); err == nil {
		t.Fatal("expected thread lookup error to propagate on launch result retry")
	}
	flaky.failThread.Store(false)

	// Next bridge retry succeeds and delivers the pending wake to the persisted owner (NOT spoofed personal:999)!
	if err := cp.RecordCommandResult(ctx, launchCmd.ID, CommandResultRequest{
		EnvironmentID: "env-fast",
		OK:            true,
		Result:        json.RawMessage(`{"thread_id":"th-fast-1","status":"running","branch":"feat/fast"}`),
	}); err != nil {
		t.Fatalf("record launch result retry: %v", err)
	}
	th, err := cp.Thread(ctx, "th-fast-1")
	if err != nil {
		t.Fatal(err)
	}
	if th.Status != "completed" || th.ConversationID != ownerConv.ID {
		t.Fatalf("late launch result must preserve completed status and attach owner: %+v", th)
	}
	pending, _ := mem.PendingIncoming(ctx, 10)
	if len(pending) != 1 || pending[0].ID != "t3-event:ev-fast-done" || pending[0].Conversation.ID != ownerConv.ID || pending[0].TaskContextID != "" || !strings.Contains(pending[0].Text, "Finished in 100ms") {
		t.Fatalf("deferred wake was not delivered to persisted owner: %+v", pending)
	}

	// Repeated launch result must NOT reopen completed thread or duplicate wake.
	if err := cp.RecordCommandResult(ctx, launchCmd.ID, CommandResultRequest{
		EnvironmentID: "env-fast",
		OK:            true,
		Result:        json.RawMessage(`{"thread_id":"th-fast-1","status":"running"}`),
	}); err != nil {
		t.Fatalf("duplicate launch result: %v", err)
	}
	th2, _ := cp.Thread(ctx, "th-fast-1")
	if th2.Status != "completed" {
		t.Fatalf("duplicate launch result reopened thread: %+v", th2)
	}
	if pending2, _ := mem.PendingIncoming(ctx, 10); len(pending2) != 1 {
		t.Fatalf("duplicate launch result created duplicate wake: %+v", pending2)
	}
}

func TestFailedWakeRetriesIdempotentlyOnDuplicateEvent(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemory()
	flaky := &flakyInboxRepo{Memory: mem}
	ownerConv := domain.Conversation{ID: "personal:66", ChatID: 66, OwnerID: 66, Private: true}
	if err := mem.SaveConversation(ctx, ownerConv); err != nil {
		t.Fatal(err)
	}
	obs := &spyObserver{}
	cp := New(mem, flaky, Config{OwnerUserID: 66, Observers: []EventObserver{obs}})
	if _, err := mem.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:       "th-retry",
		EnvironmentID:  "env-1",
		ConversationID: ownerConv.ID,
		Status:         "running",
	}); err != nil {
		t.Fatal(err)
	}

	ev := domain.T3Event{
		EventID:       "ev-flaky-1",
		EnvironmentID: "env-1",
		ThreadID:      "th-retry",
		Kind:          domain.T3EventThreadFailed,
		Error:         "build failed",
	}

	// First delivery fails during inbox write -> returns error, event stored but unwoken.
	flaky.failAccept.Store(true)
	if _, err := cp.IngestEvent(ctx, ev, nil); err == nil {
		t.Fatal("expected error when wake inbox write fails")
	}
	if pending, _ := mem.PendingIncoming(ctx, 10); len(pending) != 0 {
		t.Fatalf("inbox should still be empty: %+v", pending)
	}
	if obs.eventCount() != 1 {
		t.Fatalf("observer should have fired once on initial ingest, got %d", obs.eventCount())
	}

	// Retry same event_id after transient failure clears -> succeeds, returns deduplicated=true, delivers wake, does NOT double-fire observer!
	flaky.failAccept.Store(false)
	dup, err := cp.IngestEvent(ctx, ev, nil)
	if err != nil || !dup {
		t.Fatalf("retry should succeed as deduplicated=true: dup=%v err=%v", dup, err)
	}
	if obs.eventCount() != 1 {
		t.Fatalf("retry must not double-notify observer, got %d", obs.eventCount())
	}
	pending, _ := mem.PendingIncoming(ctx, 10)
	if len(pending) != 1 || pending[0].ID != "t3-event:ev-flaky-1" {
		t.Fatalf("retried wake was not delivered: %+v", pending)
	}
}

func TestEventIngestionIdempotencyObserversAndTaskSchedulePreservation(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemory()
	ownerConv := domain.Conversation{ID: "personal:77", ChatID: 77, OwnerID: 77, Private: true}
	if err := mem.SaveConversation(ctx, ownerConv); err != nil {
		t.Fatal(err)
	}
	baseNow := time.Date(2026, 11, 25, 9, 42, 0, 0, time.UTC)
	slot10 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	oneTimeTask, err := mem.CreateTask(ctx, domain.Task{
		ID:             "task-once",
		ConversationID: ownerConv.ID,
		Title:          "Watch PR once",
		Goal:           "Wait for T3 thread",
		Status:         "waiting",
		NextRunAt:      slot10,
	})
	if err != nil {
		t.Fatal(err)
	}
	repeatingTask, err := mem.CreateTask(ctx, domain.Task{
		ID:              "task-hourly",
		ConversationID:  ownerConv.ID,
		Title:           "Hourly PR check",
		Goal:            "Check hourly",
		Status:          "waiting",
		ScheduleSeconds: 3600,
		NextRunAt:       slot10,
	})
	if err != nil {
		t.Fatal(err)
	}
	runningOneTimeTask, err := mem.CreateTask(ctx, domain.Task{
		ID:             "task-once-running",
		ConversationID: ownerConv.ID,
		Title:          "Running one-time task",
		Goal:           "Currently executing",
		Status:         "running",
		NextRunAt:      baseNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	var taskWakeMu sync.Mutex
	var wokenTasks []string
	obs := &spyObserver{}
	cp := New(mem, mem, Config{
		OwnerUserID: 77,
		Now:         func() time.Time { return baseNow },
		Observers:   []EventObserver{obs},
		TaskChanged: func(_ context.Context, scope, id string) error {
			taskWakeMu.Lock()
			wokenTasks = append(wokenTasks, scope+"/"+id)
			taskWakeMu.Unlock()
			return nil
		},
	})

	for _, p := range []domain.T3ThreadPatch{
		{ThreadID: "th-conv", EnvironmentID: "env-1", ConversationID: ownerConv.ID, Title: "Conv Thread", Status: "running", At: baseNow},
		{ThreadID: "th-once", EnvironmentID: "env-1", ConversationID: ownerConv.ID, TaskID: oneTimeTask.ID, Title: "One-time Task Thread", Status: "running", At: baseNow},
		{ThreadID: "th-hourly", EnvironmentID: "env-1", ConversationID: ownerConv.ID, TaskID: repeatingTask.ID, Title: "Hourly Task Thread", Status: "running", At: baseNow},
		{ThreadID: "th-once-running", EnvironmentID: "env-1", ConversationID: ownerConv.ID, TaskID: runningOneTimeTask.ID, Title: "Running One-time Task Thread", Status: "running", At: baseNow},
	} {
		if _, err := mem.UpsertT3Thread(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Non-actionable progress event: observer runs, NO conversation wake.
	dup, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-prog-1",
		EnvironmentID: "env-1",
		ThreadID:      "th-conv",
		Kind:          domain.T3EventThreadProgress,
		Summary:       "Running tests",
	}, nil)
	if err != nil || dup {
		t.Fatalf("progress event: dup=%v err=%v", dup, err)
	}
	if pending, _ := mem.PendingIncoming(ctx, 10); len(pending) != 0 {
		t.Fatalf("progress event should not wake conversation: %+v", pending)
	}

	// 2. Actionable thread_waiting_input wakes owning conversation; duplicate does NOT re-wake.
	evWait := domain.T3Event{
		EventID:       "ev-wait-1",
		EnvironmentID: "env-1",
		ThreadID:      "th-conv",
		Kind:          domain.T3EventThreadWaitingInput,
		PendingRequest: &domain.T3PendingRequest{
			RequestID: "req-42",
			Questions: []json.RawMessage{json.RawMessage(`{"question":"Choose branch?"}`)},
		},
		Summary: "Need branch choice",
	}
	if dup, err := cp.IngestEvent(ctx, evWait, nil); err != nil || dup {
		t.Fatalf("waiting event: dup=%v err=%v", dup, err)
	}
	if dup, err := cp.IngestEvent(ctx, evWait, nil); err != nil || !dup {
		t.Fatalf("duplicate waiting event should deduplicate: dup=%v err=%v", dup, err)
	}
	if obs.eventCount() != 2 {
		t.Fatalf("duplicate event must not re-notify observer; count=%d", obs.eventCount())
	}

	// 3. One-time task thread wakes task directly via NextRunAt=now + TaskChanged.
	if dup, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-done-once",
		EnvironmentID: "env-1",
		ThreadID:      "th-once",
		Kind:          domain.T3EventThreadCompleted,
		Summary:       "PR ready",
		PRURL:         "https://github.com/org/repo/pull/99",
		Output:        json.RawMessage(`{"pr":99}`),
	}, nil); err != nil || dup {
		t.Fatalf("one-time task completed event: dup=%v err=%v", dup, err)
	}
	taskWakeMu.Lock()
	gotTaskWakes := append([]string(nil), wokenTasks...)
	taskWakeMu.Unlock()
	if len(gotTaskWakes) != 1 || gotTaskWakes[0] != ownerConv.ID+"/"+oneTimeTask.ID {
		t.Fatalf("expected one-time task wake, got %+v", gotTaskWakes)
	}
	updatedOnce, _, _ := mem.Task(ctx, ownerConv.ID, oneTimeTask.ID)
	if !updatedOnce.NextRunAt.Equal(baseNow) {
		t.Fatalf("expected one-time task NextRunAt pulled to now, got %v", updatedOnce.NextRunAt)
	}

	// 4. Repeating hourly task at 09:42 MUST preserve its 10:00 NextRunAt slot anchor
	// (next scheduled slot after 10:00 must be 11:00, NOT 10:42!) while waking immediately via conversation inbox with TaskContextID.
	if dup, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-done-hourly",
		EnvironmentID: "env-1",
		ThreadID:      "th-hourly",
		Kind:          domain.T3EventThreadCompleted,
		Summary:       "Hourly thread finished early",
	}, nil); err != nil || dup {
		t.Fatalf("hourly task completed event: dup=%v err=%v", dup, err)
	}
	stillHourly, _, _ := mem.Task(ctx, ownerConv.ID, repeatingTask.ID)
	if !stillHourly.NextRunAt.Equal(slot10) {
		t.Fatalf("repeating task schedule anchor drifted: want %v, got %v", slot10, stillHourly.NextRunAt)
	}
	nextSlot, err := scheduling.Next(stillHourly, slot10.Add(time.Minute))
	if err != nil || !nextSlot.Equal(slot10.Add(time.Hour)) {
		t.Fatalf("repeating task next slot drifted from 11:00: got %v err=%v", nextSlot, err)
	}
	pending, _ := mem.PendingIncoming(ctx, 10)
	foundHourlyWake := false
	for _, in := range pending {
		if in.ID == "t3-event:ev-done-hourly" && in.TaskContextID == repeatingTask.ID {
			foundHourlyWake = true
		}
	}
	if !foundHourlyWake {
		t.Fatalf("expected immediate conversation wake with TaskContextID=%s, got %+v", repeatingTask.ID, pending)
	}

	// 5. Already-running one-time task MUST route actionable events through the durable
	// conversation inbox with TaskContextID instead of relying on TaskChanged (S9).
	if dup, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-done-running-once",
		EnvironmentID: "env-1",
		ThreadID:      "th-once-running",
		Kind:          domain.T3EventThreadCompleted,
		Summary:       "Finished while one-time task running",
	}, nil); err != nil || dup {
		t.Fatalf("running one-time task event: dup=%v err=%v", dup, err)
	}
	pending, _ = mem.PendingIncoming(ctx, 10)
	foundRunningWake := false
	for _, in := range pending {
		if in.ID == "t3-event:ev-done-running-once" && in.TaskContextID == runningOneTimeTask.ID {
			foundRunningWake = true
		}
	}
	if !foundRunningWake {
		t.Fatalf("expected durable inbox wake with TaskContextID=%s for running one-time task, got %+v", runningOneTimeTask.ID, pending)
	}
}
