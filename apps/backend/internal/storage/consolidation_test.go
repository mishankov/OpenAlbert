package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestMemoryConsolidationContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { consolidationContract(t, NewMemory()) })
	t.Run("sqlite", func(t *testing.T) { consolidationContract(t, openTestSQLite(t)) })
}

func consolidationContract(t *testing.T, r domain.Repository) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	scopeA := nonce + ":a"
	scopeB := nonce + ":b"
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	r1 := domain.MemoryConsolidationReport{
		ID:                 nonce + ":run-1",
		Scope:              scopeA,
		Trigger:            "scheduled",
		StartedAt:          t0,
		CompletedAt:        t0.Add(time.Minute),
		Status:             "partial",
		BeforeCount:        100,
		AfterCount:         80,
		ProfileCharsBefore: 7000,
		ProfileCharsAfter:  4500,
		ProfileCharsDelta:  -2500,
		BatchesProcessed:   2,
		HasMore:            true,
		Operations: []domain.MemoryConsolidationOperation{
			{Phase: "profile", Op: "merge", IDs: []string{"p1", "p2"}, ResultID: "p1", Kind: "preference", Reason: "duplicate", Status: "applied"},
		},
	}
	saved1, err := r.SaveMemoryConsolidation(ctx, r1)
	if err != nil || saved1.Status != "partial" {
		t.Fatalf("initial save failed: %+v %v", saved1, err)
	}
	// Duplicate save on the same ID and scope preserves the original report without overwriting it.
	r1Dup := r1
	r1Dup.Status = "completed"
	r1Dup.Operations = nil
	savedDup, err := r.SaveMemoryConsolidation(ctx, r1Dup)
	if err != nil || savedDup.Status != "partial" || len(savedDup.Operations) != 1 {
		t.Fatalf("duplicate save overwrote original report: %+v %v", savedDup, err)
	}
	// Cross-scope save on the same ID must fail with ErrConflict.
	bad := r1
	bad.Scope = scopeB
	if _, err := r.SaveMemoryConsolidation(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict on cross-scope overwrite, got %v", err)
	}

	r2 := domain.MemoryConsolidationReport{
		ID:                 nonce + ":run-2",
		Scope:              scopeA,
		Trigger:            "threshold",
		StartedAt:          t0.Add(time.Hour),
		CompletedAt:        t0.Add(time.Hour + time.Minute),
		Status:             "completed",
		BeforeCount:        80,
		AfterCount:         70,
		ProfileCharsBefore: 4500,
		ProfileCharsAfter:  4000,
		ProfileCharsDelta:  -500,
	}
	if _, err := r.SaveMemoryConsolidation(ctx, r2); err != nil {
		t.Fatal(err)
	}

	rows, err := r.ListMemoryConsolidations(ctx, scopeA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != r2.ID || rows[1].ID != r1.ID || rows[1].Status != "partial" || len(rows[1].Operations) != 1 {
		t.Fatalf("unexpected reports for scopeA: %+v", rows)
	}
	empty, err := r.ListMemoryConsolidations(ctx, scopeB, 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("scopeB leaked reports: %+v %v", empty, err)
	}

	// Prefix deletion removes only matching checkpoints and escapes LIKE wildcards.
	prefix := "memory-consolidation-run/" + nonce + "_run/"
	for _, key := range []string{prefix + "chunks/p/0", prefix + "plan/p/0/0", "memory-consolidation-run/" + nonce + "Xrun/plan/p/0/0", "memory-consolidation-state/" + scopeA} {
		if err := r.SaveCheckpoint(ctx, key, []byte(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.DeleteCheckpointsByPrefix(ctx, prefix); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.LoadCheckpoint(ctx, prefix+"chunks/p/0"); err != nil || ok {
		t.Fatalf("prefix checkpoint survived deletion: ok=%v err=%v", ok, err)
	}
	if _, ok, err := r.LoadCheckpoint(ctx, "memory-consolidation-run/"+nonce+"Xrun/plan/p/0/0"); err != nil || !ok {
		t.Fatalf("wildcard-escaped sibling checkpoint was deleted: ok=%v err=%v", ok, err)
	}
	if _, ok, err := r.LoadCheckpoint(ctx, "memory-consolidation-state/"+scopeA); err != nil || !ok {
		t.Fatalf("state checkpoint was deleted: ok=%v err=%v", ok, err)
	}
}
