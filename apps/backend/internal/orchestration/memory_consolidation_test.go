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

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
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
			// Phase 1: profile & preferences.
			return finish(`{"operations":[{"op":"merge","ids":["pref-1","pref-2"],"content":"Answer briefly.","kind":"preference","reason":"duplicate preference"}]}`), nil
		case 1:
			// Phase 2: hygiene batch.
			return finish(`{"operations":[{"op":"delete","ids":["noise-1"],"reason":"expired watcher log"},{"op":"keep","ids":["fact-1"],"reason":"durable fact"}]}`), nil
		default:
			return domain.Message{}, fmt.Errorf("unexpected step %d", step)
		}
	}}

	env := newEnv(a)
	env.ExecuteWorkflow(MemoryConsolidationWorkflow, MemoryConsolidationInput{
		Scope:   conv.ID,
		Trigger: "scheduled",
	})
	requireWorkflowOK(t, env)

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

	threeRuns := func(ctx workflow.Context, in MemoryConsolidationInput) error {
		for i := range 3 {
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: fmt.Sprintf("consolidation-run-%d", i)})
			if err := workflow.ExecuteChildWorkflow(childCtx, MemoryConsolidationWorkflow, in).Get(ctx, nil); err != nil {
				return err
			}
		}
		return nil
	}

	env := newEnv(a)
	env.RegisterWorkflow(MemoryConsolidationWorkflow)
	env.ExecuteWorkflow(threeRuns, MemoryConsolidationInput{
		Scope:      conv.ID,
		Trigger:    "backlog",
		BatchSize:  2,
		MaxBatches: 1,
	})
	requireWorkflowOK(t, env)

	reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
	if err != nil || len(reports) != 3 {
		t.Fatalf("expected 3 reports, got %+v %v", reports, err)
	}
	// ListMemoryConsolidations returns newest first: run 2 (final) has HasMore=false, runs 1 and 0 have HasMore=true.
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

	// Coding conversation must be skipped; empty group must be skipped.
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "coding:1", Private: true, OwnerID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "group:-99", ChatID: -99, Private: false}); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "f1", Scope: conv.ID, Kind: "fact", Content: "Fact 1", UpdatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// 1. Initial run with no prior state -> scheduled trigger.
	cl := &mocks.Client{}
	cl.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
		return o.ID == MemoryConsolidationWorkflowID(conv.ID)
	}), mock.Anything, mock.MatchedBy(func(in MemoryConsolidationInput) bool {
		return in.Scope == conv.ID && in.Trigger == "scheduled"
	})).Return(nil, nil).Once()

	r := &Runtime{Repo: repo, Activities: a, Client: cl, TaskQueue: "openalbert", MemoryConsolidationInterval: 24 * time.Hour}
	if err := r.DispatchMemoryConsolidation(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Immediate second check within cooldown must not re-dispatch.
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cl.AssertExpectations(t)

	// 2. Threshold trigger when USER.md crosses 6000 chars after cooldown.
	rawState, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:      now,
		LastAttemptAt:  now,
		LastEntryCount: 1,
	})
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
	cl2 := &mocks.Client{}
	cl2.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.MatchedBy(func(in MemoryConsolidationInput) bool {
		return in.Scope == conv.ID && in.Trigger == "threshold"
	})).Return(nil, nil).Once()
	r.Client = cl2
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cl2.AssertExpectations(t)

	// 3. Backlog trigger when HasMore is true after short backlog cooldown.
	rawBacklog, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:      now.Add(2 * time.Hour),
		LastAttemptAt:  now.Add(2 * time.Hour),
		LastEntryCount: 3,
		HasMore:        true,
	})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawBacklog); err != nil {
		t.Fatal(err)
	}
	cl3 := &mocks.Client{}
	cl3.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.MatchedBy(func(in MemoryConsolidationInput) bool {
		return in.Scope == conv.ID && in.Trigger == "backlog"
	})).Return(nil, nil).Once()
	r.Client = cl3
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(2*time.Hour+10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cl3.AssertExpectations(t)

	// 4. Empty scope still prunes expired archives without starting a workflow or calling the model.
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
	// Mark conv.ID recently consolidated with HasMore=false so no workflow should start at all.
	rawDone, _ := json.Marshal(agent.ConsolidationState{
		LastRunAt:      now.Add(2 * time.Hour),
		LastAttemptAt:  now.Add(2 * time.Hour),
		LastEntryCount: 3,
	})
	if err := repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), rawDone); err != nil {
		t.Fatal(err)
	}
	cl4 := &mocks.Client{}
	r.Client = cl4
	if err := r.DispatchMemoryConsolidation(ctx, now.Add(2*time.Hour+15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cl4.AssertNotCalled(t, "ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
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

		// BatchSize=2, MaxBatches=1: Run 0 processes pref-00..01, Run 1 processes pref-02, Run 2 processes fact-00.
		threeRuns := func(ctx workflow.Context, in MemoryConsolidationInput) error {
			for i := range 3 {
				childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: fmt.Sprintf("phase1-run-%d", i)})
				if err := workflow.ExecuteChildWorkflow(childCtx, MemoryConsolidationWorkflow, in).Get(ctx, nil); err != nil {
					return err
				}
			}
			return nil
		}
		env := newEnv(a)
		env.RegisterWorkflow(MemoryConsolidationWorkflow)
		env.ExecuteWorkflow(threeRuns, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled", BatchSize: 2, MaxBatches: 1})
		requireWorkflowOK(t, env)

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
			return domain.Message{}, temporal.NewNonRetryableApplicationError("provider outage", "provider_outage", errors.New("provider outage"))
		}}

		env := newEnv(a)
		env.ExecuteWorkflow(MemoryConsolidationWorkflow, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled"})
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() == nil {
			t.Fatalf("expected workflow error on hygiene batch failure, got %v", env.GetWorkflowError())
		}
		reports, err := repo.ListMemoryConsolidations(ctx, conv.ID, 10)
		if err != nil || len(reports) != 1 {
			t.Fatalf("expected failed report persisted in Postgres, got %+v %v", reports, err)
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
				return domain.Message{}, temporal.NewNonRetryableApplicationError("sub-chunk 1 failed", "subchunk_err", errors.New("sub-chunk 1 failed"))
			}},
		}

		env := newEnv(a)
		env.ExecuteWorkflow(MemoryConsolidationWorkflow, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled"})
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() == nil {
			t.Fatalf("expected error on sub-chunk 1 failure, got %v", env.GetWorkflowError())
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

		// 61 preferences: duplicates at position 0 (pref-00) and position 60 (pref-60).
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

		env := newEnv(a)
		env.ExecuteWorkflow(MemoryConsolidationWorkflow, MemoryConsolidationInput{Scope: conv.ID, Trigger: "scheduled", BatchSize: 60, MaxBatches: 5})
		requireWorkflowOK(t, env)

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

// splitBudgetModel forces a 2-entry batch to split into two 1-entry sub-chunks.
type splitBudgetModel struct {
	*scriptedModel
}

func (m *splitBudgetModel) FitsContext(_ context.Context, messages []domain.Message, _ []domain.Tool) (bool, error) {
	return !(strings.Contains(messages[1].Content, `"sub-0"`) && strings.Contains(messages[1].Content, `"sub-1"`)), nil
}
