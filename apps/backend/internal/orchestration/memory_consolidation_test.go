package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/knowledge"
)

func TestMemoryConsolidationWorkflowRunsTwoPhasesAndPersistsReport(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	seed := []domain.MemoryEntry{
		{ID: "pref-1", Scope: conv.ID, Kind: "preference", Content: "Answer briefly.", Source: "s1", UpdatedAt: now.Add(-4 * time.Hour)},
		{ID: "pref-2", Scope: conv.ID, Kind: "preference", Content: "Keep answers short and brief.", Source: "s2", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "noise-1", Scope: conv.ID, Kind: "episode", Content: "Old watcher check from 2026-09-01.", Source: "s3", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "fact-1", Scope: conv.ID, Kind: "fact", Content: "Works on OpenAlbert.", Source: "s4", UpdatedAt: now.Add(-1 * time.Hour)},
	}
	for _, entry := range seed {
		if err := k.Update(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}

	a.Model = &scriptedModel{run: func(step int, messages []domain.Message) (domain.Message, error) {
		switch step {
		case 0:
			return finish(`{"operations":[{"op":"merge","ids":["pref-1","pref-2"],"content":"Answer briefly.","kind":"preference","reason":"duplicate preference"}]}`), nil
		case 1:
			return finish(`{"operations":[{"op":"delete","ids":["noise-1"],"reason":"expired watcher log"},{"op":"keep","ids":["fact-1"],"reason":"durable fact"}]}`), nil
		default:
			return domain.Message{}, fmt.Errorf("unexpected step %d", step)
		}
	}}

	r := newTestRuntime(a, NewFakeClock(now), time.Second, 4)
	if err := r.RunMemoryConsolidation(ctx, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled"}); err != nil {
		t.Fatal(err)
	}

	entries, err := k.ListMemory(ctx, conv.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 remaining entries, got %+v %v", entries, err)
	}
	reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
	if err != nil || len(reports) != 1 {
		t.Fatalf("expected 1 persisted report, got %+v %v", reports, err)
	}
	report := reports[0]
	if report.BeforeCount != 4 || report.AfterCount != 2 || report.ProfileCharsDelta >= 0 || report.Status != "completed" || len(report.Operations) != 3 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestMemoryConsolidationWorkflowConvergesLargeBacklogAcrossRuns(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	// 5 hygiene entries, processed with BatchSize=2, MaxBatches=1 -> requires 3 runs to sweep completely.
	for i := range 5 {
		if err := k.Update(ctx, domain.MemoryEntry{
			ID:        fmt.Sprintf("ep-%02d", i),
			Scope:     conv.ID,
			Kind:      "episode",
			Content:   fmt.Sprintf("Expired log %d", i),
			Source:    "s",
			UpdatedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}

	visited := map[string]bool{}
	a.Model = &scriptedModel{run: func(_ int, messages []domain.Message) (domain.Message, error) {
		var payload struct {
			Entries []domain.MemoryEntry `json:"entries"`
		}
		if err := json.Unmarshal([]byte(messages[1].Content), &payload); err != nil {
			return domain.Message{}, err
		}
		ops := make([]map[string]any, 0, len(payload.Entries))
		for _, e := range payload.Entries {
			visited[e.ID] = true
			ops = append(ops, map[string]any{"op": "delete", "ids": []string{e.ID}, "reason": "expired"})
		}
		raw, _ := json.Marshal(map[string]any{"operations": ops})
		return finish(string(raw)), nil
	}}

	clk := NewFakeClock(base.Add(24 * time.Hour))
	r := newTestRuntime(a, clk, time.Second, 4)
	in := MemoryConsolidationInput{Scope: conv.ID, Trigger: "backlog", BatchSize: 2, MaxBatches: 1}
	for range 3 {
		if err := r.RunMemoryConsolidation(ctx, in); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}

	reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
	if err != nil || len(reports) != 3 {
		t.Fatalf("expected 3 reports, got %+v %v", reports, err)
	}
	if reports[0].HasMore || !reports[1].HasMore || !reports[2].HasMore {
		t.Fatalf("unexpected HasMore progression: %+v", reports)
	}
	if reports[0].Status != "completed" || reports[1].Status != "partial" || reports[2].Status != "partial" {
		t.Fatalf("unexpected statuses: %+v", reports)
	}
	if len(visited) != 5 {
		t.Fatalf("backlog sweep missed entries: visited=%v", visited)
	}
	remaining, err := k.ListMemory(ctx, conv.ID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("expected all expired entries pruned, remaining=%+v %v", remaining, err)
	}
}

func TestDispatchMemoryConsolidationScheduledThresholdAndBacklog(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "coding:1", Private: true, OwnerID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "group:-99", ChatID: -99, Private: false}); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "f1", Scope: conv.ID, Kind: "fact", Content: "Fact 1", UpdatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var dispatched []MemoryConsolidationInput
	r := &Runtime{
		Repo:                        repo,
		Activities:                  a,
		MemoryConsolidationInterval: 24 * time.Hour,
		hooks: &activityHooks{
			dispatchConsol: func(in MemoryConsolidationInput) bool {
				dispatched = append(dispatched, in)
				return true
			},
		},
	}

	// 1. Initial run with no prior state -> scheduled trigger.
	if err := r.DispatchMemoryConsolidation(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Immediate second check within cooldown must not re-dispatch.
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].Scope != conv.ID || dispatched[0].Trigger != "scheduled" {
		t.Fatalf("unexpected initial dispatch: %+v", dispatched)
	}

	// 2. Threshold trigger when USER.md crosses 6000 chars after cooldown.
	dispatched = nil
	rawState, _ := json.Marshal(agent.ConsolidationState{LastRunAt: now, LastAttemptAt: now, LastEntryCount: 1})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawState); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := k.Update(ctx, domain.MemoryEntry{
			ID:        fmt.Sprintf("big-pref-%d", i),
			Scope:     conv.ID,
			Kind:      "preference",
			Content:   strings.Repeat("x", 3200),
			UpdatedAt: now.Add(10 * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].Scope != conv.ID || dispatched[0].Trigger != "threshold" {
		t.Fatalf("unexpected threshold dispatch: %+v", dispatched)
	}

	// 3. Backlog trigger when HasMore is true after short backlog cooldown.
	dispatched = nil
	rawBacklog, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:      now.Add(2 * time.Hour),
		LastAttemptAt:  now.Add(2 * time.Hour),
		LastEntryCount: 3,
		HasMore:        true,
	})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawBacklog); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(2*time.Hour+10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].Scope != conv.ID || dispatched[0].Trigger != "backlog" {
		t.Fatalf("unexpected backlog dispatch: %+v", dispatched)
	}

	// 4. Empty scope still prunes expired archives without starting consolidation or calling the model.
	dispatched = nil
	emptyRoot := t.TempDir()
	emptyKnow, err := knowledge.New(emptyRoot)
	if err != nil {
		t.Fatal(err)
	}
	emptyScope := "group:-99"
	oldDay := now.AddDate(0, 0, -45)
	tempEntry := domain.MemoryEntry{ID: "temp", Scope: emptyScope, Kind: "fact", Content: "temp", CreatedAt: oldDay, UpdatedAt: oldDay}
	if err := emptyKnow.Update(ctx, tempEntry); err != nil {
		t.Fatal(err)
	}
	if applied, reason, err := emptyKnow.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     emptyScope,
		RunID:     "old-run",
		Now:       oldDay,
		Expected:  []domain.MemoryEntry{tempEntry},
		DeleteIDs: []string{"temp"},
	}); err != nil || !applied || reason != "" {
		t.Fatalf("seed archive via ApplyMemoryMutation failed: applied=%v reason=%q err=%v", applied, reason, err)
	}
	matchesBefore, err := filepath.Glob(filepath.Join(emptyRoot, "scopes", "*", "archive", oldDay.Format("2006-01-02")))
	if err != nil || len(matchesBefore) != 1 {
		t.Fatalf("expected archived directory before prune: %v %v", matchesBefore, err)
	}
	a.Knowledge = emptyKnow
	rawDone, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:      now.Add(2 * time.Hour),
		LastAttemptAt:  now.Add(2 * time.Hour),
		LastEntryCount: 3,
	})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawDone); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(2*time.Hour+15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 0 {
		t.Fatalf("unexpected consolidation dispatch on empty scope: %+v", dispatched)
	}
	if _, err := os.Stat(matchesBefore[0]); !os.IsNotExist(err) {
		t.Fatalf("expired archive directory on empty scope was not pruned: %v", err)
	}
}

