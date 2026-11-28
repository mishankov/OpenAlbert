package orchestration

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mishankov/OpenAlbert/shared/backup"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/telemetry"
)

func (r *Runtime) DispatchBackupEvents(ctx context.Context) error {
	r.ensureInit()
	if r.Activities == nil || r.Activities.Backups == nil {
		return nil
	}
	events, err := r.Activities.Backups.Events()
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	conversations, err := r.Repo.ListPrivateConversations(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	for _, c := range conversations {
		if !c.Private || c.OwnerID != r.Activities.OwnerUserID || c.ChatID != r.Activities.OwnerUserID {
			continue
		}
		started := 0
		for _, event := range events {
			key := "backup-dispatch/" + agent.ID(c.ID, event.ID)
			if _, done, e := r.Repo.LoadCheckpoint(ctx, key); e != nil {
				return e
			} else if done {
				continue
			}
			// Do not replay months of maintenance notifications after a long outage.
			if now.Sub(event.At) >= 48*time.Hour {
				if err = r.Repo.SaveCheckpoint(ctx, key, json.RawMessage(`true`)); err != nil {
					return err
				}
				continue
			}
			in := BackupEventInput{ConversationID: c.ID, Event: event, MaxSteps: r.maxSteps()}
			if r.hooks != nil && r.hooks.dispatchBackup != nil && r.hooks.dispatchBackup(in) {
				if err = r.Repo.SaveCheckpoint(ctx, key, json.RawMessage(`true`)); err != nil {
					return err
				}
				started++
				if started >= 20 {
					break
				}
				continue
			}
			id := "backup-event/" + agent.ID(c.ID, event.ID)
			r.mu.Lock()
			if r.activeBackups[id] {
				r.mu.Unlock()
				continue
			}
			r.activeBackups[id] = true
			runCtx := r.bgCtxLocked(ctx)
			r.mu.Unlock()
			r.wg.Add(1)
			go func(eventID, checkpointKey string, input BackupEventInput) {
				defer r.wg.Done()
				defer func() {
					r.mu.Lock()
					delete(r.activeBackups, eventID)
					r.mu.Unlock()
				}()
				if err := r.RunBackupEvent(runCtx, input); err != nil {
					if runCtx.Err() == nil {
						r.record(runCtx, "backup.event", err)
					}
					return
				}
				cleanup, cancel := disconnectedCtx(runCtx)
				defer cancel()
				if err := r.Repo.SaveCheckpoint(cleanup, checkpointKey, json.RawMessage(`true`)); err != nil {
					r.record(cleanup, "backup.event.checkpoint", err)
				}
			}(id, key, in)
			started++
			if started >= 20 {
				break
			}
		}
		break
	}
	return nil
}

type BackupEventInput struct {
	ConversationID string
	Event          backup.Event
	MaxSteps       int
}

// RunBackupEvent evaluates one host backup event, falls back to BackupAlert on
// model outage, persists Finish, and closes the background session.
func (r *Runtime) RunBackupEvent(ctx context.Context, in BackupEventInput) error {
	r.ensureInit()
	id := "backup-event/" + agent.ID(in.ConversationID, in.Event.ID)
	ctx = telemetry.WithFields(ctx, map[string]any{"workflow_id": id})
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = r.maxSteps()
	}
	turn, runErr := r.runTurn(ctx, agent.RunInput{
		Key:            id,
		SessionID:      id,
		ConversationID: in.ConversationID,
		Initiative:     true,
		BackupEvent:    &in.Event,
	}, maxSteps)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cleanup, cancel := disconnectedCtx(ctx)
	defer cancel()
	if runErr != nil && !turn.Sent {
		if err := r.actBackupAlert(cleanup, agent.BackupAlertInput{ConversationID: in.ConversationID, Event: in.Event}); err != nil {
			return err
		}
	}
	if err := r.actFinish(cleanup, finishInput(turn, runErr)); err != nil {
		return err
	}
	return r.actCloseSession(cleanup, id)
}
