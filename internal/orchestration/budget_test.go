package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"openalbert/internal/agent"
	"openalbert/internal/domain"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func TestBudgetFinalizationUsesEvidenceAndQueuesOneReply(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	model := &scriptedModel{run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n < 2 {
			return calls(call(fmt.Sprint(n), "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		if n != 2 || !strings.Contains(messages[len(messages)-1].Content, "budget is exhausted") {
			t.Fatal("missing bounded finalization instruction")
		}
		found := false
		for _, message := range messages {
			found = found || message.Role == "tool" && strings.Contains(message.Content, "Official release page")
		}
		if !found {
			t.Fatal("collected evidence lost before finalization")
		}
		// Even fabricated extra actions and duplicate replies must not execute.
		return calls(call("forged", "task_create", map[string]string{"title": "Unwanted", "goal": "Keep researching"}), call("answer", "send_message", map[string]string{"text": "Нашёл официальный источник. Остальные данные проверить не успел."}), call("duplicate", "send_message", map[string]string{"text": "Duplicate"})), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 2})
	requireWorkflowOK(t, env)
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Text, "Остальные данные") || model.count() != 3 {
		t.Fatalf("partial result or bound lost: %+v %v", rows, err)
	}
	tasks, err := repo.FindTasks(context.Background(), conv.ID, "")
	if err != nil || len(tasks) != 0 {
		t.Fatal("finalization mutated task state")
	}
	raw, ok, err := repo.LoadCheckpoint(context.Background(), "turn-done/"+rows[0].TurnID)
	var turn agent.Turn
	if err != nil || !ok || json.Unmarshal(raw, &turn) != nil || !turn.Sent || turn.Stats.FinishReason != "reply_queued" || turn.Stats.ModelCalls != 3 || turn.Stats.ToolCalls != 3 {
		t.Fatalf("finalization not recorded: %+v %v", turn, err)
	}
}

type failedReplyHistory struct{ domain.Repository }

func (r failedReplyHistory) AppendMessage(ctx context.Context, session, key string, message domain.Message) error {
	if message.Role == "tool" && message.ToolCallID == "reply" {
		return temporal.NewNonRetryableApplicationError("history unavailable", "test", nil)
	}
	return r.Repository.AppendMessage(ctx, session, key, message)
}

func TestBudgetReplyIsNotFollowedByFailureWhenHistoryWriteFails(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Repo = failedReplyHistory{repo}
	a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		return reply("Available partial result"), nil
	}}
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 1})
	requireWorkflowOK(t, env)
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Text != "Available partial result" {
		t.Fatalf("queued answer followed by contradictory failure: %+v %v", rows, err)
	}
}

func TestBudgetFinalizationFailureStillNotifiesOnce(t *testing.T) {
	for _, failure := range []string{"model", "empty", "rejected send"} {
		t.Run(failure, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
				if n == 0 {
					return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
				}
				switch failure {
				case "model":
					return domain.Message{}, temporal.NewNonRetryableApplicationError("provider unavailable", "test", nil)
				case "rejected send":
					return reply(""), nil
				default:
					return finish(""), nil
				}
			}}
			env := newEnv(a)
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 1})
			requireWorkflowOK(t, env)
			rows, err := repo.PendingOutgoing(context.Background(), 10)
			if err != nil || len(rows) != 1 || rows[0].Trigger != "failure" {
				t.Fatalf("fallback missing or duplicated: %+v %v", rows, err)
			}
		})
	}
}

func TestBudgetFinalizationDoesNotNotifyBackgroundRuns(t *testing.T) {
	for _, initiative := range []bool{false, true} {
		t.Run(fmt.Sprint(initiative), func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			in := agent.RunInput{Key: "quiet-budget", SessionID: "quiet-session", ConversationID: conv.ID, Initiative: initiative}
			if !initiative {
				in.TaskID = "night-audit"
				_, err := repo.CreateTask(context.Background(), domain.Task{ID: in.TaskID, ConversationID: conv.ID, Status: "running", Goal: "Audit quietly"})
				if err != nil {
					t.Fatal(err)
				}
			}
			model := &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) {
				return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
			}}
			a.Model = model
			env := newEnv(a)
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				_, err := runTurn(options(ctx), in, 1)
				return err
			})
			rows, err := repo.PendingOutgoing(context.Background(), 10)
			if err != nil || len(rows) != 0 || model.count() != 1 || env.GetWorkflowError() == nil {
				t.Fatalf("background run forced to reply: %+v %v", rows, err)
			}
		})
	}
}

func TestBudgetFinalizationHonorsStop(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	model := &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
		}
		return reply("Stale answer"), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		_, err := runTurn(options(ctx), agent.RunInput{Key: "stop", SessionID: "stop-session", ConversationID: conv.ID}, 1, func(*agent.Turn) error {
			if model.count() > 1 {
				return temporal.NewCanceledError()
			}
			return nil
		})
		return err
	})
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 0 || model.count() != 2 || !temporal.IsCanceledError(env.GetWorkflowError()) {
		t.Fatalf("stop did not prevent finalization: %+v %v", rows, err)
	}
}

func TestBudgetFinalizationDoesNotChangeRecordedWorkflowCommands(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	model := &scriptedModel{run: func(int, []domain.Message) (domain.Message, error) {
		return calls(call("read", "web_read", map[string]string{"url": "https://example.com"})), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.OnGetVersion("budget-finalization/legacy-budget", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		_, err := runTurn(options(ctx), agent.RunInput{Key: "legacy-budget", SessionID: "legacy-session", ConversationID: conv.ID}, 1)
		return err
	})
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 0 || model.count() != 1 || env.GetWorkflowError() == nil {
		t.Fatalf("legacy turn gained new commands: %+v %v", rows, err)
	}
}
