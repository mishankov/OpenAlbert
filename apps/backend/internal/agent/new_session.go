package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"openalbert/apps/backend/internal/domain"
)

// StartNewSession is retried with stable session, inbox and outbox IDs. The
// workflow waits for the interrupted turn's Finish before changing context.
func (a *Activities) StartNewSession(ctx context.Context, scope, previous, next string, ids []string) (bool, error) {
	if len(ids) == 0 {
		return false, errors.New("new session requires a command")
	}
	// Remember which session received this command. A stale dispatcher snapshot
	// can signal it again after the workflow has completed and a new one starts.
	key := "new-session/" + ids[len(ids)-1]
	if raw, found, err := a.Repo.LoadCheckpoint(ctx, key); err != nil {
		return false, err
	} else if found {
		var original string
		if err := json.Unmarshal(raw, &original); err != nil {
			return false, err
		}
		if original != previous {
			return false, nil
		}
	} else {
		raw, _ := json.Marshal(previous)
		if err := a.Repo.SaveCheckpoint(ctx, key, raw); err != nil {
			return false, err
		}
	}
	err := a.startNewSession(ctx, scope, previous, next, ids)
	return err == nil, err
}

func (a *Activities) startNewSession(ctx context.Context, scope, previous, next string, ids []string) error {
	conv, ok, err := a.Repo.Conversation(ctx, scope)
	if err != nil {
		return err
	}
	if !ok || len(ids) == 0 {
		return errors.New("new session requires a conversation and command")
	}
	// A command may arrive before Prepare has created the first session.
	active, err := a.Repo.ActiveSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range active {
		if session.ID == previous && session.ConversationID == scope {
			if err := a.CloseSession(ctx, previous); err != nil {
				return err
			}
		}
	}
	if err := a.Repo.StartSession(ctx, domain.Session{ID: next, ConversationID: scope, CreatedAt: time.Now().UTC()}); err != nil {
		return err
	}
	if err := a.Repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: ID("new-session", ids[len(ids)-1]), Conversation: conv,
		Text: "Начал новый диалог. О чём поговорим?", State: "pending", SessionID: next,
		Trigger: "new_session", EnqueuedAt: time.Now().UTC(), PartCount: 1}); err != nil {
		return err
	}
	for _, id := range ids {
		if err := a.Repo.SaveCheckpoint(ctx, "incoming-done/"+id, json.RawMessage(`true`)); err != nil {
			return err
		}
		if err := a.Repo.MarkIncomingHandled(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
