package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestTaskUpdatesRespectInitiativeAndExecutorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                string
		taskID              string
		initiative, allowed bool
	}{
		{"interactive", "", false, true},
		{"own task", "monitor", false, true},
		{"other task", "another-task", false, false},
		{"initiative", "", true, false},
		{"initiative with forged task ID", "monitor", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			ctx := context.Background()
			prior, err := repo.CreateTask(ctx, domain.Task{ID: "monitor", ConversationID: conv.ID, Title: "Lakers reports", Goal: "Report match results", Status: "waiting", Result: "Last useful match report", ScheduleSeconds: 3600, NextRunAt: time.Now().Add(time.Hour).UTC()})
			if err != nil {
				t.Fatal(err)
			}
			notifications := 0
			a.TaskChanged = func(context.Context, string, string) error { notifications++; return nil }
			in := ToolInput{RunKey: "check", Conversation: conv, TaskID: tc.taskID, Background: tc.initiative, Call: domain.ToolCall{ID: "update", Name: "task_update", Arguments: json.RawMessage(`{"id":"monitor","result":"No news","goal":"Changed goal","status":"paused","schedule_seconds":7200,"next_run_at":"2030-01-01T00:00:00Z"}`)}}
			if !tc.allowed {
				if _, directErr := a.execute(ctx, in); directErr == nil {
					t.Fatal("direct execution bypassed task update boundary")
				}
			}
			out, err := a.ToolCall(ctx, in)
			if err != nil || ToolFailed(out) == tc.allowed {
				t.Fatalf("unexpected authorization: output=%s err=%v", out, err)
			}
			after, found, err := repo.Task(ctx, conv.ID, prior.ID)
			if err != nil || !found {
				t.Fatal(err)
			}
			if !tc.allowed && (!reflect.DeepEqual(prior, after) || notifications != 0) {
				t.Fatalf("denied run changed task or notified scheduler: %+v -> %+v", prior, after)
			}
			if tc.allowed && (after.Result != "No news" || notifications == 0) {
				t.Fatalf("legitimate update failed: %+v", after)
			}
			if tc.initiative && !strings.Contains(out, "initiative_task_update_denied") {
				t.Fatal("missing diagnostic error code")
			}
			for _, tool := range a.toolsForTurn(Turn{Conversation: conv, Input: RunInput{TaskID: tc.taskID, Initiative: tc.initiative}}) {
				if tc.initiative && tool.Name == "task_update" {
					t.Fatal("initiative advertised task_update")
				}
			}
		})
	}
}

func TestInitiativeCannotReplayCachedTaskUpdate(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	if _, err := repo.CreateTask(ctx, domain.Task{ID: "monitor", ConversationID: conv.ID, Status: "running", Goal: "Report matches"}); err != nil {
		t.Fatal(err)
	}
	in := ToolInput{RunKey: "cached-update", Conversation: conv, Call: domain.ToolCall{ID: "save", Name: "task_update", Arguments: json.RawMessage(`{"id":"monitor","result":"Useful result"}`)}}
	if out, err := a.ToolCall(ctx, in); err != nil || ToolFailed(out) {
		t.Fatalf("interactive update failed: %s %v", out, err)
	}
	prior, _, _ := repo.Task(ctx, conv.ID, "monitor")
	in.Background = true
	out, err := a.ToolCall(ctx, in)
	after, _, _ := repo.Task(ctx, conv.ID, "monitor")
	if err != nil || !strings.Contains(out, "initiative_task_update_denied") || !reflect.DeepEqual(prior, after) {
		t.Fatalf("cached success bypassed initiative boundary: %s %v", out, err)
	}
}

func TestInitiativeRetainsTaskDiscoveryAndCreation(t *testing.T) {
	a, _, _, conv := fixture(t)
	names := map[string]bool{}
	for _, tool := range a.toolsForTurn(Turn{Conversation: conv, Input: RunInput{Initiative: true}}) {
		names[tool.Name] = true
	}
	for _, name := range []string{"task_find", "task_read", "task_create"} {
		if !names[name] {
			t.Fatalf("initiative lost %s", name)
		}
	}
}
