package orchestration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

func TestTenDailyTaskRunsKeepSlotAndDeliverOnce(t *testing.T) {
	for _, calendar := range []bool{false, true} {
		name := "interval"
		if calendar {
			name = "calendar"
		}
		t.Run(name, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			start := time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)
			task := domain.Task{ID: "daily", ConversationID: conv.ID, Status: "waiting", ScheduleSeconds: 86400, NextRunAt: start}
			if calendar {
				task.ScheduleSeconds = 0
				task.ScheduleCron = "0 17 * * *"
				task.ScheduleTimezone = "UTC"
			}
			if _, err := repo.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			env := newEnv(a)
			env.SetStartTime(start)
			var starts []time.Time
			var advances int
			env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
				if info.ActivityType.Name == "BeginTask" {
					starts = append(starts, env.Now())
				}
			})
			env.SetOnActivityCompletedListener(func(info *activity.Info, result converter.EncodedValue, err error) {
				if info.ActivityType.Name != "AdvanceTask" || err != nil {
					return
				}
				var saved domain.Task
				if err := result.Get(&saved); err != nil {
					t.Fatal(err)
				}
				advances++
				want := start.AddDate(0, 0, advances)
				if !saved.NextRunAt.Equal(want) {
					t.Errorf("run %d next slot=%v want=%v", advances, saved.NextRunAt, want)
				}
			})
			// Simulate 143 seconds of generation on each run with the virtual clock.
			env.OnActivity("ModelCall", mock.Anything, mock.Anything).After(143*time.Second).Return(agent.ModelOutput{Message: reply("Synthetic daily result")}, nil)
			// Pause after the tenth completion, keeping its persisted next slot available for assertions.
			env.RegisterDelayedCallback(func() {
				saved, _, err := repo.Task(ctx, conv.ID, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				saved.Status = "paused"
				if _, err = repo.UpdateTask(ctx, saved); err != nil {
					t.Fatal(err)
				}
				env.SignalWorkflow(TaskSignal, task.ID)
			}, 9*24*time.Hour+5*time.Minute)
			env.ExecuteWorkflow(TaskWorkflow, TaskInput{ConversationID: conv.ID, ID: task.ID, MaxSteps: 4})
			requireWorkflowOK(t, env)
			if len(starts) != 10 || advances != 10 {
				t.Fatalf("starts=%d advances=%d", len(starts), advances)
			}
			for i, got := range starts {
				want := start.AddDate(0, 0, i)
				delta := got.Sub(want)
				if delta < 0 || delta > time.Second {
					t.Fatalf("run %d start=%v want=%v", i, got, want)
				}
			}
			rows, err := repo.PendingOutgoing(ctx, 20)
			if err != nil || len(rows) != 10 {
				t.Fatalf("deliveries=%d err=%v", len(rows), err)
			}
			turns := map[string]bool{}
			for _, row := range rows {
				if turns[row.TurnID] {
					t.Fatalf("duplicate delivery for turn %s", row.TurnID)
				}
				turns[row.TurnID] = true
			}
			var sends atomic.Int32
			runtime, deliveryRepo, _ := deliveryRuntime(t, func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			})
			deliveryRepo.Repository = repo
			var logs lockedLog
			runtime.Logger = telemetry.New(&logs)
			for i := 0; i < 2; i++ {
				if err := runtime.DeliverOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if sends.Load() != 10 {
				t.Fatalf("Telegram sends=%d", sends.Load())
			}
			sentByTurn := map[string]int{}
			for _, line := range strings.Split(strings.TrimSpace(logs.snapshot()), "\n") {
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				if row["operation"] == "telegram.deliver" && row["delivery_state"] == "sent" {
					sentByTurn[row["turn_id"].(string)]++
				}
			}
			if len(sentByTurn) != 10 {
				t.Fatalf("sent delivery logs: %v", sentByTurn)
			}
			for turn, n := range sentByTurn {
				if n != 1 {
					t.Fatalf("turn %s delivered %d times", turn, n)
				}
			}
		})
	}
}

func TestTaskRecoveryCollapsesThreeMissedDays(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "successful recovery"
		if failed {
			name = "failed recovery"
		}
		t.Run(name, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			anchor := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
			restart := anchor.Add(3*24*time.Hour + time.Minute)
			if _, err := repo.CreateTask(ctx, domain.Task{ID: "overdue", ConversationID: conv.ID, Status: "waiting", ScheduleSeconds: 86400, NextRunAt: anchor}); err != nil {
				t.Fatal(err)
			}
			env := newEnv(a)
			env.SetStartTime(restart)
			if failed {
				env.OnActivity("ModelCall", mock.Anything, mock.Anything).Return(agent.ModelOutput{}, temporal.NewNonRetryableApplicationError("synthetic failure", "test_failure", nil)).Once()
			} else {
				a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) { return finish("Checked"), nil }}
			}
			starts := 0
			env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
				if info.ActivityType.Name == "BeginTask" {
					starts++
				}
			})
			env.RegisterDelayedCallback(func() {
				saved, _, err := repo.Task(ctx, conv.ID, "overdue")
				if err != nil {
					t.Fatal(err)
				}
				if saved.Status != "waiting" || !saved.NextRunAt.Equal(anchor.Add(4*24*time.Hour)) {
					t.Errorf("recovery lost schedule: %+v", saved)
				}
				saved.Status = "paused"
				if _, err = repo.UpdateTask(ctx, saved); err != nil {
					t.Fatal(err)
				}
				env.SignalWorkflow(TaskSignal, saved.ID)
			}, 5*time.Minute)
			env.ExecuteWorkflow(TaskWorkflow, TaskInput{ConversationID: conv.ID, ID: "overdue", MaxSteps: 4})
			requireWorkflowOK(t, env)
			if starts != 1 {
				t.Fatalf("catch-up runs=%d", starts)
			}
		})
	}
}
