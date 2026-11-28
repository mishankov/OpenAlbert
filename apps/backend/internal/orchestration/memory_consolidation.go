package orchestration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

const (
	memoryConsolidationCheckInterval     = time.Minute
	memoryConsolidationThresholdCooldown = time.Hour
	memoryConsolidationBacklogCooldown   = 5 * time.Minute
)

type MemoryConsolidationInput struct {
	Scope      string
	Trigger    string
	BatchSize  int `json:",omitempty"`
	MaxBatches int `json:",omitempty"`
}

func MemoryConsolidationWorkflowID(scope string) string {
	return "memory-consolidation/" + agent.ID(scope)
}

// RunMemoryConsolidation executes the two-phase memory consolidation pipeline
// (profile/preferences followed by hygiene) and persists a final report.
func (r *Runtime) RunMemoryConsolidation(ctx context.Context, in MemoryConsolidationInput) error {
	r.ensureInit()
	wfID := MemoryConsolidationWorkflowID(in.Scope)
	ctx = telemetry.WithFields(ctx, map[string]any{"workflow_id": wfID})
	runID := agent.ID(in.Scope, rand.Text())
	startedAt := r.now().UTC()
	trigger := in.Trigger
	if trigger == "" {
		trigger = "scheduled"
	}

	prep, err := r.actPrepareMemoryConsolidation(ctx, agent.ConsolidationRunInput{
		RunID:      runID,
		Scope:      in.Scope,
		Trigger:    trigger,
		BatchSize:  in.BatchSize,
		MaxBatches: in.MaxBatches,
		Now:        startedAt,
	})
	if err != nil {
		return err
	}

	if prep.BeforeCount == 0 {
		_, err := r.actFinalizeMemoryConsolidation(ctx, agent.ConsolidationFinalizeInput{
			RunID:              runID,
			Scope:              in.Scope,
			Trigger:            trigger,
			StartedAt:          startedAt,
			CompletedAt:        r.now().UTC(),
			Status:             "skipped",
			BeforeCount:        0,
			ProfileCharsBefore: prep.ProfileCharsBefore,
		})
		return err
	}

	operations := []domain.MemoryConsolidationOperation{}
	completedBatches := 0
	p1CursorTime, p1CursorID := prep.StartPhase1Time, prep.StartPhase1ID
	p2CursorTime, p2CursorID := prep.StartPhase2Time, prep.StartPhase2ID

	recordFailure := func(runErr error, failedBatchKey string, p1HasMore, p1FollowUp, p2HasMore bool) error {
		cleanup, cancel := disconnectedCtx(ctx)
		defer cancel()
		_, _ = r.actFinalizeMemoryConsolidation(cleanup, agent.ConsolidationFinalizeInput{
			RunID:              runID,
			Scope:              in.Scope,
			Trigger:            trigger,
			StartedAt:          startedAt,
			CompletedAt:        r.now().UTC(),
			Status:             "failed",
			BeforeCount:        prep.BeforeCount,
			ProfileCharsBefore: prep.ProfileCharsBefore,
			BatchesProcessed:   completedBatches,
			NextPhase1Time:     p1CursorTime,
			NextPhase1ID:       p1CursorID,
			Phase1HasMore:      p1HasMore,
			Phase1FollowUp:     p1FollowUp,
			NextPhase2Time:     p2CursorTime,
			NextPhase2ID:       p2CursorID,
			Phase2HasMore:      p2HasMore,
			HasMore:            p1HasMore || p2HasMore,
			Operations:         operations,
			FailedBatchKey:     failedBatchKey,
			Error:              runErr.Error(),
		})
		return runErr
	}

	for i, batch := range prep.Phase1Batches {
		res, err := r.actConsolidateProfileBatch(ctx, agent.ConsolidationBatchInput{
			RunID:      runID,
			Scope:      in.Scope,
			Group:      prep.Group,
			BatchIndex: i,
			BatchIDs:   batch.IDs,
			Now:        r.now().UTC(),
		})
		if err != nil {
			p2More := !prep.Phase1FollowUp && (prep.Phase2HasMore || len(prep.Phase2Batches) > 0)
			return recordFailure(err, fmt.Sprintf("profile/%d", i), true, prep.Phase1FollowUp, p2More)
		}
		operations = append(operations, res.Operations...)
		completedBatches++
		p1CursorTime, p1CursorID = batch.LastTime, batch.LastID
	}

	if !prep.Phase1HasMore {
		p1CursorTime, p1CursorID = time.Time{}, ""
	}

	for i, batch := range prep.Phase2Batches {
		batchResult, err := r.actConsolidateHygieneBatch(ctx, agent.ConsolidationBatchInput{
			RunID:      runID,
			Scope:      in.Scope,
			Group:      prep.Group,
			BatchIndex: i,
			BatchIDs:   batch.IDs,
			Now:        r.now().UTC(),
		})
		if err != nil {
			return recordFailure(err, fmt.Sprintf("hygiene/%d", i), false, false, true)
		}
		operations = append(operations, batchResult.Operations...)
		completedBatches++
		p2CursorTime, p2CursorID = batch.LastTime, batch.LastID
	}

	status := "completed"
	if prep.HasMore {
		status = "partial"
	}
	_, err = r.actFinalizeMemoryConsolidation(ctx, agent.ConsolidationFinalizeInput{
		RunID:              runID,
		Scope:              in.Scope,
		Trigger:            trigger,
		StartedAt:          startedAt,
		CompletedAt:        r.now().UTC(),
		Status:             status,
		BeforeCount:        prep.BeforeCount,
		ProfileCharsBefore: prep.ProfileCharsBefore,
		BatchesProcessed:   completedBatches,
		NextPhase1Time:     prep.NextPhase1Time,
		NextPhase1ID:       prep.NextPhase1ID,
		Phase1HasMore:      prep.Phase1HasMore,
		Phase1FollowUp:     prep.Phase1FollowUp,
		NextPhase2Time:     prep.NextPhase2Time,
		NextPhase2ID:       prep.NextPhase2ID,
		Phase2HasMore:      prep.Phase2HasMore,
		HasMore:            prep.HasMore,
		Operations:         operations,
	})
	return err
}

