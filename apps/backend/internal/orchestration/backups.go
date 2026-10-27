package orchestration

import (
	"context"
	"encoding/json"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/services/backup"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"
)

func (r *Runtime) DispatchBackupEvents(ctx context.Context) error {
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
			if time.Since(event.At) < 48*time.Hour {
				id := "backup-event/" + agent.ID(c.ID, event.ID)
				opts := workflowOptions(id, r.TaskQueue)
				opts.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
				_, err = r.Client.ExecuteWorkflow(ctx, opts, BackupEventWorkflow, BackupEventInput{ConversationID: c.ID, Event: event, MaxSteps: r.MaxSteps})
				if err != nil && !already(err) {
					return err
				}
			}
			if err = r.Repo.SaveCheckpoint(ctx, key, json.RawMessage(`true`)); err != nil {
				return err
			}
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

func BackupEventWorkflow(ctx workflow.Context, in BackupEventInput) error {
	ctx = options(ctx)
	id := "backup-event/" + agent.ID(in.ConversationID, in.Event.ID)
	turn, runErr := runTurn(ctx, agent.RunInput{Key: id, SessionID: id, ConversationID: in.ConversationID, Initiative: true, BackupEvent: &in.Event}, in.MaxSteps)
	if runErr != nil && !turn.Sent {
		if err := workflow.ExecuteActivity(ctx, "BackupAlert", agent.BackupAlertInput{ConversationID: in.ConversationID, Event: in.Event}).Get(ctx, nil); err != nil {
			return err
		}
	}
	if err := workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, runErr)).Get(ctx, nil); err != nil {
		return err
	}
	return workflow.ExecuteActivity(ctx, "CloseSession", id).Get(ctx, nil)
}
