package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/services/backup"
)

func backupDefinition() domain.Tool {
	return domain.Tool{Name: "backup_status", Description: "Read host-reported backup schedule, current phase, last verified successful S3 backup and saved dates. Owner private chat only. This is a local status snapshot with observation timestamps, not a live S3 query. Unknown/stale status is not success. Does not start, change, delete or restore backups. Never exposes credentials.", Parameters: schema(``)}
}

type BackupAlertInput struct {
	ConversationID string
	Event          backup.Event
}

// BackupAlert is a fallback when the model itself is unavailable. It uses the
// ordinary idempotent outbox instead of making backup failures depend on an LLM.
func (a *Activities) BackupAlert(ctx context.Context, in BackupAlertInput) error {
	if in.Event.Kind != "failed" && in.Event.Kind != "overdue" {
		return nil
	}
	conv, found, err := a.Repo.Conversation(ctx, in.ConversationID)
	if err != nil {
		return err
	}
	if !found || a.backupAccess(conv) != nil {
		return errors.New("backup alert requires the owner's private conversation")
	}
	if time.Since(in.Event.At) > 48*time.Hour {
		return nil
	}
	s := a.Backups.View(time.Now())
	if s.LastSuccess != nil && s.LastSuccess.CreatedAt.After(in.Event.At) {
		return nil
	}
	text := "Не удалось завершить резервное копирование."
	if in.Event.Kind == "overdue" {
		text = "Нет подтверждения своевременного резервного копирования."
	}
	if s.LastSuccess != nil {
		text += fmt.Sprintf(" Последняя проверенная копия: %s.", s.LastSuccess.CreatedAt.Format(time.RFC3339))
	} else {
		text += " Время последней успешной копии неизвестно."
	}
	text += " Подробности доступны в журнале службы openalbert-backup на сервере."
	return a.Repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: ID("backup-alert", conv.ID, in.Event.ID), Conversation: conv, Text: text, State: "pending", Trigger: "backup", EnqueuedAt: time.Now().UTC(), PartCount: 1})
}

func (a *Activities) backupAccess(c domain.Conversation) error {
	if a.Backups == nil || !a.isOwnerPrivate(c) {
		return errors.New("backup status is only available in the owner's private chat when configured")
	}
	return nil
}

func (a *Activities) backupStatus(in ToolInput) (any, error) {
	if err := a.backupAccess(in.Conversation); err != nil {
		return nil, err
	}
	if string(in.Call.Arguments) != "{}" && string(in.Call.Arguments) != "" {
		return nil, errors.New("backup_status takes no arguments")
	}
	return a.Backups.View(time.Now()), nil
}
