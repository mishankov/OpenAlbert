package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

func TestMaintenanceToolRightsAndAudit(t *testing.T) {
	for _, tc := range []struct {
		name, args           string
		maintenance, allowed bool
	}{
		{"repair", `"status":"waiting","next_run_at":"2030-01-01T17:00:00Z"`, true, true},
		{"ordinary task", `"next_run_at":"2030-01-01T17:00:00Z"`, false, false},
		{"change goal", `"goal":"new"`, true, false},
		{"change result", `"result":"new"`, true, false},
		{"change interval", `"schedule_seconds":7200`, true, false},
		{"grant authority", `"maintenance":true`, true, false},
		{"unknown field", `"recurse":true`, true, false},
		{"past slot", `"next_run_at":"2000-01-01T00:00:00Z"`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			var logs bytes.Buffer
			a.Logger = telemetry.New(&logs)
			if _, err := repo.CreateTask(ctx, domain.Task{ID: "watchdog", ConversationID: conv.ID, Status: "running", Maintenance: tc.maintenance}); err != nil {
				t.Fatal(err)
			}
			before, err := repo.CreateTask(ctx, domain.Task{ID: "preview", ConversationID: conv.ID, Status: "waiting", Goal: "keep", NextRunAt: time.Now().Add(time.Hour), ScheduleSeconds: 86400})
			if err != nil {
				t.Fatal(err)
			}
			notifications := 0
			a.TaskChanged = func(context.Context, string, string) error { notifications++; return nil }
			in := ToolInput{RunKey: "maintenance-run", Conversation: conv, TaskID: "watchdog", Call: domain.ToolCall{ID: "repair", Name: "task_update", Arguments: json.RawMessage(`{"id":"preview",` + tc.args + `}`)}}
			for retry := 0; retry < 2; retry++ {
				out, err := a.ToolCall(ctx, in)
				if err != nil || ToolFailed(out) == tc.allowed {
					t.Fatalf("%s %v", out, err)
				}
			}
			after, _, _ := repo.Task(ctx, conv.ID, before.ID)
			if !tc.allowed {
				if !reflect.DeepEqual(before, after) || notifications != 0 {
					t.Fatalf("denied mutation changed task: %+v", after)
				}
				return
			}
			if notifications != 1 || after.Version != before.Version+1 || !strings.Contains(logs.String(), `"operation":"task.maintenance"`) {
				t.Fatalf("missing audit or duplicated update: %+v notifications=%d logs=%s", after, notifications, logs.String())
			}
		})
	}
}

func TestMaintenanceGrantRequiresInteractiveRequest(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	task, err := repo.CreateTask(ctx, domain.Task{ID: "watchdog", ConversationID: conv.ID, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, taskID        string
		background, allowed bool
	}{
		{name: "interactive", allowed: true}, {name: "self", taskID: task.ID}, {name: "initiative", background: true},
	} {
		in := ToolInput{RunKey: tc.name, Conversation: conv, TaskID: tc.taskID, Background: tc.background, Call: domain.ToolCall{ID: "grant", Name: "task_update", Arguments: json.RawMessage(`{"id":"watchdog","maintenance":true}`)}}
		out, err := a.ToolCall(ctx, in)
		if err != nil || ToolFailed(out) == tc.allowed {
			t.Fatalf("%s: %s %v", tc.name, out, err)
		}
	}
	// Initiative must not create a privileged task, either.
	out, err := a.ToolCall(ctx, ToolInput{RunKey: "initiative-create", Background: true, Conversation: conv, Call: domain.ToolCall{ID: "create", Name: "task_create", Arguments: json.RawMessage(`{"title":"watchdog","goal":"synthetic","maintenance":true}`)}})
	if err != nil || !ToolFailed(out) {
		t.Fatalf("initiative granted authority: %s %v", out, err)
	}
}