func TestMemoryConsolidationWorkflowBoundsPhase1BacklogAndPersistsFailureReceipt(t *testing.T) {
	t.Run("bounded phase 1 cursor convergence", func(t *testing.T) {
		a, repo, k, conv := newFixture(t)
		ctx := context.Background()
		base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		for i := range 3 {
			if err := k.Update(ctx, domain.MemoryEntry{
				ID:        fmt.Sprintf("pref-%02d", i),
				Scope:     conv.ID,
				Kind:      "preference",
				Content:   fmt.Sprintf("Preference %d", i),
				Source:    "s",
				UpdatedAt: base.Add(time.Duration(i) * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := k.Update(ctx, domain.MemoryEntry{ID: "fact-00", Scope: conv.ID, Kind: "fact", Content: "Durable fact", Source: "s", UpdatedAt: base.Add(10 * time.Hour)}); err != nil {
			t.Fatal(err)
		}

		seen := []string{}
		a.Model = &scriptedModel{run: func(_ int, messages []domain.Message) (domain.Message, error) {
			var payload struct {
				Entries []domain.MemoryEntry `json:"entries"`
			}
			if err := json.Unmarshal([]byte(messages[1].Content), &payload); err != nil {
				return domain.Message{}, err
			}
			for _, e := range payload.Entries {
				seen = append(seen, e.ID)
			}
			return finish(`{"operations":[]}`), nil
		}}

		clk := NewFakeClock(base.Add(24 * time.Hour))
		r := newTestRuntime(a, clk, time.Second, 4)
		in := MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled", BatchSize: 2, MaxBatches: 1}
		for range 3 {
			if err := r.RunMemoryConsolidation(ctx, in); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Second)
		}

		if strings.Join(seen, ",") != "pref-00,pref-01,pref-02,fact-00" {
			t.Fatalf("unexpected phase 1 -> phase 2 order: %v", seen)
		}
		reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
		if err != nil || len(reports) != 3 || reports[0].HasMore || !reports[1].HasMore || !reports[2].HasMore {
			t.Fatalf("unexpected bounded phase 1 reports: %+v %v", reports, err)
		}
	})

	t.Run("later batch failure still persists report with earlier operations", func(t *testing.T) {
		a, repo, k, conv := newFixture(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

		if err := k.Update(ctx, domain.MemoryEntry{ID: "p1", Scope: conv.ID, Kind: "preference", Content: "Rule A", Source: "s1", UpdatedAt: now.Add(-3 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := k.Update(ctx, domain.MemoryEntry{ID: "p2", Scope: conv.ID, Kind: "preference", Content: "Rule A dup", Source: "s2", UpdatedAt: now.Add(-2 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := k.Update(ctx, domain.MemoryEntry{ID: "f1", Scope: conv.ID, Kind: "fact", Content: "Fact 1", Source: "s3", UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}

		a.Model = &scriptedModel{run: func(step int, _ []domain.Message) (domain.Message, error) {
			if step == 0 {
				return finish(`{"operations":[{"op":"merge","ids":["p1","p2"],"content":"Rule A","kind":"preference","reason":"dup"}]}`), nil
			}
			return domain.Message{}, domain.NewNonRetryableError("provider outage", "provider_outage", errors.New("provider outage"))
		}}

		r := newTestRuntime(a, NewFakeClock(now), time.Second, 4)
		if err := r.RunMemoryConsolidation(ctx, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled"}); err == nil {
			t.Fatal("expected error on hygiene batch failure")
		}
		reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
		if err != nil || len(reports) != 1 {
			t.Fatalf("expected failed report persisted, got %+v %v", reports, err)
		}
		if reports[0].Status != "failed" || !reports[0].HasMore || len(reports[0].Operations) != 1 || reports[0].Operations[0].Status != "applied" || reports[0].Error == "" {
			t.Fatalf("unexpected failure report: %+v", reports[0])
		}
	})

	t.Run("partial sub-chunk failure still records earlier sub-chunk deletion", func(t *testing.T) {
		a, repo, k, conv := newFixture(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

		if err := k.Update(ctx, domain.MemoryEntry{ID: "sub-0", Scope: conv.ID, Kind: "episode", Content: "Expired A", Source: "s1", UpdatedAt: now.Add(-2 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := k.Update(ctx, domain.MemoryEntry{ID: "sub-1", Scope: conv.ID, Kind: "episode", Content: "Expired B", Source: "s2", UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}

		a.Model = &splitBudgetModel{
			scriptedModel: &scriptedModel{run: func(step int, _ []domain.Message) (domain.Message, error) {
				if step == 0 {
					return finish(`{"operations":[{"op":"delete","ids":["sub-0"],"reason":"expired sub-chunk 0"}]}`), nil
				}
				return domain.Message{}, domain.NewNonRetryableError("sub-chunk 1 failed", "subchunk_err", errors.New("sub-chunk 1 failed"))
			}},
		}

		r := newTestRuntime(a, NewFakeClock(now), time.Second, 4)
		if err := r.RunMemoryConsolidation(ctx, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled"}); err == nil {
			t.Fatal("expected error on sub-chunk 1 failure")
		}
		reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
		if err != nil || len(reports) != 1 {
			t.Fatalf("expected failure report, got %+v %v", reports, err)
		}
		if reports[0].Status != "failed" || len(reports[0].Operations) != 1 || reports[0].Operations[0].Op != "delete" || reports[0].Operations[0].IDs[0] != "sub-0" {
			t.Fatalf("failed report omitted sub-chunk 0 deletion: %+v", reports[0])
		}
	})

	t.Run("phase 1 compares duplicates across a full 60-entry batch boundary", func(t *testing.T) {
		a, repo, k, conv := newFixture(t)
		ctx := context.Background()
		base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

		for i := range 61 {
			content := fmt.Sprintf("Unique preference %02d", i)
			if i == 0 || i == 60 {
				content = "Always prefer concise bullet points"
			}
			if err := k.Update(ctx, domain.MemoryEntry{
				ID:        fmt.Sprintf("pref-%02d", i),
				Scope:     conv.ID,
				Kind:      "preference",
				Content:   content,
				Source:    fmt.Sprintf("s%d", i),
				UpdatedAt: base.Add(time.Duration(i) * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
		}

		mergedAcrossBoundary := false
		a.Model = &scriptedModel{run: func(_ int, messages []domain.Message) (domain.Message, error) {
			var payload struct {
				Entries          []domain.MemoryEntry `json:"entries"`
				ReferenceEntries []domain.MemoryEntry `json:"reference_entries"`
			}
			if err := json.Unmarshal([]byte(messages[1].Content), &payload); err != nil {
				return domain.Message{}, err
			}
			hasFirst, hasLast := false, false
			for _, e := range append(payload.Entries, payload.ReferenceEntries...) {
				if e.ID == "pref-00" {
					hasFirst = true
				}
				if e.ID == "pref-60" {
					hasLast = true
				}
			}
			if hasFirst && hasLast && !mergedAcrossBoundary {
				mergedAcrossBoundary = true
				return finish(`{"operations":[{"op":"merge","ids":["pref-00","pref-60"],"content":"Always prefer concise bullet points","kind":"preference","reason":"cross-batch duplicate"}]}`), nil
			}
			return finish(`{"operations":[]}`), nil
		}}

		r := newTestRuntime(a, NewFakeClock(base.Add(24*time.Hour)), time.Second, 4)
		if err := r.RunMemoryConsolidation(ctx, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled", BatchSize: 60, MaxBatches: 5}); err != nil {
			t.Fatal(err)
		}

		if !mergedAcrossBoundary {
			t.Fatal("pref-00 and pref-60 never appeared together across batch boundary")
		}
		entries, err := k.ListMemory(ctx, conv.ID)
		if err != nil || len(entries) != 60 {
			t.Fatalf("expected 60 entries after cross-batch merge, got %d %v", len(entries), err)
		}
		reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
		if err != nil || len(reports) != 1 || reports[0].AfterCount != 60 {
			t.Fatalf("unexpected report after cross-batch merge: %+v %v", reports, err)
		}
	})
}

type splitBudgetModel struct {
	*scriptedModel
}

func (m *splitBudgetModel) FitsContext(_ context.Context, messages []domain.Message, _ []domain.Tool) (bool, error) {
	return !(strings.Contains(messages[1].Content, `"sub-0"`) && strings.Contains(messages[1].Content, `"sub-1"`)), nil
}

func TestDispatchMemoryConsolidationPreventsHourlyProfileThresholdLoop(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	for i := range 2 {
		if err := k.Update(ctx, domain.MemoryEntry{
			ID:        fmt.Sprintf("stable-pref-%d", i),
			Scope:     conv.ID,
			Kind:      "preference",
			Content:   strings.Repeat("p", 3200),
			UpdatedAt: now.Add(-2 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := k.Profile(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	stableChars := len([]rune(profile))
	if stableChars < agent.DefaultProfileCharThreshold {
		t.Fatalf("test setup error: expected USER.md >= %d chars, got %d", agent.DefaultProfileCharThreshold, stableChars)
	}

	rawState, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:        now,
		LastAttemptAt:    now,
		LastRunID:        "run-stable",
		LastEntryCount:   2,
		LastProfileChars: stableChars,
		LastProfileHash:  agent.ID(profile),
	})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawState); err != nil {
		t.Fatal(err)
	}

	var dispatched []MemoryConsolidationInput
	r := &Runtime{
		Repo:                        repo,
		Activities:                  a,
		MemoryConsolidationInterval: 24 * time.Hour,
		hooks: &activityHooks{
			dispatchConsol: func(in MemoryConsolidationInput) bool {
				dispatched = append(dispatched, in)
				return true
			},
		},
	}

	// 1. After threshold cooldown (2h, 6h, 12h), unchanged USER.md >= 6000 chars must NOT re-trigger.
	for _, offset := range []time.Duration{2 * time.Hour, 6 * time.Hour, 12 * time.Hour} {
		if err := r.DispatchMemoryConsolidation(ctx, now.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	if len(dispatched) != 0 {
		t.Fatalf("unchanged profile re-triggered: %+v", dispatched)
	}

	// 2. If USER.md shrinks slightly while staying >= 6000 chars, still do NOT trigger threshold.
	if err := k.Update(ctx, domain.MemoryEntry{
		ID:        "stable-pref-1",
		Scope:     conv.ID,
		Kind:      "preference",
		Content:   strings.Repeat("p", 3100),
		UpdatedAt: now.Add(3 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 0 {
		t.Fatalf("shrunk profile triggered: %+v", dispatched)
	}

	// 3. If USER.md changes at the exact same length (hash changes), trigger threshold once.
	if err := k.Update(ctx, domain.MemoryEntry{
		ID:        "stable-pref-1",
		Scope:     conv.ID,
		Kind:      "preference",
		Content:   strings.Repeat("q", 3200),
		UpdatedAt: now.Add(5 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].Trigger != "threshold" {
		t.Fatalf("hash change did not trigger threshold: %+v", dispatched)
	}

	// 4. Reset to stable state, then grow USER.md -> triggers threshold.
	dispatched = nil
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawState); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{
		ID:        "extra-pref",
		Scope:     conv.ID,
		Kind:      "preference",
		Content:   "Additional preference that grows USER.md.",
		UpdatedAt: now.Add(7 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].Trigger != "threshold" {
		t.Fatalf("profile growth did not trigger threshold: %+v", dispatched)
	}

	// 5. After MemoryConsolidationInterval (24h), scheduled run still fires even without growth.
	dispatched = nil
	if err := k.Delete(ctx, conv.ID, "extra-pref"); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{
		ID:        "stable-pref-1",
		Scope:     conv.ID,
		Kind:      "preference",
		Content:   strings.Repeat("p", 3200),
		UpdatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawState); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(dispatched) != 1 || dispatched[0].Trigger != "scheduled" {
		t.Fatalf("scheduled run did not fire at 24h: %+v", dispatched)
	}
}

func TestMemoryConsolidationWorkflowPhase2PromotionQueuesPhase1FollowUp(t *testing.T) {
	a, repo, k, conv := newFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	if err := k.Update(ctx, domain.MemoryEntry{ID: "p-existing", Scope: conv.ID, Kind: "preference", Content: "Prefer short answers.", Source: "s1", UpdatedAt: now.Add(-3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "f-keep", Scope: conv.ID, Kind: "fact", Content: "Has a bicycle.", Source: "s1b", UpdatedAt: now.Add(-2*time.Hour - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "f-promote", Scope: conv.ID, Kind: "fact", Content: "Always prefers short answers in chat.", Source: "s2", UpdatedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	rawState, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:     now.Add(-10 * time.Minute),
		LastAttemptAt: now.Add(-10 * time.Minute),
		LastRunID:     "prev-run",
		Phase1HasMore: false,
		Phase2HasMore: true,
		HasMore:       true,
	})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawState); err != nil {
		t.Fatal(err)
	}

	sawPhase1InSecondRun := false
	modelCalls := 0
	a.Model = &scriptedModel{run: func(step int, messages []domain.Message) (domain.Message, error) {
		modelCalls++
		switch step {
		case 0:
			return finish(`{"operations":[{"op":"reclassify","ids":["f-promote"],"kind":"preference","reason":"lasting preference"},{"op":"keep","ids":["f-keep"],"reason":"durable fact"}]}`), nil
		case 1:
			if strings.Contains(messages[0].Content, `"compact"`) && strings.Contains(messages[1].Content, `"f-promote"`) && strings.Contains(messages[1].Content, `"p-existing"`) {
				sawPhase1InSecondRun = true
			}
			return finish(`{"operations":[{"op":"merge","ids":["p-existing","f-promote"],"content":"Prefer short answers.","kind":"preference","reason":"deduplicate promoted preference"}]}`), nil
		default:
			return domain.Message{}, fmt.Errorf("unexpected step %d (hygiene should not re-run)", step)
		}
	}}

	clk := NewFakeClock(now)
	r := newTestRuntime(a, clk, time.Second, 4)
	for range 2 {
		if err := r.RunMemoryConsolidation(ctx, MemoryConsolidationInput{Scope: conv.ID, Trigger: "backlog"}); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}

	if !sawPhase1InSecondRun || modelCalls != 2 {
		t.Fatalf("expected exactly 2 model calls (phase 2 + phase 1 follow-up only), got calls=%d sawPhase1=%v", modelCalls, sawPhase1InSecondRun)
	}
	reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
	if err != nil || len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %+v %v", reports, err)
	}
	if reports[1].Status != "partial" || !reports[1].HasMore || reports[0].Status != "completed" || reports[0].HasMore {
		t.Fatalf("unexpected report progression after Phase 2 promotion: %+v", reports)
	}
	entries, err := k.ListMemory(ctx, conv.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected p-existing and f-keep after Phase 1 follow-up, got %+v %v", entries, err)
	}
}
