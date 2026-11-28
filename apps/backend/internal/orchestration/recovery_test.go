package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/shared/backup"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
)

// S1: Crash after a tool queues a reply but before Finish must resume the exact
// same turn key on restart so tool/model receipts replay without duplicate replies.
func TestSessionTurnCrashAfterToolReplaysExactTurnKeyWithoutDuplicateReply(t *testing.T) {
	a, repo, _, _ := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var calls atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(n int, _ []domain.Message) (domain.Message, error) {
			c := calls.Add(1)
			switch c {
			case 1:
				return reply("First and only reply"), nil
			case 2:
				// Simulate process crash / SIGTERM immediately after send_message committed
				// its outbox entry and receipt, before Finish runs.
				cancelRun1()
				return domain.Message{}, context.Canceled
			default:
				return finish("done"), nil
			}
		}}

	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Incoming message must still be pending after crash before Finish,
	// and the single reply must already be in the outbox.
	pendingIn, err := repo.PendingIncoming(ctx, 10)
	if err != nil || len(pendingIn) != 1 {
		t.Fatalf("expected pending incoming after crash before Finish, got %+v %v", pendingIn, err)
	}
	out1, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out1) != 1 {
		t.Fatalf("expected 1 queued reply before restart, got %+v %v", out1, err)
	}

	// Simulate fresh process startup with a new Runtime over the same SQLite/repo.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	out2, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out2) != 1 {
		t.Fatalf("restart duplicated reply: got %d (%+v), err=%v", len(out2), out2, err)
	}
}

// S2: Normal runtime shutdown (SIGTERM / ctx cancel) during a model call must
// leave incoming requests pending and one-shot tasks resumable instead of
// dropping the request or permanently failing the task.
func TestGracefulShutdownLeavesPendingRequestAndOneShotTaskResumable(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var firstTurnBlocked atomic.Bool
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		if firstTurnBlocked.CompareAndSwap(false, true) {
			cancelRun1()
			return domain.Message{}, context.Canceled
		}
		return reply("Answered after restart"), nil
	}}

	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Message was NOT silently discarded by Finish on SIGTERM.
	pendingIn, err := repo.PendingIncoming(ctx, 10)
	if err != nil || len(pendingIn) != 1 {
		t.Fatalf("SIGTERM discarded unanswered request: %+v %v", pendingIn, err)
	}

	// Restart answers the pending request cleanly.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Text != "Answered after restart" {
		t.Fatalf("expected answer after restart, got %+v %v", out, err)
	}

	// One-shot scheduled task interrupted by SIGTERM must stay running (not failed)
	// and complete after startup recovery.
	task := domain.Task{
		ID:             "oneshot-sigterm",
		ConversationID: conv.ID,
		Title:          "One-shot reminder",
		Goal:           "Remind once",
		Status:         "waiting",
		NextRunAt:      clk.Now(),
		CreatedAt:      clk.Now(),
		UpdatedAt:      clk.Now(),
	}
	if _, err := repo.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	taskCtx1, cancelTask1 := context.WithCancel(ctx)
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		cancelTask1()
		return domain.Message{}, context.Canceled
	}}
	rtTask1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rtTask1.RunTaskOnce(taskCtx1, TaskInput{ConversationID: conv.ID, ID: task.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on task shutdown, got %v", err)
	}
	gotTask, ok, err := repo.Task(ctx, conv.ID, task.ID)
	if err != nil || !ok || gotTask.Status != "running" {
		t.Fatalf("SIGTERM permanently failed or altered one-shot task: %+v %v", gotTask, err)
	}

	// Startup recovery resets running -> waiting and completes the task.
	rtTask2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rtTask2.recoverRunningTasks(ctx); err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		return finish("completed"), nil
	}}
	if err := rtTask2.RunTaskOnce(ctx, TaskInput{ConversationID: conv.ID, ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	gotTask, ok, err = repo.Task(ctx, conv.ID, task.ID)
	if err != nil || !ok || gotTask.Status != "completed" {
		t.Fatalf("expected recovered one-shot task to complete, got %+v %v", gotTask, err)
	}
}

// S3: Crash after a scheduled task queues a reminder but before AdvanceTask must
// reuse the stable occurrence ID on recovery and not duplicate the reminder.
func TestTaskCrashAfterReminderQueuedReusesOccurrenceKeyWithoutDuplicate(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC))

	task := domain.Task{
		ID:             "remind-once",
		ConversationID: conv.ID,
		Title:          "Reminder",
		Goal:           "Send one reminder",
		Status:         "waiting",
		NextRunAt:      clk.Now(),
		CreatedAt:      clk.Now(),
		UpdatedAt:      clk.Now(),
	}
	if _, err := repo.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	taskCtx1, cancelTask1 := context.WithCancel(ctx)
	var step atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, _ []domain.Message) (domain.Message, error) {
			switch step.Add(1) {
			case 1:
				return reply("Time for standup!"), nil
			case 2:
				cancelTask1()
				return domain.Message{}, context.Canceled
			default:
				return finish("reminded"), nil
			}
		}}

	rt1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rt1.RunTaskOnce(taskCtx1, TaskInput{ConversationID: conv.ID, ID: task.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation during task run, got %v", err)
	}
	out1, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out1) != 1 {
		t.Fatalf("expected 1 queued reminder before restart, got %+v %v", out1, err)
	}

	// Recover on restart and run the occurrence again.
	rt2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rt2.recoverRunningTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt2.RunTaskOnce(ctx, TaskInput{ConversationID: conv.ID, ID: task.ID}); err != nil {
		t.Fatal(err)
	}

	out2, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out2) != 1 {
		t.Fatalf("task recovery sent duplicate reminder: got %d (%+v), err=%v", len(out2), out2, err)
	}
	gotTask, ok, err := repo.Task(ctx, conv.ID, task.ID)
	if err != nil || !ok || gotTask.Status != "completed" {
		t.Fatalf("expected completed task after recovery, got %+v %v", gotTask, err)
	}
}

