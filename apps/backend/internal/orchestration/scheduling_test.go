package orchestration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
			clk := NewFakeClock(start)
			var mu sync.Mutex
			var starts []time.Time
			var advances int
			advancedCh := make(chan int, 20)

			r := &Runtime{
				Repo:       repo,
				Activities: a,
				MaxSteps:   4,
				Clock:      clk,
				Logger:     a.Logger,
				hooks: &activityHooks{
					onStart: func(name string, _ any) {
						if name == "BeginTask" {
							mu.Lock()
							starts = append(starts, clk.Now())
							mu.Unlock()
						}
					},
					modelCall: func(_ context.Context, in agent.ModelInput, _ func(context.Context, agent.ModelInput) (agent.ModelOutput, error)) (agent.ModelOutput, error) {
						if in.Step == 0 {
							// Simulate 143 seconds of generation on each run with the virtual clock.
							clk.Advance(143 * time.Second)
							return agent.ModelOutput{Message: reply("Synthetic daily result")}, nil
						}
						return agent.ModelOutput{Message: finish("")}, nil
					},
					onComplete: func(name string, result any, err error) {
						if name != "AdvanceTask" || err != nil {
							return
						}
						saved, ok := result.(domain.Task)
						if !ok {
							t.Errorf("unexpected AdvanceTask result %T", result)
							return
						}
						mu.Lock()
						advances++
						n := advances
						mu.Unlock()
						want := start.AddDate(0, 0, n)
						if !saved.NextRunAt.Equal(want) {
							t.Errorf("run %d next slot=%v want=%v", n, saved.NextRunAt, want)
						}
						advancedCh <- n
					},
				},
			}

			schedCtx, stopSched := context.WithCancel(ctx)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.runTaskScheduler(schedCtx)
			}()

			for day := 0; day < 10; day++ {
				select {
				case <-advancedCh:
				case <-time.After(5 * time.Second):
					stopSched()
					t.Fatalf("timed out waiting for daily run %d", day+1)
				}
				waitUntil(t, func() bool {
					r.mu.Lock()
					active := r.activeTasks[task.ID]
					r.mu.Unlock()
					return !active
				})
				if day < 9 {
					clk.Set(start.AddDate(0, 0, day+1))
					_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)
				} else {
					// Pause after the tenth completion, keeping its persisted next slot available.
					saved, _, err := repo.Task(ctx, conv.ID, task.ID)
					if err != nil {
						t.Fatal(err)
					}
					saved.Status = "paused"
					if _, err = repo.UpdateTask(ctx, saved); err != nil {
						t.Fatal(err)
					}
					_ = r.NotifyTaskChanged(ctx, conv.ID, task.ID)
					clk.Advance(5 * time.Minute)
				}
			}

			stopSched()
			r.Wait()

			mu.Lock()
			gotStarts := append([]time.Time(nil), starts...)
			gotAdvances := advances
			mu.Unlock()
			if len(gotStarts) != 10 || gotAdvances != 10 {
				t.Fatalf("starts=%d advances=%d", len(gotStarts), gotAdvances)
			}
			for i, got := range gotStarts {
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
			clk := NewFakeClock(restart)
			var starts atomic.Int32
			advancedCh := make(chan struct{}, 4)

			hooks := &activityHooks{
				onStart: func(name string, _ any) {
					if name == "BeginTask" {
						starts.Add(1)
					}
				},
				onComplete: func(name string, _ any, err error) {
					if name == "AdvanceTask" && err == nil {
						advancedCh <- struct{}{}
					}
				},
			}
			if failed {
				hooks.modelCall = func(context.Context, agent.ModelInput, func(context.Context, agent.ModelInput) (agent.ModelOutput, error)) (agent.ModelOutput, error) {
					return agent.ModelOutput{}, domain.NewNonRetryableError("synthetic failure", "test_failure", nil)
				}
			} else {
				a.Model = &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) { return finish("Checked"), nil }}
			}

			r := &Runtime{Repo: repo, Activities: a, MaxSteps: 4, Clock: clk, Logger: a.Logger, hooks: hooks}
			schedCtx, stopSched := context.WithCancel(ctx)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.runTaskScheduler(schedCtx)
			}()

			select {
			case <-advancedCh:
			case <-time.After(5 * time.Second):
				stopSched()
				t.Fatal("timed out waiting for recovery run")
			}
			waitUntil(t, func() bool {
				r.mu.Lock()
				active := r.activeTasks["overdue"]
				r.mu.Unlock()
				return !active
			})
			clk.Advance(5 * time.Minute)
			_ = r.NotifyTaskChanged(ctx, conv.ID, "overdue")
			time.Sleep(10 * time.Millisecond)

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
			_ = r.NotifyTaskChanged(ctx, conv.ID, saved.ID)
			stopSched()
			r.Wait()

			if starts.Load() != 1 {
				t.Fatalf("catch-up runs=%d", starts.Load())
			}
		})
	}
}
