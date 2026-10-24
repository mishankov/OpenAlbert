package maintenance

import (
	"context"
	"errors"
	"testing"
	"time"

	"openalbert/internal/agent"
	"openalbert/internal/domain"
	"openalbert/internal/orchestration"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func TestIdleQueueHandoffPreservesTaskAndInitiativeDeadline(t *testing.T) {
	for _, initiative := range []bool{false, true} {
		t.Run(map[bool]string{false: "task", true: "initiative"}[initiative], func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{&queueInterceptor{}}})
			start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			env.SetStartTime(start)
			deadline := start.Add(time.Hour)
			env.RegisterActivityWithOptions(func(context.Context, string, string) (domain.Task, error) {
				return domain.Task{ID: "task", Status: "waiting", NextRunAt: deadline}, nil
			}, activity.RegisterOptions{Name: "GetTask"})
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(handoffSignal, handoff{Queue: "new-queue", ResumeAt: deadline}) }, time.Second)
			if initiative {
				env.ExecuteWorkflow(orchestration.InitiativeWorkflow, orchestration.InitiativeInput{ConversationID: "personal:42", Interval: time.Hour, MaxSteps: 12})
			} else {
				env.ExecuteWorkflow(orchestration.TaskWorkflow, orchestration.TaskInput{ConversationID: "personal:42", ID: "task", MaxSteps: 12})
			}
			var continued *workflow.ContinueAsNewError
			if !errors.As(env.GetWorkflowError(), &continued) {
				t.Fatalf("expected continuation, got %v", env.GetWorkflowError())
			}
			if continued.TaskQueueName != "new-queue" {
				t.Fatalf("wrong queue: %s", continued.TaskQueueName)
			}
			if initiative {
				var in orchestration.InitiativeInput
				if err := converter.GetDefaultDataConverter().FromPayloads(continued.Input, &in); err != nil {
					t.Fatal(err)
				}
				if !in.ResumeAt.Equal(deadline) || in.Interval != time.Hour || in.MaxSteps != 12 {
					t.Fatalf("initiative schedule lost: %+v", in)
				}
			} else {
				var in orchestration.TaskInput
				if err := converter.GetDefaultDataConverter().FromPayloads(continued.Input, &in); err != nil {
					t.Fatal(err)
				}
				if in.ID != "task" || in.ConversationID != "personal:42" || in.MaxSteps != 12 {
					t.Fatalf("task identity lost: %+v", in)
				}
			}
		})
	}
}

func TestInitiativeResumesOriginalDeadlineThenNormalInterval(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	env.SetStartTime(start)
	var calls []time.Time
	env.RegisterActivityWithOptions(func(context.Context, agent.RunInput) ([]agent.AudioInput, error) { return nil, nil }, activity.RegisterOptions{Name: "PrepareAudio"})
	env.RegisterActivityWithOptions(func(_ context.Context, input agent.RunInput) (agent.Turn, error) {
		calls = append(calls, env.Now())
		return agent.Turn{Input: input, Done: true}, nil
	}, activity.RegisterOptions{Name: "Prepare"})
	env.RegisterActivityWithOptions(func(context.Context, agent.FinishInput) error { return nil }, activity.RegisterOptions{Name: "Finish"})
	env.RegisterActivityWithOptions(func(context.Context, string) error { return nil }, activity.RegisterOptions{Name: "CloseSession"})
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 40*time.Minute)
	env.ExecuteWorkflow(orchestration.InitiativeWorkflow, orchestration.InitiativeInput{Interval: 30 * time.Minute, ResumeAt: start.Add(5 * time.Minute)})
	if len(calls) != 2 || !calls[0].Equal(start.Add(5*time.Minute)) || !calls[1].Equal(start.Add(35*time.Minute)) {
		t.Fatalf("original deadline and subsequent interval not preserved: %v", calls)
	}
	if !temporal.IsCanceledError(env.GetWorkflowError()) {
		t.Fatalf("test did not stop at cancellation: %v", env.GetWorkflowError())
	}
}
