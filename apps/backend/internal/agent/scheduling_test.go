package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestAdvanceTaskPreservesSlotsAndRetriesAfterFailure(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failed, explicit bool
		status           string
	}{
		{name: "success"}, {name: "failed run", failed: true}, {name: "explicit anchor", explicit: true},
		{name: "paused wins", status: "paused"}, {name: "cancelled wins", status: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			slot := time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)
			task, err := repo.CreateTask(ctx, domain.Task{ID: "daily", ConversationID: conv.ID, Status: "waiting", ScheduleSeconds: 86400, NextRunAt: slot})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.BeginTask(ctx, conv.ID, task.ID, slot.Add(194*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			want := slot.Add(24 * time.Hour)
			if tc.explicit {
				raw, _ := json.Marshal(map[string]string{"id": task.ID, "next_run_at": want.Format(time.RFC3339), "status": "waiting"})
				if out, err := a.ToolCall(ctx, ToolInput{RunKey: "daily", TaskID: task.ID, Conversation: conv, Call: domain.ToolCall{ID: "anchor", Name: "task_update", Arguments: raw}}); err != nil || ToolFailed(out) {
					t.Fatalf("%s %v", out, err)
				}
			}
			if tc.status != "" {
				current, _, _ := repo.Task(ctx, conv.ID, task.ID)
				current.Status = tc.status
				if _, err := repo.UpdateTask(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			for retry := 0; retry < 2; retry++ {
				got, err := a.AdvanceTask(ctx, conv.ID, task.ID, tc.failed, slot.Add(143*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				if tc.status != "" {
					if got.Status != tc.status || !got.NextRunAt.Equal(slot) {
						t.Fatalf("stopped task changed: %+v", got)
					}
					continue
				}
				if got.Status != "waiting" || !got.NextRunAt.Equal(want) {
					t.Fatalf("wrong next slot on retry %d: %+v", retry, got)
				}
			}
		})
	}
}

func TestCalendarTaskToolConfiguration(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	in := ToolInput{RunKey: "create-calendar", Conversation: conv, Call: domain.ToolCall{ID: "create", Name: "task_create", Arguments: json.RawMessage(`{"title":"Daily","goal":"Synthetic","schedule_cron":"0 20 * * *","schedule_timezone":"Europe/Moscow"}`)}}
	out, err := a.ToolCall(ctx, in)
	if err != nil || ToolFailed(out) {
		t.Fatalf("%s %v", out, err)
	}
	var task domain.Task
	if err = json.Unmarshal([]byte(out), &task); err != nil {
		t.Fatal(err)
	}
	if task.NextRunAt.Hour() != 17 || task.NextRunAt.Minute() != 0 || task.NextRunAt.Second() != 0 || task.NextRunAt.Nanosecond() != 0 || !task.NextRunAt.After(time.Now()) {
		t.Fatalf("bad initial slot: %+v", task)
	}
	original := task.NextRunAt
	in.Call = domain.ToolCall{ID: "update", Name: "task_update", Arguments: json.RawMessage(`{"id":"` + task.ID + `","schedule_cron":"0 17 * * *","schedule_timezone":"UTC"}`)}
	out, err = a.ToolCall(ctx, in)
	if err != nil || ToolFailed(out) {
		t.Fatalf("%s %v", out, err)
	}
	saved, _, _ := repo.Task(ctx, conv.ID, task.ID)
	if !saved.NextRunAt.Equal(original) {
		t.Fatalf("configuration reset current slot: %+v", saved)
	}
}

func TestMissingLegacySlotAnchorsAtBegin(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			task, err := repo.CreateTask(ctx, domain.Task{ID: "legacy", ConversationID: conv.ID, Status: status, ScheduleSeconds: 86400})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)
			began, err := a.BeginTask(ctx, conv.ID, task.ID, start)
			if err != nil || !began.NextRunAt.Equal(start) {
				t.Fatalf("missing start anchor: %+v %v", began, err)
			}
			saved, err := a.AdvanceTask(ctx, conv.ID, task.ID, false, start.Add(143*time.Second))
			if err != nil || !saved.NextRunAt.Equal(start.Add(24*time.Hour)) {
				t.Fatalf("completion changed phase: %+v %v", saved, err)
			}
		})
	}
}
