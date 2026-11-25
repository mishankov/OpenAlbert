package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestConsolidationProfileBatchMergesReclassifiesDeletesAndArchives(t *testing.T) {
	a, repo, k, conv := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	seed := []domain.MemoryEntry{
		{ID: "p1", Scope: conv.ID, Kind: "preference", Content: "Reply concisely.", Source: "s1", UpdatedAt: now.Add(-4 * time.Hour)},
		{ID: "p2", Scope: conv.ID, Kind: "preference", Content: "Keep answers brief and concise.", Source: "s2", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "p3", Scope: conv.ID, Kind: "preference", Content: "Remind about dentist tomorrow at 9am.", Source: "s3", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "p4", Scope: conv.ID, Kind: "preference", Content: "Obsolete duplicate rule.", Source: "s4", UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "core", Scope: conv.ID, Kind: "profile", Content: "Lives in Moscow, UTC+3.", Source: "s0", UpdatedAt: now.Add(-5 * time.Hour)},
	}
	for _, entry := range seed {
		if err := k.Update(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}

	m := &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
		if !strings.Contains(messages[1].Content, `"p1"`) || !strings.Contains(messages[1].Content, `"core"`) {
			t.Fatalf("missing profile/preference entries in prompt: %s", messages[1].Content)
		}
		return domain.Message{Content: `{"operations":[
			{"op":"merge","ids":["p1","p2"],"content":"Prefer brief, concise replies.","kind":"preference","reason":"duplicate conciseness rules"},
			{"op":"reclassify","ids":["p3"],"kind":"episode","reason":"one-time reminder, not lasting preference"},
			{"op":"delete","ids":["p4"],"reason":"obsolete duplicate"},
			{"op":"keep","ids":["core"],"reason":"core profile fact"},
			{"op":"delete","ids":["invented-id"],"reason":"hallucinated"}
		]}`}, nil
	}}
	a.Model = m

	in := ConsolidationBatchInput{RunID: "run-1", Scope: conv.ID, Group: false, BatchIndex: 0, BatchIDs: []string{"core", "p1", "p2", "p3", "p4"}, Now: now}
	res, err := a.ConsolidateProfileBatch(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// Retry must reuse durable batch-done checkpoint without re-calling model or double-mutating.
	res2, err := a.ConsolidateProfileBatch(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 || len(res.Operations) != 5 || len(res2.Operations) != 5 {
		t.Fatalf("unexpected retry behavior: calls=%d ops=%+v", m.calls, res.Operations)
	}
	if res.Operations[4].Status != "skipped" || res.Operations[4].SkipReason != "unknown_id" {
		t.Fatalf("invented ID not rejected: %+v", res.Operations[4])
	}

	merged, err := k.Read(ctx, conv.ID, "p1")
	if err != nil || merged.Content != "Prefer brief, concise replies." || merged.Source != "s1; s2" {
		t.Fatalf("merged entry invalid: %+v %v", merged, err)
	}
	if _, err := k.Read(ctx, conv.ID, "p2"); !os.IsNotExist(err) {
		t.Fatalf("absorbed entry p2 still exists: %v", err)
	}
	if _, err := k.Read(ctx, conv.ID, "p4"); !os.IsNotExist(err) {
		t.Fatalf("deleted entry p4 still exists: %v", err)
	}
	reclassified, err := k.Read(ctx, conv.ID, "p3")
	if err != nil || reclassified.Kind != "episode" {
		t.Fatalf("reclassify failed: %+v %v", reclassified, err)
	}
	profile, err := k.Profile(ctx, conv.ID)
	if err != nil || !strings.Contains(profile, "Lives in Moscow") || !strings.Contains(profile, "Prefer brief, concise replies.") || strings.Contains(profile, "dentist") || strings.Contains(profile, "Obsolete") {
		t.Fatalf("USER.md not cleanly updated: %q %v", profile, err)
	}
	if _, ok, err := repo.LoadCheckpoint(ctx, "memory-consolidation-run/run-1/batch-done/profile/0"); err != nil || !ok {
		t.Fatalf("missing batch-done checkpoint: ok=%v err=%v", ok, err)
	}
}

func TestConsolidationOptimisticConcurrencySkipsStaleOperations(t *testing.T) {
	a, _, k, conv := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	e1 := domain.MemoryEntry{ID: "m1", Scope: conv.ID, Kind: "preference", Content: "Old A", Source: "s1", UpdatedAt: now.Add(-2 * time.Hour)}
	e2 := domain.MemoryEntry{ID: "m2", Scope: conv.ID, Kind: "preference", Content: "Old B", Source: "s2", UpdatedAt: now.Add(-2 * time.Hour)}
	e3 := domain.MemoryEntry{ID: "m3", Scope: conv.ID, Kind: "preference", Content: "Clean delete", Source: "s3", UpdatedAt: now.Add(-2 * time.Hour)}
	for _, entry := range []domain.MemoryEntry{e1, e2, e3} {
		if err := k.Update(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}

	a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
		// Simulate a concurrent live chat edit to m1 while the LLM was running.
		updated := e1
		updated.Content = "Fresh live edit"
		updated.UpdatedAt = now.Add(-time.Minute)
		if err := k.Update(ctx, updated); err != nil {
			t.Fatal(err)
		}
		return domain.Message{Content: `{"operations":[
			{"op":"merge","ids":["m1","m2"],"content":"Stale merged content","kind":"preference","reason":"dup"},
			{"op":"delete","ids":["m3"],"reason":"obsolete"}
		]}`}, nil
	}}

	res, err := a.ConsolidateProfileBatch(ctx, ConsolidationBatchInput{RunID: "run-conc", Scope: conv.ID, BatchIndex: 0, BatchIDs: []string{"m1", "m2", "m3"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Operations) != 2 || res.Operations[0].Status != "skipped" || res.Operations[0].SkipReason != "concurrent_modification" || res.Operations[1].Status != "applied" {
		t.Fatalf("concurrency guard mismatch: %+v", res.Operations)
	}
	got1, err := k.Read(ctx, conv.ID, "m1")
	if err != nil || got1.Content != "Fresh live edit" {
		t.Fatalf("concurrent edit overwritten: %+v %v", got1, err)
	}
	if _, err := k.Read(ctx, conv.ID, "m2"); err != nil {
		t.Fatalf("m2 deleted despite skipped merge: %v", err)
	}
	if _, err := k.Read(ctx, conv.ID, "m3"); !os.IsNotExist(err) {
		t.Fatalf("unrelated clean op not applied: %v", err)
	}
}

func TestConsolidationRetryRecoversAfterHalfMergeAndFullBatchDeletion(t *testing.T) {
	t.Run("half-applied merge", func(t *testing.T) {
		a, repo, k, conv := fixture(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

		e1 := domain.MemoryEntry{ID: "h1", Scope: conv.ID, Kind: "preference", Content: "Short answers", Source: "s1", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
		e2 := domain.MemoryEntry{ID: "h2", Scope: conv.ID, Kind: "preference", Content: "Brief replies", Source: "s2", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
		for _, e := range []domain.MemoryEntry{e1, e2} {
			if err := k.Update(ctx, e); err != nil {
				t.Fatal(err)
			}
		}

		// Simulate crash after chunks + plan checkpoint saved AND survivor h1 already updated on disk,
		// before h2 was deleted or progress/applied checkpoints were written.
		chunksRaw, _ := json.Marshal(batchChunksCheckpoint{Chunks: []consolidationChunk{{Entries: []domain.MemoryEntry{e1, e2}}}})
		if err := repo.SaveCheckpoint(ctx, "memory-consolidation-run/run-half/chunks/profile/0", chunksRaw); err != nil {
			t.Fatal(err)
		}
		planRaw, _ := json.Marshal(consolidationPlanCheckpoint{
			Plan:     consolidationPlan{Operations: []consolidationPlanOp{{Op: "merge", IDs: []string{"h1", "h2"}, Content: "Prefer short, brief replies.", Kind: "preference", Reason: "dup"}}},
			Snapshot: []domain.MemoryEntry{e1, e2},
			Now:      now,
		})
		if err := repo.SaveCheckpoint(ctx, "memory-consolidation-run/run-half/plan/profile/0/0", planRaw); err != nil {
			t.Fatal(err)
		}
		survivor := domain.MemoryEntry{ID: "h1", Scope: conv.ID, Kind: "preference", Content: "Prefer short, brief replies.", Source: "s1; s2", OriginSource: "s1", CreatedAt: e1.CreatedAt, UpdatedAt: now}
		if err := k.Update(ctx, survivor); err != nil {
			t.Fatal(err)
		}

		a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
			t.Fatal("retry should reuse plan checkpoint without calling model")
			return domain.Message{}, nil
		}}

		res, err := a.ConsolidateProfileBatch(ctx, ConsolidationBatchInput{RunID: "run-half", Scope: conv.ID, BatchIndex: 0, BatchIDs: []string{"h1", "h2"}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Operations) != 1 || res.Operations[0].Status != "applied" {
			t.Fatalf("half-merge retry did not recover: %+v", res.Operations)
		}
		if _, err := k.Read(ctx, conv.ID, "h2"); !os.IsNotExist(err) {
			t.Fatalf("absorbed h2 was not deleted on retry: %v", err)
		}
	})

	t.Run("all entries deleted before batch-done checkpoint", func(t *testing.T) {
		a, repo, k, conv := fixture(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

		d1 := domain.MemoryEntry{ID: "d1", Scope: conv.ID, Kind: "episode", Content: "Expired 1", Source: "s1", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
		if err := k.Update(ctx, d1); err != nil {
			t.Fatal(err)
		}
		chunksRaw, _ := json.Marshal(batchChunksCheckpoint{Chunks: []consolidationChunk{{Entries: []domain.MemoryEntry{d1}}}})
		if err := repo.SaveCheckpoint(ctx, "memory-consolidation-run/run-del/chunks/hygiene/0", chunksRaw); err != nil {
			t.Fatal(err)
		}
		planRaw, _ := json.Marshal(consolidationPlanCheckpoint{
			Plan:     consolidationPlan{Operations: []consolidationPlanOp{{Op: "delete", IDs: []string{"d1"}, Reason: "expired"}}},
			Snapshot: []domain.MemoryEntry{d1},
			Now:      now,
		})
		if err := repo.SaveCheckpoint(ctx, "memory-consolidation-run/run-del/plan/hygiene/0/0", planRaw); err != nil {
			t.Fatal(err)
		}
		// Archive and delete d1 via production ApplyMemoryMutation as if step finished
		// mutating right before saving applied/batch-done checkpoints.
		if applied, reason, err := k.ApplyMemoryMutation(ctx, domain.MemoryMutation{
			Scope:     conv.ID,
			RunID:     "run-del",
			Now:       now,
			Expected:  []domain.MemoryEntry{d1},
			DeleteIDs: []string{"d1"},
		}); err != nil || !applied || reason != "" {
			t.Fatalf("seed delete mutation failed: applied=%v reason=%q err=%v", applied, reason, err)
		}

		a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
			t.Fatal("retry should recover receipt from plan checkpoint without calling model")
			return domain.Message{}, nil
		}}

		res, err := a.ConsolidateHygieneBatch(ctx, ConsolidationBatchInput{RunID: "run-del", Scope: conv.ID, BatchIndex: 0, BatchIDs: []string{"d1"}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Operations) != 1 || res.Operations[0].Status != "applied" || res.Operations[0].IDs[0] != "d1" {
			t.Fatalf("failed to recover delete receipt when 0 live entries remained: %+v", res.Operations)
		}
	})
}

func TestConsolidationEnforcesGroupSubjectInvariantsAndProvenance(t *testing.T) {
	a, repo, k, _ := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	groupConv := domain.Conversation{ID: "group:-100", ChatID: -100, Private: false, Title: "Team"}
	if err := repo.SaveConversation(ctx, groupConv); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int64{10, 20} {
		if err := k.ObserveParticipant(ctx, domain.Participant{Scope: groupConv.ID, UserID: uid, DisplayName: "User", LastSeenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	entries := []domain.MemoryEntry{
		{ID: "u10-a", Scope: groupConv.ID, SubjectUserID: 10, SourceMessageID: "msg-10a", Evidence: "I drink oat milk", Kind: "preference", Content: "Drinks oat milk", Source: "gs1", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "u10-b", Scope: groupConv.ID, SubjectUserID: 10, SourceMessageID: "msg-10b", Evidence: "Oat milk in coffee", Kind: "preference", Content: "Prefers oat milk in coffee", Source: "gs2", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "u20-a", Scope: groupConv.ID, SubjectUserID: 20, SourceMessageID: "msg-20a", Evidence: "I prefer tea", Kind: "preference", Content: "Prefers tea", Source: "gs3", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "group-fact", Scope: groupConv.ID, SubjectUserID: 0, Kind: "fact", Content: "Standup is at 10am", Source: "gs4", UpdatedAt: now.Add(-1 * time.Hour)},
	}
	for _, entry := range entries {
		if err := k.Update(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := k.ListMemory(ctx, groupConv.ID)
	if err != nil {
		t.Fatal(err)
	}

	plan := consolidationPlan{Operations: []consolidationPlanOp{
		{Op: "merge", IDs: []string{"u10-a", "u20-a"}, Content: "Everyone likes drinks", Kind: "preference", Reason: "cross-user merge must fail"},
		{Op: "merge", IDs: []string{"u10-a", "u10-b"}, Content: "Prefers oat milk in coffee.", Kind: "preference", Reason: "same participant duplicate"},
		{Op: "reclassify", IDs: []string{"group-fact"}, Kind: "preference", Reason: "must not turn group fact into USER preference"},
	}}

	ops, err := a.applyConsolidationPlan(ctx, groupConv.ID, "run-group", "profile", "", true, now, snapshot, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops, got %+v", ops)
	}
	if ops[0].Status != "skipped" || ops[0].SkipReason != "different_subject_user_id" {
		t.Fatalf("cross-participant merge not blocked: %+v", ops[0])
	}
	if ops[1].Status != "applied" || ops[1].ResultID != "u10-a" {
		t.Fatalf("same-participant merge failed: %+v", ops[1])
	}
	if ops[2].Status != "skipped" || ops[2].SkipReason != "group_invariant" {
		t.Fatalf("group-wide preference reclassification not blocked: %+v", ops[2])
	}

	merged, err := k.Read(ctx, groupConv.ID, "u10-a")
	if err != nil {
		t.Fatal(err)
	}
	if merged.SubjectUserID != 10 || merged.SourceMessageID != "msg-10a" || merged.Evidence != "I drink oat milk" {
		t.Fatalf("primary participant provenance lost after merge: %+v", merged)
	}
	if len(merged.Provenance) != 2 || merged.Provenance[1].SourceMessageID != "msg-10b" || merged.Provenance[1].Evidence != "Oat milk in coffee" {
		t.Fatalf("supporting participant provenance pairs not preserved together: %+v", merged.Provenance)
	}
	if _, err := k.Read(ctx, groupConv.ID, "u20-a"); err != nil {
		t.Fatalf("other participant memory touched: %v", err)
	}
	gf, err := k.Read(ctx, groupConv.ID, "group-fact")
	if err != nil || gf.Kind != "fact" {
		t.Fatalf("group fact illegally reclassified: %+v %v", gf, err)
	}
}

func TestConsolidationHygieneBatchSuppliesTimeProfileAndSingleEntryFallback(t *testing.T) {
	a, _, k, conv := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	if err := k.Update(ctx, domain.MemoryEntry{ID: "prof", Scope: conv.ID, Kind: "profile", Content: "Lives in Moscow", Source: "s0", UpdatedAt: now.Add(-10 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "expired", Scope: conv.ID, Kind: "episode", Content: "Reminder fired on 2026-10-01", Source: "s1", UpdatedAt: now.Add(-9 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "durable", Scope: conv.ID, Kind: "fact", Content: "Owns a MacBook Pro", Source: "s2", UpdatedAt: now.Add(-8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	a.Model = &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
		var payload consolidationPayload
		if err := json.Unmarshal([]byte(messages[1].Content), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.CurrentTime != "2026-11-25T10:00:00Z" || !strings.Contains(payload.UserProfile, "Lives in Moscow") {
			t.Fatalf("missing current_time or user_profile in hygiene prompt: %+v", payload)
		}
		return domain.Message{Content: `{"operations":[{"op":"delete","ids":["expired"],"reason":"expired one-off log"},{"op":"keep","ids":["durable"],"reason":"durable personal fact"}]}`}, nil
	}}

	res, err := a.ConsolidateHygieneBatch(ctx, ConsolidationBatchInput{
		RunID:      "run-hygiene",
		Scope:      conv.ID,
		BatchIndex: 0,
		BatchIDs:   []string{"expired", "durable"},
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Operations) != 2 || res.Operations[0].Status != "applied" || res.Operations[1].Status != "kept" {
		t.Fatalf("unexpected hygiene results: %+v", res.Operations)
	}

	// Single-entry fallback: if a hygiene entry only fits once user_profile is dropped,
	// the model call must actually receive an empty user_profile.
	if err := k.Update(ctx, domain.MemoryEntry{ID: "oversized-profile-pref", Scope: conv.ID, Kind: "preference", Content: "oversized profile marker", Source: "s0", UpdatedAt: now.Add(-7 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	a.Model = &analysisBudgetModel{testModel: &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
		var payload consolidationPayload
		if err := json.Unmarshal([]byte(messages[1].Content), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.UserProfile != "" {
			t.Fatalf("fitted profile fallback was ignored on model call: %q", payload.UserProfile)
		}
		return domain.Message{Content: `{"operations":[{"op":"keep","ids":["durable"],"reason":"durable"}]}`}, nil
	}}}
	if _, err := a.ConsolidateHygieneBatch(ctx, ConsolidationBatchInput{
		RunID:      "run-hygiene-fallback",
		Scope:      conv.ID,
		BatchIndex: 0,
		BatchIDs:   []string{"durable"},
		Now:        now,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := a.FinalizeMemoryConsolidation(ctx, ConsolidationFinalizeInput{
		RunID:              "run-hygiene",
		Scope:              conv.ID,
		Trigger:            "scheduled",
		StartedAt:          now,
		CompletedAt:        now.Add(time.Second),
		BeforeCount:        3,
		ProfileCharsBefore: 35,
		BatchesProcessed:   1,
		Operations:         res.Operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.BeforeCount != 3 || report.AfterCount != 3 || report.Status != "completed" {
		t.Fatalf("unexpected report: %+v", report)
	}
	// Run-specific recovery checkpoints must be purged after finalization.
	for _, key := range []string{
		"memory-consolidation-run/run-hygiene/chunks/hygiene/0",
		"memory-consolidation-run/run-hygiene/plan/hygiene/0/0",
		"memory-consolidation-run/run-hygiene/batch-done/hygiene/0",
	} {
		if _, ok, err := a.Repo.LoadCheckpoint(ctx, key); err != nil || ok {
			t.Fatalf("ephemeral recovery checkpoint %s survived finalization: ok=%v err=%v", key, ok, err)
		}
	}
}

type interleavedFinalizeRepo struct {
	domain.Repository
	onFirstLoadKey string
	onFirstLoad    func()
	fired          bool
	failKey        string
	failErr        error
}

func (r *interleavedFinalizeRepo) LoadCheckpoint(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if r.onFirstLoad != nil && !r.fired && key == r.onFirstLoadKey {
		r.fired = true
		r.onFirstLoad()
	}
	if r.failKey != "" && key == r.failKey {
		return nil, false, r.failErr
	}
	return r.Repository.LoadCheckpoint(ctx, key)
}

func TestFinalizeMemoryConsolidationRetryAndInterleavingPreserveReceiptsAndState(t *testing.T) {
	t.Run("interleaved attempts do not overwrite recovered deletion receipt", func(t *testing.T) {
		a, repo, _, conv := fixture(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

		const runID = "run-finalize-interleaved"
		chunksRaw, _ := json.Marshal(batchChunksCheckpoint{Chunks: []consolidationChunk{{Entries: []domain.MemoryEntry{{ID: "sub-0"}}}}})
		if err := repo.SaveCheckpoint(ctx, "memory-consolidation-run/"+runID+"/chunks/hygiene/0", chunksRaw); err != nil {
			t.Fatal(err)
		}
		appliedRaw, _ := json.Marshal([]domain.MemoryConsolidationOperation{{Phase: "hygiene", Op: "delete", IDs: []string{"sub-0"}, Reason: "expired", Status: "applied"}})
		if err := repo.SaveCheckpoint(ctx, "memory-consolidation-run/"+runID+"/applied/hygiene/0/0", appliedRaw); err != nil {
			t.Fatal(err)
		}

		in := ConsolidationFinalizeInput{
			RunID:          runID,
			Scope:          conv.ID,
			Trigger:        "scheduled",
			StartedAt:      now,
			CompletedAt:    now.Add(time.Second),
			Status:         "failed",
			BeforeCount:    2,
			HasMore:        true,
			FailedBatchKey: "hygiene/0",
			Error:          "sub-chunk 1 failed",
		}
		// Interleave: right before attempt B reads the chunk checkpoint, attempt A
		// finishes recovering sub-0, persisting the report, and clearing checkpoints.
		// Attempt B then builds an empty local candidate, which must NOT overwrite A's report.
		a.Repo = &interleavedFinalizeRepo{
			Repository:     repo,
			onFirstLoadKey: "memory-consolidation-run/" + runID + "/chunks/hygiene/0",
			onFirstLoad: func() {
				aWinner := *a
				aWinner.Repo = repo
				if _, err := aWinner.FinalizeMemoryConsolidation(ctx, in); err != nil {
					t.Errorf("attempt A finalize failed: %v", err)
				}
			},
		}
		attemptBReport, attemptBErr := a.FinalizeMemoryConsolidation(ctx, in)
		if attemptBErr != nil {
			t.Fatal(attemptBErr)
		}
		if len(attemptBReport.Operations) != 1 || attemptBReport.Operations[0].IDs[0] != "sub-0" {
			t.Fatalf("interleaved attempt B lost recovered receipt: %+v", attemptBReport)
		}
		reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
		if err != nil || len(reports) != 1 || len(reports[0].Operations) != 1 || reports[0].Operations[0].IDs[0] != "sub-0" {
			t.Fatalf("persisted report overwritten by interleaved attempt: %+v %v", reports, err)
		}
	})

	t.Run("state checkpoint read error aborts without erasing LastRunAt or checkpoints", func(t *testing.T) {
		a, repo, _, conv := fixture(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
		prevSuccess := now.Add(-12 * time.Hour)
		prevRaw, _ := json.Marshal(ConsolidationState{LastRunAt: prevSuccess, LastAttemptAt: prevSuccess, LastRunID: "prior-ok"})
		if err := repo.SaveCheckpoint(ctx, ConsolidationStateKey(conv.ID), prevRaw); err != nil {
			t.Fatal(err)
		}
		ephemeralKey := "memory-consolidation-run/run-state-err/chunks/hygiene/0"
		if err := repo.SaveCheckpoint(ctx, ephemeralKey, []byte(`{"chunks":[]}`)); err != nil {
			t.Fatal(err)
		}

		a.Repo = &interleavedFinalizeRepo{
			Repository: repo,
			failKey:    ConsolidationStateKey(conv.ID),
			failErr:    errors.New("transient postgres read error"),
		}
		if _, err := a.FinalizeMemoryConsolidation(ctx, ConsolidationFinalizeInput{
			RunID:       "run-state-err",
			Scope:       conv.ID,
			Trigger:     "scheduled",
			StartedAt:   now,
			CompletedAt: now.Add(time.Second),
			Status:      "failed",
			HasMore:     true,
			Error:       "batch error",
		}); err == nil {
			t.Fatal("expected state checkpoint read error to abort finalization")
		}
		// Previous LastRunAt and ephemeral checkpoints must remain intact.
		raw, ok, err := repo.LoadCheckpoint(ctx, ConsolidationStateKey(conv.ID))
		var state ConsolidationState
		if err != nil || !ok || json.Unmarshal(raw, &state) != nil || !state.LastRunAt.Equal(prevSuccess) {
			t.Fatalf("state checkpoint corrupted after read error: %s %v", raw, err)
		}
		if _, ok, err := repo.LoadCheckpoint(ctx, ephemeralKey); err != nil || !ok {
			t.Fatalf("ephemeral checkpoint cleared despite state read error: ok=%v err=%v", ok, err)
		}
	})
}

func TestSelectProfileReferenceEntriesFindsOlderDuplicatesAcrossLargeBacklog(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldestFirst := make([]domain.MemoryEntry, 0, 360)
	for i := range 360 {
		content := fmt.Sprintf("Unrelated rule number%03d about topic%03d", i, i)
		if i == 0 {
			content = "Always format code examples with Go syntax highlighting"
		}
		if i == 60 {
			content = "Format code examples with Go syntax highlighting always"
		}
		oldestFirst = append(oldestFirst, domain.MemoryEntry{
			ID:        fmt.Sprintf("pref-%03d", i),
			Scope:     "personal:7",
			Kind:      "preference",
			Content:   content,
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	// Pass newest-first ordering (matching production ListMemory) while batch0 holds oldest 60.
	newestFirst := make([]domain.MemoryEntry, len(oldestFirst))
	for i := range oldestFirst {
		newestFirst[i] = oldestFirst[len(oldestFirst)-1-i]
	}
	batch0 := oldestFirst[:60]
	refs := selectProfileReferenceEntries(batch0, newestFirst, maxProfileReferenceEntries)
	found := false
	for _, ref := range refs {
		if ref.ID == "pref-060" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("older duplicate pref-060 omitted from references in 360-entry backlog (got %d refs)", len(refs))
	}
}