// S4: Restarting mid-way through a session's idle period must restore the
// remaining timer from turn completion and close the session on deadline.
func TestRestartRestoresRemainingSessionIdleTimer(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		// Simulate a 2-minute turn so completion deadline is T0 + 2m + 30m = 10:32.
		clk.Advance(2 * time.Minute)
		return finish("done"), nil
	}}

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun1()
	r1.Wait()

	// Restart 5 minutes after completion (10:07, 25 minutes remaining until 10:32).
	clk.Advance(5 * time.Minute)
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.reapOrphanedSessions(runCtx2); err != nil {
		t.Fatal(err)
	}
	// Session is still active right after restart.
	active, err := repo.ActiveSessions(ctx)
	if err != nil || len(active) != 1 || active[0].ConversationID != conv.ID {
		t.Fatalf("expected active session after early restart, got %+v %v", active, err)
	}
	// Wait for the restored session actor to register its remaining idle timer.
	waitUntil(t, func() bool { return clk.ActiveTimers() > 0 })

	// 20 minutes later (10:27, still 5m before completion-based deadline 10:32): still active!
	clk.Advance(20 * time.Minute)
	active, err = repo.ActiveSessions(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("session closed too early before completion-based deadline: %+v %v", active, err)
	}

	// 6 more minutes (10:33, past 10:32 deadline): closes automatically and queues for analysis!
	clk.Advance(6 * time.Minute)
	waitUntil(t, func() bool {
		as, _ := repo.ActiveSessions(ctx)
		return len(as) == 0
	})
	cancelRun2()
	r2.Wait()

	pendingAnalysis, err := repo.PendingAnalysis(ctx, 10)
	if err != nil || len(pendingAnalysis) != 1 {
		t.Fatalf("expected closed session in PendingAnalysis, got %+v %v", pendingAnalysis, err)
	}
}

// S5: A /stop or /new queued between drainBatch and startTurnCancel must be
// observed atomically under the actor lock before entering prepareTurn.
func TestStartTurnCancelDetectsQueuedControlCommand(t *testing.T) {
	actor := newSessionActor()
	actor.enqueue(Event{ID: "msg-1"})
	batch := actor.drainBatch()
	if len(batch) != 1 {
		t.Fatalf("expected 1 drained event, got %d", len(batch))
	}

	// Control command arrives in the gap between drainBatch and startTurnCancel.
	actor.enqueue(Event{ID: "stop-1", Stop: true})

	runctx, cancel := context.WithCancelCause(context.Background())
	if actor.startTurnCancel(cancel) {
		t.Fatal("startTurnCancel must reject turn start when /stop is already queued")
	}
	if !isUserCanceled(runctx) {
		t.Fatalf("expected errUserCancel cause, got %v", context.Cause(runctx))
	}
}

// S6: Backup dispatch checkpoint must only be written after RunBackupEvent
// succeeds, so an interrupted or failing delivery retries on the next poll.
func TestBackupEventCheckpointSavedOnlyAfterSuccess(t *testing.T) {
	a, repo, _, owner := newFixture(t)
	a.OwnerUserID = owner.OwnerID
	dir := t.TempDir()
	a.Backups = &backup.Reader{Dir: dir}
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	ev := backup.Event{ID: "retry-until-ok", Kind: "failed", At: now}
	if err := backup.AtomicJSON(filepath.Join(dir, "events", ev.ID+".json"), ev); err != nil {
		t.Fatal(err)
	}

	// First dispatch is interrupted by runtime shutdown before RunBackupEvent finishes.
	ctx1, cancel1 := context.WithCancel(context.Background())
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		cancel1()
		return domain.Message{}, context.Canceled
	}}
	r1 := &Runtime{Repo: repo, Activities: a, MaxSteps: 3, Clock: NewFakeClock(now), runCtx: ctx1}
	if err := r1.DispatchBackupEvents(ctx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	cpKey := "backup-dispatch/" + agent.ID(owner.ID, ev.ID)
	if _, done, err := repo.LoadCheckpoint(context.Background(), cpKey); err != nil || done {
		t.Fatalf("checkpoint must not be saved before RunBackupEvent succeeds: done=%v err=%v", done, err)
	}

	// Second dispatch after restart completes and persists the checkpoint.
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		return reply("Backup failed, please check S3."), nil
	}}
	r2 := &Runtime{Repo: repo, Activities: a, MaxSteps: 3, Clock: NewFakeClock(now)}
	if err := r2.DispatchBackupEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	r2.Wait()

	if _, done, err := repo.LoadCheckpoint(context.Background(), cpKey); err != nil || !done {
		t.Fatalf("expected checkpoint saved after successful RunBackupEvent: done=%v err=%v", done, err)
	}
	out, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(out) != 1 {
		t.Fatalf("expected 1 backup alert after retry, got %+v %v", out, err)
	}
}

