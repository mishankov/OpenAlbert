package storage

import (
	"errors"
	"time"

	"openalbert/apps/backend/internal/domain"
)

// TaskMaintenanceAudit is stored atomically with the mutation in checkpoints.
// It contains scheduling metadata only, never task goals or message bodies.
type TaskMaintenanceAudit struct {
	ActorTaskID     string    `json:"actor_task_id"`
	TargetTaskID    string    `json:"target_task_id"`
	ConversationID  string    `json:"conversation_id"`
	RunID           string    `json:"run_id"`
	MutationID      string    `json:"mutation_id"`
	BeforeStatus    string    `json:"before_status"`
	AfterStatus     string    `json:"after_status"`
	BeforeNextRunAt time.Time `json:"before_next_run_at"`
	AfterNextRunAt  time.Time `json:"after_next_run_at"`
	BeforeVersion   int64     `json:"before_version"`
	AfterVersion    int64     `json:"after_version"`
	ChangedAt       time.Time `json:"changed_at"`
}

func validateMaintenance(actor, before, after domain.Task, now time.Time) error {
	if !actor.Maintenance || actor.Status != "running" || actor.ConversationID != before.ConversationID {
		return errors.New("active maintenance task required in the same conversation")
	}
	if actor.ID == before.ID || before.Maintenance || before.Status == "running" || after.Status == "running" {
		return errors.New("maintenance cannot update itself, another maintenance task or an active run")
	}
	if after.Status == "" {
		return errors.New("task status is required")
	}
	if before.Version != after.Version {
		return ErrConflict
	}
	// Defense in depth: this repository operation cannot carry unrelated changes.
	check := after
	check.Status, check.NextRunAt, check.UpdatedAt = before.Status, before.NextRunAt, before.UpdatedAt
	if check != before {
		return errors.New("maintenance may only update next_run_at and status")
	}
	if (after.Status == "queued" || after.Status == "waiting") && !after.NextRunAt.After(now) {
		return errors.New("maintenance must schedule resumed tasks strictly in the future")
	}
	return validateTask(after)
}

func maintenanceAudit(actorID, runID, key string, before, after domain.Task) TaskMaintenanceAudit {
	return TaskMaintenanceAudit{ActorTaskID: actorID, TargetTaskID: after.ID, ConversationID: after.ConversationID, RunID: runID, MutationID: key,
		BeforeStatus: before.Status, AfterStatus: after.Status, BeforeNextRunAt: before.NextRunAt, AfterNextRunAt: after.NextRunAt,
		BeforeVersion: before.Version, AfterVersion: after.Version, ChangedAt: after.UpdatedAt}
}
