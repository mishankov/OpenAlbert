package orchestration

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"

	"go.temporal.io/sdk/workflow"
)

type diagnosticLogs struct{ calls atomic.Int32 }

func (l *diagnosticLogs) Search(_ context.Context, f domain.LogFilter) (domain.LogSearch, error) {
	l.calls.Add(1)
	return domain.LogSearch{ConversationID: f.ConversationID, Summary: []domain.LogCount{{Operation: "turn.complete", Result: "error", ErrorCode: "step_limit", Count: 1}}, Events: []map[string]string{{"operation": "turn.complete", "result": "error", "error_code": "step_limit", "turn_id": "past-turn", "processing_ms": "87000"}}}, nil
}

func TestScheduledOwnerAuditReadsLogsAndSavesResultSilently(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.OwnerUserID = conv.OwnerID
	logs := &diagnosticLogs{}
	a.Logs = logs
	const taskID = "night-audit"
	ctx := context.Background()
	_, err := repo.CreateTask(ctx, domain.Task{ID: taskID, ConversationID: conv.ID, Title: "Night audit", Goal: "Read logs and save the result without a notification", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		switch n {
		case 0:
			return calls(call("logs", "logs_search", map[string]string{"since": "2026-10-22T00:00:00Z", "until": "2026-10-23T00:00:00Z"})), nil
		case 1:
			last := messages[len(messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, "step_limit") || strings.Contains(last.Content, "logs_access_denied") {
				t.Fatal("audit did not receive logs")
			}
			return calls(call("save", "task_update", map[string]string{"id": taskID, "result": "One turn reached step_limit", "status": "completed"})), nil
		default:
			return finish("Audit saved quietly."), nil
		}
	}}
	env := newEnv(a)
	env.ExecuteWorkflow(func(wctx workflow.Context) error {
		turn, err := runTurn(options(wctx), agent.RunInput{Key: "audit-run", SessionID: "audit-session", ConversationID: conv.ID, TaskID: taskID}, 4)
		if err != nil {
			return err
		}
		return workflow.ExecuteActivity(options(wctx), "Finish", finishInput(turn, nil)).Get(wctx, nil)
	})
	requireWorkflowOK(t, env)
	rows, err := repo.PendingOutgoing(ctx, 10)
	task, found, taskErr := repo.Task(ctx, conv.ID, taskID)
	if err != nil || taskErr != nil || len(rows) != 0 || !found || task.Result != "One turn reached step_limit" || logs.calls.Load() != 1 {
		t.Fatalf("audit failed or notified: %+v %+v %v %v", task, rows, err, taskErr)
	}
}

func TestOwnerCanReadDiagnosticsAndReplyWithoutChangingThePastRun(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.OwnerUserID = conv.OwnerID
	logs := &diagnosticLogs{}
	a.Logs = logs
	model := &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("diagnose", "logs_search", map[string]string{"turn_id": "past-turn"})), nil
		}
		if n == 1 {
			found := false
			for _, message := range messages {
				if message.Role == "tool" && message.ToolCallID == "diagnose" {
					var evidence domain.LogSearch
					if err := json.Unmarshal([]byte(message.Content), &evidence); err != nil || len(evidence.Events) != 1 || evidence.Events[0]["error_code"] != "step_limit" {
						t.Errorf("diagnostic evidence not returned: %s %v", message.Content, err)
					}
					found = true
				}
			}
			if !found {
				t.Error("model never received logs")
			}
			return reply("По логам прошлое обращение остановилось из-за лимита шагов. Причину продолжения поиска без подробностей вызовов установить нельзя."), nil
		}
		return finish(`{"summary":"Diagnostic reply sent","memories":[]}`), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 4})
	requireWorkflowOK(t, env)
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Text, "лимита шагов") || logs.calls.Load() != 1 {
		t.Fatalf("diagnosis was not queued exactly once: %+v %v", rows, err)
	}
	tasks, err := repo.FindTasks(context.Background(), conv.ID, "")
	if err != nil || len(tasks) != 0 {
		t.Fatal("diagnosis changed tasks")
	}
	if _, ok, err := repo.LoadCheckpoint(context.Background(), "turn-done/past-turn"); err != nil || ok {
		t.Fatal("diagnosis created a fake past-run receipt")
	}
}