// S7: Over 50 permanent analysis failures must mark sessions failed in SQLite
// rather than starving later sessions behind the 50-row PendingAnalysis limit.
func TestTerminalAnalysisFailuresDoNotStarveLaterSessions(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	const failingCount = 52
	const goodCount = 3
	for i := range failingCount + goodCount {
		sid := fmt.Sprintf("sess-%03d", i)
		if err := repo.StartSession(ctx, domain.Session{
			ID:             sid,
			ConversationID: conv.ID,
			Status:         "active",
			CreatedAt:      clk.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if err := repo.EndSession(ctx, sid, ""); err != nil {
			t.Fatal(err)
		}
	}

	var analyzedGood atomic.Int32
	r := newTestRuntime(a, clk, 30*time.Minute, 6)
	r.hooks = &activityHooks{
		analyze: func(_ context.Context, s domain.Session, _ func(context.Context, domain.Session) error) error {
			var idx int
			if _, err := fmt.Sscanf(s.ID, "sess-%d", &idx); err == nil && idx < failingCount {
				return domain.NewNonRetryableError("bad model request", "model_bad_request", errors.New("400"))
			}
			analyzedGood.Add(1)
			return repo.MarkAnalyzed(ctx, s.ID)
		},
	}

	// First batch processes the oldest 50 failing sessions and marks them permanently failed.
	if err := r.dispatchAnalysis(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	// Second batch must now see the remaining 2 failing + 3 good sessions without starvation.
	if err := r.dispatchAnalysis(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	if got := analyzedGood.Load(); got != goodCount {
		t.Fatalf("expected %d healthy sessions analyzed after >50 terminal failures, got %d", goodCount, got)
	}
	remaining, err := repo.PendingAnalysis(ctx, 100)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("expected 0 remaining pending analyses, got %d (%v)", len(remaining), err)
	}
}

// S1 (steering): Restarting after a follow-up steered away from an earlier
// model step's tool call must resume at the post-steering step and never
// execute the discarded cached action.
func TestSteeringCrashRecoverySkipsDiscardedCachedToolAction(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var step atomic.Int32
	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1

	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, _ []domain.Message) (domain.Message, error) {
			switch step.Add(1) {
			case 1:
				// Step 0 returns task_create, then before execution the user sends a
				// follow-up steering message cancelling it.
				if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
					ID:           "steer-cancel",
					Conversation: conv,
					Text:         "Actually cancel that, do not create any task.",
					CreatedAt:    clk.Now().Add(time.Second),
				}, 2); err != nil {
					return domain.Message{}, err
				}
				if err := r1.SignalSession(ctx, conv.ID, Event{ID: "steer-cancel"}); err != nil {
					return domain.Message{}, err
				}
				return calls(call("tc-1", "task_create", map[string]any{
					"title":            "Unwanted task",
					"goal":             "Should never be created",
					"schedule_seconds": 300,
				})), nil
			case 2:
				// Process crashes during Step 1 (after steering superseded Step 0).
				cancelRun1()
				return domain.Message{}, context.Canceled
			case 3:
				return reply("Cancelled as requested, no task created."), nil
			default:
				return finish("done"), nil
			}
		},
	}

	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Restart on the same repo: recovery must resume at Step 1 and NEVER run task_create from Step 0.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	tasks, err := repo.FindTasks(ctx, conv.ID, "")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("restart executed discarded cached task_create after steering: %+v %v", tasks, err)
	}
	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Text != "Cancelled as requested, no task created." {
		t.Fatalf("unexpected reply after steering recovery: %+v %v", out, err)
	}
}

// S4 (task isolation): reapOrphanedSessions must never adopt an interrupted
// scheduled task's active session as the interactive chat session.
func TestReapOrphanedSessionsIgnoresTaskSessions(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))
	if err := repo.MarkIncomingHandled(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartSession(ctx, domain.Session{
		ID:             "task-sess-1",
		ConversationID: conv.ID,
		Status:         "active",
		Trigger:        "task",
		CreatedAt:      clk.Now().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := newTestRuntime(a, clk, 30*time.Minute, 6)
	r.runCtx = runCtx
	if err := r.reapOrphanedSessions(runCtx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.LoadCheckpoint(ctx, sessionRuntimeKey(conv.ID)); err != nil || ok {
		t.Fatalf("reapOrphanedSessions adopted task session into chat runtime checkpoint: ok=%v err=%v", ok, err)
	}
	cancel()
	r.Wait()
}

// S14: A /stop arriving across a restart while an interrupted turn key is
// saved must retire that turn number so the next question does not reuse the
// stopped turn's receipts.
func TestStopAcrossRestartRetiresInterruptedTurnNumber(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var step atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, _ []domain.Message) (domain.Message, error) {
			switch step.Add(1) {
			case 1:
				return reply("Old interrupted reply"), nil
			case 2:
				cancelRun1()
				return domain.Message{}, context.Canceled
			case 3:
				return reply("Fresh answer to new question"), nil
			default:
				return finish("done"), nil
			}
		},
	}

	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Across restart, user sends /stop followed by a brand-new question.
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID: "stop-after-restart", Conversation: conv, Command: "stop", Text: "/stop", CreatedAt: clk.Now().Add(time.Second),
	}, 2); err != nil {
		t.Fatal(err)
	}
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})

	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID: "fresh-question", Conversation: conv, Text: "New question", CreatedAt: clk.Now().Add(2 * time.Second),
	}, 3); err != nil {
		t.Fatal(err)
	}
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundFresh := false
	for _, m := range out {
		if m.Text == "Fresh answer to new question" {
			foundFresh = true
		}
	}
	if !foundFresh {
		t.Fatalf("new question after /stop reused old receipts instead of answering: %+v", out)
	}
}

// S15: Crash after Finish saves turn-done/<key> but before the actor advances
// NextTurn must reconcile turn-done so the next message starts a fresh turn.
func TestCrashAfterTurnDoneReconcilesBeforeNextMessage(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	// Run turn 0 cleanly to completion, then rewind the runtime checkpoint to
	// simulate crashing after Finish wrote turn-done/<key> and before NextTurn++.
	a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 0 {
			return reply("Reply zero"), nil
		}
		return reply("Reply one"), nil
	}}
	runCtx1, cancelRun1 := context.WithCancel(ctx)
	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun1()
	r1.Wait()

	saved, ok, err := r1.loadSessionRuntime(ctx, conv.ID)
	if err != nil || !ok {
		t.Fatalf("missing session runtime: ok=%v err=%v", ok, err)
	}
	// Simulate crash right after Finish wrote turn-done for turn 0.
	stale := sessionRuntimeState{
		SessionID:      saved.SessionID,
		NextTurn:       0,
		ActiveTurnKey:  agent.ID(saved.SessionID, "0"),
		ActiveIncoming: []string{"first"},
	}
	if err := r1.saveSessionRuntime(ctx, conv.ID, stale); err != nil {
		t.Fatal(err)
	}

	// Brand-new message arrives after restart.
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID: "second-msg", Conversation: conv, Text: "Second message", CreatedAt: clk.Now().Add(time.Second),
	}, 2); err != nil {
		t.Fatal(err)
	}
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 2 || out[1].Text != "Reply one" {
		t.Fatalf("second message was stranded by stale turn-done checkpoint: %+v %v", out, err)
	}
}