func (r *Runtime) DispatchMemoryConsolidation(ctx context.Context, now time.Time) error {
	r.ensureInit()
	if r.MemoryConsolidationInterval <= 0 || r.Activities == nil || r.Activities.Knowledge == nil {
		return nil
	}
	if now.IsZero() {
		now = r.now().UTC()
	}
	conversations, err := r.Repo.ListConversations(ctx)
	if err != nil {
		return err
	}
	archiveCutoff := now.UTC().AddDate(0, 0, -agent.DefaultConsolidationKeepDays)
	for _, conv := range conversations {
		if strings.HasPrefix(conv.ID, "coding:") {
			continue
		}
		entries, err := r.Activities.Knowledge.ListMemory(ctx, conv.ID)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			// Retain archive pruning even after a scope's last live memory is deleted.
			if err := r.Activities.Knowledge.PruneArchive(ctx, conv.ID, archiveCutoff); err != nil {
				return err
			}
			continue
		}
		var state agent.ConsolidationState
		if raw, ok, err := r.Repo.LoadCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID)); err != nil {
			return err
		} else if ok {
			_ = json.Unmarshal(raw, &state)
		}

		trigger := ""
		switch {
		case state.HasMore && (state.LastAttemptAt.IsZero() || now.Sub(state.LastAttemptAt) >= memoryConsolidationBacklogCooldown):
			trigger = "backlog"
		case state.LastRunAt.IsZero() && (state.LastAttemptAt.IsZero() || now.Sub(state.LastAttemptAt) >= memoryConsolidationThresholdCooldown):
			trigger = "scheduled"
		case !state.LastRunAt.IsZero() && now.Sub(state.LastRunAt) >= r.MemoryConsolidationInterval && (state.LastAttemptAt.IsZero() || now.Sub(state.LastAttemptAt) >= memoryConsolidationThresholdCooldown):
			trigger = "scheduled"
		default:
			if state.LastAttemptAt.IsZero() || now.Sub(state.LastAttemptAt) >= memoryConsolidationThresholdCooldown {
				profile, err := r.Activities.Knowledge.Profile(ctx, conv.ID)
				if err != nil {
					return err
				}
				updatedSince := 0
				for _, entry := range entries {
					if !state.LastRunAt.IsZero() && entry.UpdatedAt.After(state.LastRunAt) {
						updatedSince++
					}
				}
				profileChars := utf8.RuneCountInString(profile)
				profileHash := agent.ID(profile)
				profileThreshold := false
				if profileChars >= agent.DefaultProfileCharThreshold {
					if state.LastProfileHash != "" {
						profileThreshold = profileChars > state.LastProfileChars ||
							(profileChars == state.LastProfileChars && profileHash != state.LastProfileHash)
					} else {
						profileThreshold = profileChars > state.LastProfileChars
					}
				}
				if profileThreshold ||
					len(entries)-state.LastEntryCount >= agent.DefaultNewEntryThreshold ||
					updatedSince >= agent.DefaultNewEntryThreshold {
					trigger = "threshold"
				}
			}
		}
		if trigger == "" {
			continue
		}

		in := MemoryConsolidationInput{
			Scope:   conv.ID,
			Trigger: trigger,
		}
		started := false
		if r.hooks != nil && r.hooks.dispatchConsol != nil && r.hooks.dispatchConsol(in) {
			started = true
		} else {
			r.mu.Lock()
			if !r.activeConsolidation[conv.ID] {
				r.activeConsolidation[conv.ID] = true
				runCtx := r.bgCtxLocked(ctx)
				r.mu.Unlock()
				started = true
				r.wg.Add(1)
				go func(scope string, input MemoryConsolidationInput) {
					defer r.wg.Done()
					defer func() {
						r.mu.Lock()
						delete(r.activeConsolidation, scope)
						r.mu.Unlock()
					}()
					if err := r.RunMemoryConsolidation(runCtx, input); err != nil && runCtx.Err() == nil {
						r.record(runCtx, "memory.consolidation", err)
					}
				}(conv.ID, in)
			} else {
				r.mu.Unlock()
			}
		}
		if started {
			state.LastAttemptAt = now.UTC()
			if raw, marshalErr := json.Marshal(state); marshalErr == nil {
				if saveErr := r.Repo.SaveCheckpoint(ctx, agent.ConsolidationStateKey(conv.ID), raw); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	return nil
}
