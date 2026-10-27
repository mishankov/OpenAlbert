package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestMaintenanceRepository(t *testing.T) {
	t.Run("memory", func(t *testing.T) { maintenanceContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL to test PostgreSQL persistence")
		}
		repo, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		maintenanceContract(t, repo)
	})
}

func maintenanceContract(t *testing.T, repo domain.Repository) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		mutate  func(*domain.Task, *domain.Task, *domain.Task)
		allowed bool
	}{
		{name: "repair", allowed: true},
		{name: "no grant", mutate: func(a, b, c *domain.Task) { a.Maintenance = false }},
		{name: "grant revoked by pause", mutate: func(a, b, c *domain.Task) { a.Status = "paused" }},
		{name: "other maintenance task", mutate: func(a, b, c *domain.Task) { b.Maintenance = true; c.Maintenance = true }},
		{name: "active target", mutate: func(a, b, c *domain.Task) { b.Status = "running" }},
		{name: "force run", mutate: func(a, b, c *domain.Task) { c.Status = "running" }},
		{name: "past anchor", mutate: func(a, b, c *domain.Task) { c.NextRunAt = time.Now().Add(-time.Hour) }},
		{name: "goal", mutate: func(a, b, c *domain.Task) { c.Goal = "injected" }},
		{name: "grant escalation", mutate: func(a, b, c *domain.Task) { c.Maintenance = true }},
		{name: "interval", mutate: func(a, b, c *domain.Task) { c.ScheduleSeconds = 60 }},
		{name: "other scope", mutate: func(a, b, c *domain.Task) { a.ConversationID += "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := fmt.Sprintf("maintenance-%d", time.Now().UnixNano())
			key := scope + "-mutation"
			actor := domain.Task{ID: scope + "-actor", ConversationID: scope, Status: "running", Maintenance: true}
			before := domain.Task{ID: scope + "-target", ConversationID: scope, Status: "waiting", Goal: "original", ScheduleSeconds: 86400, NextRunAt: time.Now().Add(time.Hour).UTC()}
			requested := before
			requested.NextRunAt = time.Now().Add(24 * time.Hour).UTC()
			requested.Status = "queued"
			if tc.mutate != nil {
				tc.mutate(&actor, &before, &requested)
			}
			var err error
			actor, err = repo.CreateTask(ctx, actor)
			if err != nil {
				t.Fatal(err)
			}
			before, err = repo.CreateTask(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			requested.Version = before.Version
			requested.CreatedAt = before.CreatedAt
			requested.UpdatedAt = before.UpdatedAt
			got, err := repo.MaintainTask(ctx, actor.ID, "run-1", key, requested)
			after, _, _ := repo.Task(ctx, scope, before.ID)
			raw, found, auditErr := repo.LoadCheckpoint(ctx, "task-maintenance/"+key)
			if !tc.allowed {
				if err == nil || !reflect.DeepEqual(before, after) || found || auditErr != nil {
					t.Fatalf("denied mutation changed state: err=%v before=%+v after=%+v audit=%s", err, before, after, raw)
				}
				return
			}
			if err != nil || auditErr != nil || !found || got.Version != before.Version+1 || !got.NextRunAt.Equal(requested.NextRunAt) || got.Status != "queued" {
				t.Fatalf("repair failed: %+v %v %v %s", got, err, auditErr, raw)
			}
			var audit TaskMaintenanceAudit
			if err = json.Unmarshal(raw, &audit); err != nil {
				t.Fatal(err)
			}
			if audit.ActorTaskID != actor.ID || audit.TargetTaskID != before.ID || audit.RunID != "run-1" || audit.BeforeVersion != before.Version || audit.AfterVersion != got.Version || !audit.BeforeNextRunAt.Equal(before.NextRunAt) || !audit.AfterNextRunAt.Equal(got.NextRunAt) {
				t.Fatalf("incomplete audit: %+v", audit)
			}
			// Lost activity acknowledgement: replay the committed mutation without a second write.
			retried, err := repo.MaintainTask(ctx, actor.ID, "run-1", key, requested)
			if err != nil || !reflect.DeepEqual(got, retried) {
				t.Fatalf("retry mutated target: %+v %v", retried, err)
			}
			// A second concurrent repair must reload; stale writes produce no audit entry.
			if _, err = repo.MaintainTask(ctx, actor.ID, "run-2", key+"-stale", requested); err != ErrConflict {
				t.Fatalf("stale repair: %v", err)
			}
			if _, found, _ := repo.LoadCheckpoint(ctx, "task-maintenance/"+key+"-stale"); found {
				t.Fatal("failed repair wrote audit")
			}
		})
	}
	t.Run("concurrent repairs", func(t *testing.T) {
		scope := fmt.Sprintf("concurrent-maintenance-%d", time.Now().UnixNano())
		actor, err := repo.CreateTask(ctx, domain.Task{ID: scope + "-actor", ConversationID: scope, Status: "running", Maintenance: true})
		if err != nil {
			t.Fatal(err)
		}
		before, err := repo.CreateTask(ctx, domain.Task{ID: scope + "-target", ConversationID: scope, Status: "waiting", NextRunAt: time.Now().Add(time.Hour).UTC()})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Go(func() {
				next := before
				next.NextRunAt = before.NextRunAt.Add(time.Hour)
				_, err := repo.MaintainTask(ctx, actor.ID, "run", fmt.Sprintf("%s-%d", scope, i), next)
				results <- err
			})
		}
		wg.Wait()
		close(results)
		success, conflicts := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if err == ErrConflict {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || conflicts != 1 {
			t.Fatalf("success=%d conflicts=%d", success, conflicts)
		}
	})
}