// S16: A one-shot task that fails and persists turn-done before a crash must
// remain "failed" (never become "completed") when recovered before AdvanceTask.
func TestOneShotTaskPreservesFailureAcrossCrashBetweenFinishAndAdvance(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC))

	task := domain.Task{
		ID:             "oneshot-fail-crash",
		ConversationID: conv.ID,
		Title:          "Failing one-shot",
		Goal:           "Do something that fails",
		Status:         "waiting",
		NextRunAt:      clk.Now(),
		CreatedAt:      clk.Now(),
		UpdatedAt:      clk.Now(),
	}
	if _, err := repo.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// First run: fail the model call and run Finish + CloseSession (saving turn-done
	// with FinishReason="failed"), simulating a crash right before AdvanceTask.
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		return domain.Message{}, domain.NewNonRetryableError("permanent model failure", "model_bad_request", errors.New("400"))
	}}
	rt1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	begun, err := rt1.actBeginTask(ctx, conv.ID, task.ID, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	session := agent.ID("task-run", task.ID, begun.NextRunAt.UTC().Format(time.RFC3339Nano))
	turn, runErr := rt1.runTurn(ctx, agent.RunInput{
		Key: session, SessionID: session, ConversationID: conv.ID, TaskID: task.ID,
	}, 6)
	if runErr == nil {
		t.Fatal("expected runTurn to fail")
	}
	if err := rt1.actFinish(ctx, finishInput(turn, runErr)); err != nil {
		t.Fatal(err)
	}
	if err := rt1.actCloseSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	// Crash here — AdvanceTask has NOT run yet; task is still "running".

	// Restart recovers running tasks and replays RunTaskOnce.
	rt2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rt2.recoverRunningTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt2.RunTaskOnce(ctx, TaskInput{ConversationID: conv.ID, ID: task.ID}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := repo.Task(ctx, conv.ID, task.ID)
	if err != nil || !ok || got.Status != "failed" {
		t.Fatalf("one-shot task lost failure outcome across crash before AdvanceTask: %+v %v", got, err)
	}
}

// S1 (offline steering): If a follow-up cancelling an action arrives while the
// service is down (after model step 0 cached task_create), recovery must
// supersede step 0 before dispatching its cached tool call.
func TestOfflineSteeringMessageSupersedesCachedModelActionOnRestart(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var callsCount atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, _ []domain.Message) (domain.Message, error) {
			if callsCount.Add(1) == 1 {
				return calls(call("tc-offline", "task_create", map[string]any{
					"title":            "Unwanted offline task",
					"goal":             "Must not run",
					"schedule_seconds": 300,
				})), nil
			}
			return reply("Okay, skipped creating the task."), nil
		},
	}

	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	// Crash right before task_create executes (after model/0 was cached and assistant persisted).
	r1.hooks = &activityHooks{
		toolCall: func(_ context.Context, _ agent.ToolInput, _ func(context.Context, agent.ToolInput) (string, error)) (string, error) {
			cancelRun1()
			return "", context.Canceled
		},
	}
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// While the service is down, user sends a follow-up cancelling the task.
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID:           "offline-cancel",
		Conversation: conv,
		Text:         "Cancel that, do not create the task!",
		CreatedAt:    clk.Now().Add(time.Second),
	}, 2); err != nil {
		t.Fatal(err)
	}

	// Restart: recovery must advance past cached step 0 and never run task_create.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		return reply("Okay, skipped creating the task."), nil
	}}
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	tasks, err := repo.FindTasks(ctx, conv.ID, "")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("offline steering did not supersede cached task_create: %+v %v", tasks, err)
	}
	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Text != "Okay, skipped creating the task." {
		t.Fatalf("unexpected reply after offline steering recovery: %+v %v", out, err)
	}
}

