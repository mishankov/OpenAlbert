package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestT3StoreContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { t3StoreContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL to test PostgreSQL persistence")
		}
		s, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		t3StoreContract(t, s)
	})
}

func t3StoreContract(t *testing.T, s domain.T3Store) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprintf("t3-%d", time.Now().UnixNano())
	envA := nonce + "-env-a"
	envB := nonce + "-env-b"
	t0 := time.Now().UTC().Truncate(time.Microsecond)

	// 1. Hosts upsert, re-register preserves host_id and created_at, lookup by both IDs.
	h1, err := s.UpsertT3Host(ctx, domain.T3Host{
		HostID:        nonce + "-host-a",
		EnvironmentID: envA,
		Label:         "MacBook Air",
		Projects:      []domain.T3Project{{ID: "proj-1", Title: "OpenAlbert", WorkspaceRoot: "/code/oa"}},
		Providers: []domain.T3Provider{{
			ProviderInstanceID: "ag-1",
			DriverKind:         "antigravity",
			DisplayName:        "Antigravity",
			Models: []domain.T3Model{{
				ID:    "gemini-4-neon",
				Label: "Gemini 4 Neon",
				Options: []domain.T3ModelOption{{
					ID:      "effort",
					Values:  []any{"low", "high"},
					Default: "high",
				}},
			}},
		}},
		LastSeenAt: t0,
		CreatedAt:  t0,
		UpdatedAt:  t0,
	})
	if err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	if h1.HostID != nonce+"-host-a" || len(h1.Projects) != 1 || len(h1.Providers) != 1 {
		t.Fatalf("unexpected host: %+v", h1)
	}

	t1 := t0.Add(30 * time.Second)
	h1Updated, err := s.UpsertT3Host(ctx, domain.T3Host{
		HostID:        nonce + "-different-host-id-should-be-ignored",
		EnvironmentID: envA,
		Label:         "MacBook Air M4",
		Projects:      []domain.T3Project{{ID: "proj-1", Title: "OpenAlbert", WorkspaceRoot: "/code/oa"}},
		Providers:     []domain.T3Provider{},
		LastSeenAt:    t1,
		CreatedAt:     t1,
		UpdatedAt:     t1,
	})
	if err != nil {
		t.Fatalf("re-register host: %v", err)
	}
	if h1Updated.HostID != h1.HostID || !h1Updated.CreatedAt.Equal(h1.CreatedAt) || h1Updated.Label != "MacBook Air M4" {
		t.Fatalf("re-register did not preserve identity: %+v vs %+v", h1Updated, h1)
	}

	byHostID, found, err := s.T3Host(ctx, h1.HostID)
	if err != nil || !found || byHostID.EnvironmentID != envA {
		t.Fatalf("lookup by host_id failed: %+v %v %v", byHostID, found, err)
	}
	byEnvID, found, err := s.T3Host(ctx, envA)
	if err != nil || !found || byEnvID.HostID != h1.HostID {
		t.Fatalf("lookup by environment_id failed: %+v %v %v", byEnvID, found, err)
	}

	t2 := t1.Add(30 * time.Second)
	touched, found, err := s.TouchT3Host(ctx, envA, t2)
	if err != nil || !found || !touched.LastSeenAt.Equal(t2) {
		t.Fatalf("touch host: %+v %v %v", touched, found, err)
	}
	if _, found, err := s.TouchT3Host(ctx, nonce+"-missing", t2); err != nil || found {
		t.Fatalf("missing touch: found=%v err=%v", found, err)
	}

	// 2. Commands: idempotent create, FIFO exact-once claim, redelivery & requeue, atomic complete.
	c1, created, err := s.CreateT3Command(ctx, domain.T3Command{
		ID:            nonce + "-cmd-1",
		EnvironmentID: envA,
		Action:        domain.T3ActionSyncCapabilities,
		Payload:       json.RawMessage(`{}`),
		CreatedAt:     t0,
		UpdatedAt:     t0,
	})
	if err != nil || !created || c1.Status != domain.T3CommandStatusPending {
		t.Fatalf("create cmd1: %+v created=%v err=%v", c1, created, err)
	}
	_, createdDup, err := s.CreateT3Command(ctx, domain.T3Command{
		ID:            nonce + "-cmd-1",
		EnvironmentID: envA,
		Action:        domain.T3ActionSyncCapabilities,
		Payload:       json.RawMessage(`{"dup":true}`),
		CreatedAt:     t1,
		UpdatedAt:     t1,
	})
	if err != nil || createdDup {
		t.Fatalf("duplicate create should be idempotent: created=%v err=%v", createdDup, err)
	}
	c2, _, err := s.CreateT3Command(ctx, domain.T3Command{
		ID:            nonce + "-cmd-2",
		EnvironmentID: envA,
		Action:        domain.T3ActionThreadSend,
		Payload:       json.RawMessage(`{"thread_id":"th-1","message":"hi"}`),
		CreatedAt:     t1,
		UpdatedAt:     t1,
	})
	if err != nil {
		t.Fatalf("create cmd2: %v", err)
	}
	if _, _, err := s.CreateT3Command(ctx, domain.T3Command{
		ID:            nonce + "-cmd-b",
		EnvironmentID: envB,
		Action:        domain.T3ActionSyncCapabilities,
		CreatedAt:     t1,
		UpdatedAt:     t1,
	}); err != nil {
		t.Fatalf("create cmdB: %v", err)
	}

	// Concurrent claimers must claim each envA pending command at most once.
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimedAll := []domain.T3Command{}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimPendingT3Commands(ctx, envA, 10, t2, time.Time{})
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			claimedAll = append(claimedAll, got...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(claimedAll) != 2 {
		t.Fatalf("expected 2 claimed commands across concurrent claimers, got %d: %+v", len(claimedAll), claimedAll)
	}
	if claimedAll[0].Status != domain.T3CommandStatusDispatched || claimedAll[1].Status != domain.T3CommandStatusDispatched {
		t.Fatalf("claimed commands should be dispatched: %+v", claimedAll)
	}

	// Unacknowledged dispatched commands are recoverable after redelivery threshold under same IDs.
	t3 := t2.Add(time.Minute)
	redelivered, err := s.ClaimPendingT3Commands(ctx, envA, 10, t3, t2)
	if err != nil || len(redelivered) != 2 || redelivered[0].ID != c1.ID || redelivered[1].ID != c2.ID {
		t.Fatalf("expected redelivery of unacked commands in FIFO order, got %+v err=%v", redelivered, err)
	}

	// RequeueT3Command restores a single dispatched command to pending.
	if err := s.RequeueT3Command(ctx, c1.ID, t3); err != nil {
		t.Fatalf("requeue cmd1: %v", err)
	}
	gotRequeued, err := s.ClaimPendingT3Commands(ctx, envA, 10, t3.Add(time.Second), time.Time{})
	if err != nil || len(gotRequeued) != 1 || gotRequeued[0].ID != c1.ID {
		t.Fatalf("expected requeued cmd1, got %+v err=%v", gotRequeued, err)
	}

	// Wrong environment rejected.
	if _, _, _, _, err := s.CompleteT3Command(ctx, c2.ID, envB, true, json.RawMessage(`{"ok":true}`), "", t3, nil); !errors.Is(err, domain.ErrT3EnvironmentMismatch) {
		t.Fatalf("expected ErrT3EnvironmentMismatch, got %v", err)
	}
	// Atomic rollback if thread effect fails: command stays dispatched.
	if _, _, _, _, err := s.CompleteT3Command(ctx, c2.ID, envA, true, json.RawMessage(`{}`), "", t3, func(domain.T3Command) *domain.T3CommandThreadEffect {
		return &domain.T3CommandThreadEffect{Patch: &domain.T3ThreadPatch{ThreadID: "", EnvironmentID: envA}}
	}); err == nil {
		t.Fatal("expected error when thread patch has empty thread_id")
	}
	stillDispatched, _, _ := s.T3Command(ctx, c2.ID)
	if stillDispatched.Status != domain.T3CommandStatusDispatched {
		t.Fatalf("command should remain dispatched after failed atomic thread update, got %q", stillDispatched.Status)
	}

	completed, _, found, transitioned, err := s.CompleteT3Command(ctx, c2.ID, envA, true, json.RawMessage(`{"delivered":true}`), "", t3, nil)
	if err != nil || !found || !transitioned || completed.Status != domain.T3CommandStatusSucceeded {
		t.Fatalf("complete cmd2: %+v found=%v transitioned=%v err=%v", completed, found, transitioned, err)
	}
	// Duplicate completion preserves original terminal result and reports transitioned=false.
	completedAgain, _, found, transitionedAgain, err := s.CompleteT3Command(ctx, c2.ID, envA, false, nil, "late error", t3.Add(time.Second), func(domain.T3Command) *domain.T3CommandThreadEffect {
		t.Error("buildEffect must not run on duplicate completion")
		return nil
	})
	if err != nil || !found || transitionedAgain || completedAgain.Status != domain.T3CommandStatusSucceeded {
		t.Fatalf("duplicate complete should keep terminal status: %+v found=%v transitioned=%v err=%v", completedAgain, found, transitionedAgain, err)
	}

	// 3. Threads upsert merge, host-event clock vs server clock (S6), out-of-order protection, pending_request clear, filters, wake tracking, owner replay isolation (S8).
	thID := nonce + "-thread-1"
	// Launch result uses server clock t3 (> host event time t2) without setting last_event_at.
	th, err := s.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:           thID,
		EnvironmentID:      envA,
		ConversationID:     "personal:7",
		TaskID:             "task-1",
		ProjectID:          "proj-1",
		Title:              "PR B",
		Link:               "t3://thread/" + thID,
		ProviderInstanceID: "ag-1",
		Model:              "gemini-4-neon",
		Branch:             "feat/t3",
		Status:             "running",
		At:                 t3,
	})
	if err != nil {
		t.Fatalf("upsert thread: %v", err)
	}
	if th.HostID != h1.HostID || th.ConversationID != "personal:7" {
		t.Fatalf("unexpected thread: %+v", th)
	}

	// Host emits thread_waiting_input with occurred_at=t2 (< server launch time t3):
	// since no host event has been applied yet, this IS the newest host event and MUST apply!
	// Also embed spoofed conversation_id/task_id in rawPayload to prove replay strips them (S8).
	ev1 := domain.T3Event{
		EventID:       nonce + "-ev-1",
		EnvironmentID: envA,
		ThreadID:      thID,
		Kind:          domain.T3EventThreadWaitingInput,
		Status:        "waiting_input",
		PendingRequest: &domain.T3PendingRequest{
			RequestID: "req-1",
			Questions: []json.RawMessage{json.RawMessage(`{"question":"Ship it?"}`)},
		},
		OccurredAt: t2,
	}
	spoofRaw := json.RawMessage(`{"event_id":"` + ev1.EventID + `","kind":"thread_waiting_input","conversation_id":"personal:999","task_id":"evil-task"}`)
	thWait, dup, stored1, err := s.RecordT3EventAndUpsertThread(ctx, ev1, spoofRaw, domain.T3ThreadPatch{
		ThreadID:           thID,
		EnvironmentID:      envA,
		ProjectID:          "proj-2",
		Link:               "t3://thread/new-link",
		ProviderInstanceID: "codex-2",
		Model:              "gpt-6.2-astra",
		Status:             "waiting_input",
		RequireCurrent:     true,
		SetPendingRequest:  true,
		PendingRequest:     ev1.PendingRequest,
		LastSummary:        "Waiting for approval",
		At:                 t2,
	}, t3)
	if err != nil || dup || stored1.WokenAt != nil {
		t.Fatalf("record event 1: dup=%v stored=%+v err=%v", dup, stored1, err)
	}
	if thWait.Status != "waiting_input" || thWait.ConversationID != "personal:7" || thWait.Title != "PR B" || thWait.Model != "gpt-6.2-astra" || thWait.PendingRequest == nil || thWait.PendingRequest.RequestID != "req-1" {
		t.Fatalf("first host event (occurred_at < server updated_at) must apply cleanly: %+v", thWait)
	}

	// Out-of-order delayed thread_progress (t1 < last_event_at=t2) MUST be recorded for dedup
	// but MUST NOT overwrite waiting_input status, pending_request, model, provider, project, or link!
	evDelayed := domain.T3Event{
		EventID:       nonce + "-ev-delayed",
		EnvironmentID: envA,
		ThreadID:      thID,
		Kind:          domain.T3EventThreadProgress,
		Status:        "running",
		Summary:       "Older progress",
		OccurredAt:    t1,
	}
	thAfterDelayed, dupDelayed, _, err := s.RecordT3EventAndUpsertThread(ctx, evDelayed, json.RawMessage(`{"occurred_at":""}`), domain.T3ThreadPatch{
		ThreadID:           thID,
		EnvironmentID:      envA,
		ProjectID:          "proj-old",
		Link:               "t3://thread/old-link",
		ProviderInstanceID: "ag-old",
		Model:              "gemini-old",
		Status:             "running",
		RequireCurrent:     true,
		SetPendingRequest:  true,
		PendingRequest:     nil,
		LastSummary:        "Older progress",
		At:                 t1,
	}, t3.Add(time.Second))
	if err != nil || dupDelayed {
		t.Fatalf("record delayed event: dup=%v err=%v", dupDelayed, err)
	}
	if thAfterDelayed.Status != "waiting_input" || thAfterDelayed.PendingRequest == nil || thAfterDelayed.PendingRequest.RequestID != "req-1" ||
		thAfterDelayed.LastSummary != "Waiting for approval" || thAfterDelayed.Model != "gpt-6.2-astra" ||
		thAfterDelayed.ProviderInstanceID != "codex-2" || thAfterDelayed.ProjectID != "proj-2" || thAfterDelayed.Link != "t3://thread/new-link" {
		t.Fatalf("delayed event overwrote newer thread state/metadata: %+v", thAfterDelayed)
	}
	// Replaying delayed event deduplicates.
	_, dupDelayed2, _, err := s.RecordT3EventAndUpsertThread(ctx, evDelayed, json.RawMessage(`{}`), domain.T3ThreadPatch{
		ThreadID:      thID,
		EnvironmentID: envA,
		At:            t1,
	}, t3.Add(2*time.Second))
	if err != nil || !dupDelayed2 {
		t.Fatalf("replayed delayed event must deduplicate: dup=%v err=%v", dupDelayed2, err)
	}

	// Pending wake tracking + S8 owner stripping + MarkT3EventWoken.
	pendingWakes, err := s.PendingWakeT3Events(ctx, thID)
	if err != nil || len(pendingWakes) != 1 || pendingWakes[0].EventID != ev1.EventID {
		t.Fatalf("expected 1 pending wake event, got %+v err=%v", pendingWakes, err)
	}
	if pendingWakes[0].ConversationID != "" || pendingWakes[0].TaskID != "" {
		t.Fatalf("replayed event must not carry spoofed owner fields: %+v", pendingWakes[0])
	}
	// Duplicate event_id before wake must also strip spoofed owner fields.
	_, dupPreWake, storedPreWake, err := s.RecordT3EventAndUpsertThread(ctx, ev1, spoofRaw, domain.T3ThreadPatch{
		ThreadID:      thID,
		EnvironmentID: envA,
		At:            t2,
	}, t3)
	if err != nil || !dupPreWake || storedPreWake.Event.ConversationID != "" || storedPreWake.Event.TaskID != "" {
		t.Fatalf("duplicate event leaked spoofed owner fields: dup=%v stored=%+v err=%v", dupPreWake, storedPreWake, err)
	}
	if err := s.MarkT3EventWoken(ctx, ev1.EventID, t3); err != nil {
		t.Fatalf("mark woken: %v", err)
	}
	pendingWakesAfter, err := s.PendingWakeT3Events(ctx, thID)
	if err != nil || len(pendingWakesAfter) != 0 {
		t.Fatalf("expected 0 pending wakes after mark, got %+v err=%v", pendingWakesAfter, err)
	}
	// Duplicate event_id reports stored WokenAt != nil.
	_, dup2, stored2, err := s.RecordT3EventAndUpsertThread(ctx, ev1, json.RawMessage(`{}`), domain.T3ThreadPatch{
		ThreadID:      thID,
		EnvironmentID: envA,
		Status:        "corrupted",
		At:            t3,
	}, t3)
	if err != nil || !dup2 || stored2.WokenAt == nil {
		t.Fatalf("expected deduplicated=true with WokenAt set, got dup=%v stored=%+v err=%v", dup2, stored2, err)
	}

	// Wrong request_id does NOT clear pending_request; matching request_id DOES clear it.
	if err := s.ClearT3ThreadPendingRequest(ctx, thID, "req-other", t3); err != nil {
		t.Fatalf("clear mismatch request_id: %v", err)
	}
	stillPending, _, _ := s.T3Thread(ctx, thID)
	if stillPending.PendingRequest == nil {
		t.Fatal("mismatched request_id cleared pending_request")
	}
	if err := s.ClearT3ThreadPendingRequest(ctx, thID, "req-1", t3); err != nil {
		t.Fatalf("clear matching request_id: %v", err)
	}
	cleared, _, _ := s.T3Thread(ctx, thID)
	if cleared.PendingRequest != nil {
		t.Fatalf("matching request_id did not clear pending_request: %+v", cleared.PendingRequest)
	}

	// Filter threads by host_id, conversation_id, project_id, status.
	listed, err := s.ListT3Threads(ctx, domain.T3ThreadFilter{
		HostID:         h1.HostID,
		ConversationID: "personal:7",
		ProjectID:      "proj-2",
		Status:         "waiting_input",
	})
	if err != nil || len(listed) != 1 || listed[0].ThreadID != thID {
		t.Fatalf("list threads filtered by host_id: %+v err=%v", listed, err)
	}
	empty, err := s.ListT3Threads(ctx, domain.T3ThreadFilter{
		HostID: h1.HostID,
		Status: "completed",
	})
	if err != nil || len(empty) != 0 {
		t.Fatalf("expected empty list for non-matching status, got %+v err=%v", empty, err)
	}
}