// S15 (reap): If a failed turn wrote turn-done and the service stayed down
// past the idle deadline, startup reaping must deliver the pending failure
// notice before expiring the session.
func TestReapOrphanedSessionsDeliversPendingFailureNoticeBeforeClosing(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	sessID := "failed-sess"
	turnKey := agent.ID(sessID, "0")
	if err := repo.StartSession(ctx, domain.Session{
		ID: sessID, ConversationID: conv.ID, Status: "active", Trigger: "message", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate Finish having saved turn-done with FinishReason="failed" and
	// marked "first" handled, then crashing before NotifyFailure ran.
	if err := a.Finish(ctx, agent.FinishInput{
		Turn: agent.Turn{
			Input: agent.RunInput{Key: turnKey, SessionID: sessID, ConversationID: conv.ID, IncomingIDs: []string{"first"}},
			Stats: agent.TurnStats{FinishReason: "failed"},
		},
		Failed:      true,
		FailureCode: "model_error",
	}); err != nil {
		t.Fatal(err)
	}
	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := r1.saveSessionRuntime(ctx, conv.ID, sessionRuntimeState{
		SessionID:      sessID,
		NextTurn:       0,
		ActiveTurnKey:  turnKey,
		ActiveIncoming: []string{"first"},
	}); err != nil {
		t.Fatal(err)
	}

	// Service stays down for 2 hours (past 30-minute idle timeout).
	clk.Advance(2 * time.Hour)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := r2.reapOrphanedSessions(ctx); err != nil {
		t.Fatal(err)
	}

	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 {
		t.Fatalf("expected failure notice delivered before reaping expired session, got %+v %v", out, err)
	}
	s, _, _ := repo.Session(ctx, sessID)
	if s.Status != "closed" {
		t.Fatalf("expected expired session closed after failure notice, got %+v", s)
	}
}

// S18 + S19: Restarting midway through a multi-tool batch (after artifact_read
// completed, before the second tool ran) must rebuild the complete assistant +
// tool transcript and reconstruct the pending image view for the next model step.
func TestMidToolBatchRecoveryRebuildsTranscriptAndImageViews(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	// Save a valid 1x1 PNG artifact so artifact_read produces an image view.
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d,
		0xb0, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
	if err := repo.SaveArtifact(ctx, domain.Artifact{
		ID:             "photo-1",
		ConversationID: conv.ID,
		Name:           "photo.png",
		MIME:           "image/png",
		Data:           pngBytes,
	}); err != nil {
		t.Fatal(err)
	}

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var sawCompleteExchange atomic.Bool
	var sawImageView atomic.Bool
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(n int, msgs []domain.Message) (domain.Message, error) {
			if n == 0 {
				return calls(
					call("c1", "artifact_read", map[string]string{"id": "photo-1"}),
					call("c2", "task_find", map[string]string{}),
				), nil
			}
			// Step 1 on restart: verify assistant(c1,c2) + tool(c1) + tool(c2) + image view are ALL present!
			hasAsst, hasC1, hasC2, hasImg := false, false, false, false
			for _, m := range msgs {
				if m.Role == "assistant" && len(m.ToolCalls) == 2 && m.ToolCalls[0].ID == "c1" && m.ToolCalls[1].ID == "c2" {
					hasAsst = true
				}
				if m.Role == "tool" && m.ToolCallID == "c1" {
					hasC1 = true
				}
				if m.Role == "tool" && m.ToolCallID == "c2" {
					hasC2 = true
				}
				if m.Role == "user" && len(m.Images) > 0 {
					hasImg = true
				}
			}
			if hasAsst && hasC1 && hasC2 {
				sawCompleteExchange.Store(true)
			}
			if hasImg {
				sawImageView.Store(true)
			}
			if n == 1 {
				return reply("Saw the photo and tasks!"), nil
			}
			return finish("done"), nil
		},
	}

	// Run 1: let c1 (artifact_read) complete, then crash right when c2 (task_find) starts.
	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	r1.hooks = &activityHooks{
		toolCall: func(tCtx context.Context, in agent.ToolInput, next func(context.Context, agent.ToolInput) (string, error)) (string, error) {
			if in.Call.ID == "c2" {
				cancelRun1()
				return "", context.Canceled
			}
			return next(tCtx, in)
		},
	}
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Run 2: restart and complete c2 + step 1.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	if !sawCompleteExchange.Load() {
		t.Fatal("S18: step 1 after mid-batch recovery did not receive complete assistant+tool1+tool2 exchange")
	}
	if !sawImageView.Load() {
		t.Fatal("S19: step 1 after mid-batch recovery lost artifact_read image view")
	}
}

// S20: Steering during the reserved finalization step persists ActiveStep > maxSteps.
// Restarting must NOT re-enter finalizeBudget(maxSteps) or reuse the discarded reply.
func TestRecoveryPastFinalizationStepDoesNotReenterFinalizeBudget(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	sessID := "fin-sess"
	turnKey := agent.ID(sessID, "0")
	if err := repo.StartSession(ctx, domain.Session{
		ID: sessID, ConversationID: conv.ID, Status: "active", Trigger: "message", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	r := newTestRuntime(a, clk, 30*time.Minute, 2)
	// Simulate crash after steering superseded finalizeBudget(step=2) -> ActiveStep=3 (> maxSteps=2).
	var finalizeCalls atomic.Int32
	a.Model = &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		finalizeCalls.Add(1)
		return reply("Discarded finalization reply"), nil
	}}
	progress := &turnProgress{StartStep: 3}
	_, runErr := r.runTurnWithProgress(ctx, agent.RunInput{
		Key: turnKey, SessionID: sessID, ConversationID: conv.ID, IncomingIDs: []string{"first"},
	}, 2, progress)
	if runErr == nil || domain.ErrorCode(runErr) != "step_limit" {
		t.Fatalf("expected step_limit error when resuming past finalization, got %v", runErr)
	}
	if finalizeCalls.Load() != 0 {
		t.Fatalf("finalizeBudget should not have been called when StartStep > maxSteps (got %d calls)", finalizeCalls.Load())
	}
}

// S21: Cumulative TurnStats (ModelCalls, ToolCalls, tokens) must be restored
// across restarts without resetting or recounting completed work.
func TestTurnStatsPreservedAcrossCrashRecovery(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var step atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, _ []domain.Message) (domain.Message, error) {
			switch step.Add(1) {
			case 1:
				return calls(call("t1", "task_find", map[string]string{})), nil
			case 2:
				// Crash at start of step 1 (after step 0 completed 1 model call + 1 tool call).
				cancelRun1()
				return domain.Message{}, context.Canceled
			case 3:
				return reply("Completed after crash"), nil
			default:
				return finish("done"), nil
			}
		},
	}

	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	saved, ok, err := r1.loadSessionRuntime(ctx, conv.ID)
	if err != nil || !ok {
		t.Fatalf("missing saved runtime state: ok=%v err=%v", ok, err)
	}
	if saved.ActiveStep != 1 || saved.ActiveStats.ModelCalls != 1 || saved.ActiveStats.ToolCalls != 1 {
		t.Fatalf("expected ActiveStep=1 ModelCalls=1 ToolCalls=1 in checkpoint, got %+v", saved)
	}

	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	raw, ok, err := repo.LoadCheckpoint(ctx, "turn-done/"+saved.ActiveTurnKey)
	if err != nil || !ok {
		t.Fatalf("missing turn-done checkpoint: ok=%v err=%v", ok, err)
	}
	var doneTurn agent.Turn
	if err := json.Unmarshal(raw, &doneTurn); err != nil {
		t.Fatal(err)
	}
	// Step 0: 1 model + 1 tool (task_find); Step 1: 1 model + 1 tool (send_message); Step 2: 1 model (finish).
	if doneTurn.Stats.ModelCalls != 3 || doneTurn.Stats.ToolCalls != 2 {
		t.Fatalf("expected cumulative stats ModelCalls=3 ToolCalls=2 across restart, got %+v", doneTurn.Stats)
	}
}

// S1 (pre-model steering during restart preparation) + S21 (cached usage counted):
// A follow-up arriving during restart Prepare must supersede any cached model
// output at step 0 instead of replaying it, AND must still account for the
// cached model call's usage in TurnStats.
func TestPreModelSteeringDuringRestartPrepSupersedesCachedOutputAndCountsUsage(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	sessID := "prep-steer-sess"
	turnKey := agent.ID(sessID, "0")
	if err := repo.StartSession(ctx, domain.Session{
		ID: sessID, ConversationID: conv.ID, Status: "active", Trigger: "message", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Seed a cached model/0 output that would queue an obsolete reply (crash
	// happened right after ModelCall saved its checkpoint, before ActiveAfterModel).
	cachedOut := agent.ModelOutput{
		Message:      reply("Obsolete cached reply"),
		InputTokens:  10,
		OutputTokens: 5,
	}
	rawCached, _ := json.Marshal(cachedOut)
	if err := repo.SaveCheckpoint(ctx, fmt.Sprintf("model/%s/0", turnKey), rawCached); err != nil {
		t.Fatal(err)
	}
	rSeed := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rSeed.saveSessionRuntime(ctx, conv.ID, sessionRuntimeState{
		SessionID:      sessID,
		NextTurn:       0,
		ActiveTurnKey:  turnKey,
		ActiveIncoming: []string{"first"},
	}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	r := newTestRuntime(a, clk, 30*time.Minute, 6)
	r.runCtx = runCtx
	var injected atomic.Bool
	r.hooks = &activityHooks{
		prepare: func(pCtx context.Context, in agent.RunInput, next func(context.Context, agent.RunInput) (agent.Turn, error)) (agent.Turn, error) {
			turn, err := next(pCtx, in)
			if err == nil && injected.CompareAndSwap(false, true) {
				// Deliver cancellation follow-up right during restart Prepare.
				_, _ = repo.AcceptIncoming(ctx, domain.Incoming{
					ID: "cancel-during-prep", Conversation: conv, Text: "Stop, send the updated answer instead.", CreatedAt: clk.Now().Add(time.Second),
				}, 2)
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "cancel-during-prep"})
			}
			return turn, err
		},
	}
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(n int, _ []domain.Message) (domain.Message, error) {
			if n == 0 {
				return reply("Updated answer after prep steering"), nil
			}
			return finish("done"), nil
		},
	}
	if err := r.DispatchOnce(runCtx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun()
	r.Wait()

	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Text != "Updated answer after prep steering" {
		t.Fatalf("S1: pre-model steering replayed obsolete cached reply: %+v %v", out, err)
	}
	rawDone, ok, err := repo.LoadCheckpoint(ctx, "turn-done/"+turnKey)
	if err != nil || !ok {
		t.Fatalf("missing turn-done: ok=%v err=%v", ok, err)
	}
	var doneTurn agent.Turn
	if err := json.Unmarshal(rawDone, &doneTurn); err != nil {
		t.Fatal(err)
	}
	// Cached step 0 (10 in) + step 1 reply (10 in) + step 2 finish (10 in) = 3 calls, 30 input tokens.
	if doneTurn.Stats.ModelCalls != 3 || doneTurn.Stats.InputTokens != 30 {
		t.Fatalf("S21: superseded cached model usage was dropped: %+v", doneTurn.Stats)
	}
}

// S18 (crash after final tool persisted, before progress checkpoint):
// Replaying must NOT append a duplicate tool response for the same call ID.
func TestCrashAfterFinalToolPersistDoesNotDuplicateToolMessage(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	sessID := "dup-tool-sess"
	turnKey := agent.ID(sessID, "0")
	if err := repo.StartSession(ctx, domain.Session{
		ID: sessID, ConversationID: conv.ID, Status: "active", Trigger: "message", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	asstMsg := calls(call("only-call", "task_find", map[string]string{}))
	rawModel, _ := json.Marshal(agent.ModelOutput{Message: asstMsg, InputTokens: 10, OutputTokens: 5})
	if err := repo.SaveCheckpoint(ctx, fmt.Sprintf("model/%s/0", turnKey), rawModel); err != nil {
		t.Fatal(err)
	}
	// Persist BOTH assistant AND the single tool result in openalbert_messages,
	// simulating a crash right before saveProgress(0, true, 1).
	if err := repo.AppendMessage(ctx, sessID, fmt.Sprintf("%s/assistant/0", turnKey), asstMsg); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendMessage(ctx, sessID, fmt.Sprintf("%s/tool/0/only-call", turnKey), domain.Message{
		Role: "tool", Content: `{"tasks":[]}`, ToolCallID: "only-call",
	}); err != nil {
		t.Fatal(err)
	}
	// Also seed the cached tool receipt so executeTool doesn't re-run.
	if err := repo.SaveCheckpoint(ctx, "tool/"+agent.ID(turnKey, "0", "only-call"), []byte(`{"tasks":[]}`)); err != nil {
		t.Fatal(err)
	}

	var toolResponsesForCall atomic.Int32
	a.Model = &scriptedModel{run: func(_ int, msgs []domain.Message) (domain.Message, error) {
		count := int32(0)
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID == "only-call" {
				count++
			}
		}
		toolResponsesForCall.Store(count)
		return reply("All good"), nil
	}}
	r := newTestRuntime(a, clk, 30*time.Minute, 6)
	progress := &turnProgress{StartStep: 0, AfterModel: true, StartTool: 0, Stats: agent.TurnStats{ModelCalls: 1, InputTokens: 10, OutputTokens: 5}}
	if _, err := r.runTurnWithProgress(ctx, agent.RunInput{
		Key: turnKey, SessionID: sessID, ConversationID: conv.ID, IncomingIDs: []string{"first"},
	}, 6, progress); err != nil {
		t.Fatal(err)
	}
	if got := toolResponsesForCall.Load(); got != 1 {
		t.Fatalf("S18: expected exactly 1 tool response for only-call, got %d", got)
	}
}

// S19 (multi-turn artifact_read): Reading the same image artifact again in a
// second turn must append a fresh image view to the second turn's context.
func TestSecondTurnReadingSameArtifactAppendsFreshImageView(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d,
		0xb0, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
	if err := repo.SaveArtifact(ctx, domain.Artifact{
		ID: "shared-photo", ConversationID: conv.ID, Name: "photo.png", MIME: "image/png", Data: pngBytes,
	}); err != nil {
		t.Fatal(err)
	}

	var secondTurnImgCount atomic.Int32
	var step atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, msgs []domain.Message) (domain.Message, error) {
			switch step.Add(1) {
			case 1:
				return calls(call("read-t1", "artifact_read", map[string]string{"id": "shared-photo"})), nil
			case 2:
				return reply("Turn 1 saw photo"), nil
			case 3:
				return finish("turn 1 done"), nil
			case 4:
				return calls(call("read-t2", "artifact_read", map[string]string{"id": "shared-photo"})), nil
			case 5:
				// Count image messages appearing AFTER Turn 2's read-t2 tool response.
				afterReadT2 := false
				imgs := int32(0)
				for _, m := range msgs {
					if m.Role == "tool" && m.ToolCallID == "read-t2" {
						afterReadT2 = true
						continue
					}
					if afterReadT2 && m.Role == "user" && len(m.Images) > 0 {
						imgs++
					}
				}
				secondTurnImgCount.Store(imgs)
				return reply("Turn 2 saw fresh photo"), nil
			default:
				return finish("turn 2 done"), nil
			}
		},
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	r := newTestRuntime(a, clk, 30*time.Minute, 6)
	r.runCtx = runCtx
	if err := r.DispatchOnce(runCtx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})

	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID: "second-photo-ask", Conversation: conv, Text: "Look at the photo again", CreatedAt: clk.Now().Add(time.Second),
	}, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchOnce(runCtx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun()
	r.Wait()

	if got := secondTurnImgCount.Load(); got != 1 {
		t.Fatalf("S19: expected 1 fresh image view in turn 2 after reading same artifact again, got %d", got)
	}
}

// S1 (crash DURING steering preparation): When steering adds an ID while a
// cached model output exists at the current step, the merged IDs, cached usage,
// and advanced resume step must ALL be persisted in one update BEFORE prepareTurn.
func TestCrashDuringSteeringPreparePersistsAdvancedStepAndUsage(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	sessID := "crash-steer-prep"
	turnKey := agent.ID(sessID, "0")
	if err := repo.StartSession(ctx, domain.Session{
		ID: sessID, ConversationID: conv.ID, Status: "active", Trigger: "message", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rawCached, _ := json.Marshal(agent.ModelOutput{Message: reply("Obsolete cached"), InputTokens: 12, OutputTokens: 6})
	if err := repo.SaveCheckpoint(ctx, fmt.Sprintf("model/%s/0", turnKey), rawCached); err != nil {
		t.Fatal(err)
	}
	rSeed := newTestRuntime(a, clk, 30*time.Minute, 6)
	if err := rSeed.saveSessionRuntime(ctx, conv.ID, sessionRuntimeState{
		SessionID: sessID, NextTurn: 0, ActiveTurnKey: turnKey, ActiveIncoming: []string{"first"},
	}); err != nil {
		t.Fatal(err)
	}

	// Run 1: inject steering during first Prepare, then crash during second (steering) Prepare.
	runCtx1, cancelRun1 := context.WithCancel(ctx)
	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	var prepCalls atomic.Int32
	r1.hooks = &activityHooks{
		prepare: func(pCtx context.Context, in agent.RunInput, next func(context.Context, agent.RunInput) (agent.Turn, error)) (agent.Turn, error) {
			switch prepCalls.Add(1) {
			case 1:
				turn, err := next(pCtx, in)
				_, _ = repo.AcceptIncoming(ctx, domain.Incoming{
					ID: "cancel-prep-crash", Conversation: conv, Text: "Cancel obsolete reply!", CreatedAt: clk.Now().Add(time.Second),
				}, 2)
				_ = r1.SignalSession(ctx, conv.ID, Event{ID: "cancel-prep-crash"})
				return turn, err
			default:
				cancelRun1()
				return agent.Turn{}, context.Canceled
			}
		},
	}
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Verify checkpoint already advanced to step 1 with merged IDs and cached model usage!
	saved, ok, err := r1.loadSessionRuntime(ctx, conv.ID)
	if err != nil || !ok {
		t.Fatalf("missing saved runtime: ok=%v err=%v", ok, err)
	}
	if saved.ActiveStep != 1 || saved.ActiveStats.ModelCalls != 1 || saved.ActiveStats.InputTokens != 12 || len(saved.ActiveIncoming) != 2 {
		t.Fatalf("S1: steering did not atomically persist step=1 + cached usage before Prepare: %+v", saved)
	}

	// Run 2: restart must NOT replay obsolete cached reply at step 0.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(n int, _ []domain.Message) (domain.Message, error) {
			if n == 0 {
				return reply("Fresh post-crash answer"), nil
			}
			return finish("done"), nil
		},
	}
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Text != "Fresh post-crash answer" {
		t.Fatalf("S1: restart replayed obsolete cached reply after steering-prepare crash: %+v %v", out, err)
	}
}

// S18 (Codemode): Crash after persisting final Codemode tool response (after
// send_message queued a reply) and before saving step progress must replay
// durable child receipts to restore Sent=true, ToolCalls=1, and FinishReason,
// without duplicating the tool message or the queued reply.
func TestCodemodeCrashAfterFinalToolPersistDoesNotDuplicateToolMessage(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Codemode = true
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	// Run 1: execute a real Codemode script that queues one reply via send_message,
	// then simulate a crash right after the final Codemode tool response was
	// persisted in openalbert_messages and before step 1's model call / checkpoint.
	deadlineMs := clk.Now().Add(5 * time.Minute).UnixMilli()
	runCtx1, cancelRun1 := context.WithCancel(ctx)
	var step atomic.Int32
	var toolResponses atomic.Int32
	a.Model = &scriptedModel{
		continueAfterReply: true,
		run: func(_ int, msgs []domain.Message) (domain.Message, error) {
			switch step.Add(1) {
			case 1:
				return calls(call("cm-1", agent.CodemodeName, map[string]string{
					"code": fmt.Sprintf(`if (Date.now() <= %d) { await tools.send_message({text: "Codemode reply once"}); } return {ok: true};`, deadlineMs),
				})), nil
			case 2:
				// Crash at the start of step 1 (after cm-1 finished and persisted its tool message).
				cancelRun1()
				return domain.Message{}, context.Canceled
			default:
				count := int32(0)
				for _, m := range msgs {
					if m.Role == "tool" && m.ToolCallID == "cm-1" {
						count++
					}
				}
				toolResponses.Store(count)
				return finish("codemode ok"), nil
			}
		},
	}

	r1 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r1.runCtx = runCtx1
	if err := r1.DispatchOnce(runCtx1); err != nil {
		t.Fatal(err)
	}
	r1.Wait()

	// Rewind sessionRuntimeState to step=0, ActiveAfterModel=true, ActiveSent=false,
	// ActiveStats.ToolCalls=0 — the exact state if the crash fell between
	// persistCodemodeOutput and saveProgress(1, false, 0, ...).
	saved, ok, err := r1.loadSessionRuntime(ctx, conv.ID)
	if err != nil || !ok {
		t.Fatalf("missing saved runtime: ok=%v err=%v", ok, err)
	}
	saved.ActiveStep = 0
	saved.ActiveAfterModel = true
	saved.ActiveToolIndex = 0
	saved.ActiveSent = false
	saved.ActiveStats = agent.TurnStats{ModelCalls: 1, InputTokens: 10, OutputTokens: 5}
	if err := r1.saveSessionRuntime(ctx, conv.ID, saved); err != nil {
		t.Fatal(err)
	}

	// Advance the clock past the script's 5-minute deadline before restart so
	// replay must reuse the persisted script start timestamp rather than r.now().
	clk.Advance(time.Hour)

	// Run 2: restart and finish turn.
	runCtx2, cancelRun2 := context.WithCancel(ctx)
	r2 := newTestRuntime(a, clk, 30*time.Minute, 6)
	r2.runCtx = runCtx2
	if err := r2.DispatchOnce(runCtx2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		p, _ := repo.PendingIncoming(ctx, 10)
		return len(p) == 0
	})
	cancelRun2()
	r2.Wait()

	if got := toolResponses.Load(); got != 1 {
		t.Fatalf("S18 (Codemode): expected exactly 1 tool response for cm-1, got %d", got)
	}
	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 || out[0].Text != "Codemode reply once" {
		t.Fatalf("S18 (Codemode): expected exactly 1 queued reply, got %+v %v", out, err)
	}
	rawDone, ok, err := repo.LoadCheckpoint(ctx, "turn-done/"+saved.ActiveTurnKey)
	if err != nil || !ok {
		t.Fatalf("missing turn-done checkpoint: ok=%v err=%v", ok, err)
	}
	var doneTurn agent.Turn
	if err := json.Unmarshal(rawDone, &doneTurn); err != nil {
		t.Fatal(err)
	}
	if !doneTurn.Sent || doneTurn.Stats.ToolCalls != 1 || doneTurn.Stats.FinishReason != "reply_queued" {
		t.Fatalf("S18 (Codemode): lost Sent/ToolCalls/FinishReason on replay: sent=%v stats=%+v", doneTurn.Sent, doneTurn.Stats)
	}
}

// S21 (finalization steering): When steering supersedes a cached finalization
// model output, the saved progress checkpoint must carry the updated ModelCalls
// and tokens from finalizeBudget's turn copy.
func TestFinalizeBudgetSteeringPersistsCachedModelUsageInCheckpoint(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	clk := NewFakeClock(time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC))

	sessID := "fin-stats-sess"
	turnKey := agent.ID(sessID, "0")
	if err := repo.StartSession(ctx, domain.Session{
		ID: sessID, ConversationID: conv.ID, Status: "active", Trigger: "message", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Seed cached finalize model output at step = maxSteps = 1.
	rawFin, _ := json.Marshal(agent.ModelOutput{Message: reply("Obsolete final"), InputTokens: 17, OutputTokens: 9})
	if err := repo.SaveCheckpoint(ctx, fmt.Sprintf("model/%s/1/finalize", turnKey), rawFin); err != nil {
		t.Fatal(err)
	}

	var savedStep int
	var savedStats agent.TurnStats
	r := newTestRuntime(a, clk, 30*time.Minute, 1)
	progress := &turnProgress{
		StartStep: 1, // enters finalizeBudget(step=1) directly
		Stats:     agent.TurnStats{ModelCalls: 1, InputTokens: 10, OutputTokens: 5},
		OnUpdate: func(_ context.Context, step int, _ bool, _ int, _ bool, stats agent.TurnStats) error {
			savedStep = step
			savedStats = cloneTurnStats(stats)
			return nil
		},
	}
	_, runErr := r.runTurnWithProgress(ctx, agent.RunInput{
		Key: turnKey, SessionID: sessID, ConversationID: conv.ID, IncomingIDs: []string{"first"},
	}, 1, progress, func(turn *agent.Turn, _ int, _ func() error) error {
		turn.Input.IncomingIDs = append(turn.Input.IncomingIDs, "fin-steer-msg")
		return nil
	})
	if runErr == nil || domain.ErrorCode(runErr) != "step_limit" {
		t.Fatalf("expected step_limit after finalize steering, got %v", runErr)
	}
	if savedStep != 2 || savedStats.ModelCalls != 2 || savedStats.InputTokens != 27 || savedStats.OutputTokens != 14 {
		t.Fatalf("S21: finalize steering checkpoint lost cached usage: step=%d stats=%+v", savedStep, savedStats)
	}
}
