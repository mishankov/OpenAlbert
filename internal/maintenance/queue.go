// Package maintenance provides explicit, offline deployment operations.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"openalbert/internal/orchestration"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const handoffSignal = orchestration.QueueHandoffSignal

type handoff = orchestration.QueueHandoff

type queueInterceptor struct {
	interceptor.WorkerInterceptorBase
}
type queueInbound struct {
	interceptor.WorkflowInboundInterceptorBase
}

func (*queueInterceptor) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	return &queueInbound{interceptor.WorkflowInboundInterceptorBase{Next: next}}
}

// The original workflow replays unchanged in a coroutine. Only an explicit
// maintenance signal can end it; no application activities are registered by
// the maintenance worker. Histories and workflow IDs remain in the same chain.
func (q *queueInbound) ExecuteWorkflow(ctx workflow.Context, input *interceptor.ExecuteWorkflowInput) (any, error) {
	if workflow.GetInfo(ctx).WorkflowType.Name == "SessionWorkflow" {
		// SessionWorkflow exports its own continuation, including deduplication,
		// pending messages and turn counter; original arguments alone are insufficient.
		return q.Next.ExecuteWorkflow(ctx, input)
	}
	var result any
	var runErr error
	done := false
	workflow.Go(ctx, func(ctx workflow.Context) {
		result, runErr = q.Next.ExecuteWorkflow(ctx, input)
		done = true
	})
	var request handoff
	channel := workflow.GetSignalChannel(ctx, handoffSignal)
	if err := workflow.Await(ctx, func() bool { return done || channel.ReceiveAsync(&request) }); err != nil {
		return nil, err
	}
	if done {
		return result, runErr
	}
	if request.Queue == "" {
		return nil, errors.New("empty queue handoff")
	}
	args := append([]any(nil), input.Args...)
	if value, ok := args[0].(orchestration.InitiativeInput); ok {
		value.ResumeAt = request.ResumeAt
		args[0] = value
	}
	ctx = workflow.WithWorkflowTaskQueue(ctx, request.Queue)
	return nil, workflow.NewContinueAsNewError(ctx, workflow.GetInfo(ctx).WorkflowType.Name, args...)
}

func register(r interface{ RegisterWorkflow(any) }) {
	r.RegisterWorkflow(orchestration.SessionWorkflow)
	r.RegisterWorkflow(orchestration.TaskWorkflow)
	r.RegisterWorkflow(orchestration.AnalysisWorkflow)
	r.RegisterWorkflow(orchestration.InitiativeWorkflow)
}

func replayer(bridge bool) (worker.WorkflowReplayer, error) {
	options := worker.WorkflowReplayerOptions{}
	if bridge {
		options.Interceptors = []interceptor.WorkerInterceptor{&queueInterceptor{}}
	}
	r, err := worker.NewWorkflowReplayerWithOptions(options)
	if err == nil {
		register(r)
	}
	return r, err
}

func ReplayFile(path string) error {
	for _, bridge := range []bool{false, true} {
		r, err := replayer(bridge)
		if err != nil {
			return err
		}
		if err = r.ReplayWorkflowHistoryFromJSONFile(nil, path); err != nil {
			return err
		}
	}
	return nil
}

func timerDeadline(h *historypb.History, session bool) (time.Time, error) {
	type timer struct {
		deadline time.Time
		event    int64
	}
	timers := map[string]timer{}
	for _, e := range h.Events {
		if a := e.GetTimerStartedEventAttributes(); a != nil {
			timers[a.TimerId] = timer{e.EventTime.AsTime().Add(a.StartToFireTimeout.AsDuration()), e.EventId}
		}
		if a := e.GetTimerFiredEventAttributes(); a != nil {
			delete(timers, a.TimerId)
		}
		if a := e.GetTimerCanceledEventAttributes(); a != nil {
			delete(timers, a.TimerId)
		}
	}
	if len(timers) == 0 || (!session && len(timers) != 1) {
		return time.Time{}, fmt.Errorf("expected one idle timer, found %d", len(timers))
	}
	// AwaitWithTimeout can leave earlier timers in history when an incoming
	// message ends the wait. The latest active timer is the current idle wait.
	var latest timer
	for _, value := range timers {
		if value.event > latest.event {
			latest = value
		}
	}
	return latest.deadline, nil
}

// HandoffQueue requires all application workers on source to be stopped.
// It refuses active work, validates replay, and preserves idle timer deadlines.
func HandoffQueue(ctx context.Context, c client.Client, namespace, source, target string) error {
	if source == "" || target == "" || source == target {
		return errors.New("distinct source and target queues are required")
	}
	list, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Namespace: namespace, Query: `ExecutionStatus="Running"`})
	if err != nil {
		return err
	}
	if len(list.NextPageToken) > 0 {
		return errors.New("too many workflows for a single maintenance batch")
	}
	type candidate struct {
		id, run string
		resume  time.Time
	}
	var candidates []candidate
	for _, info := range list.Executions {
		if info.TaskQueue != source {
			continue
		}
		id, run := info.Execution.WorkflowId, info.Execution.RunId
		name := info.Type.Name
		if name != "TaskWorkflow" && name != "InitiativeWorkflow" && name != "SessionWorkflow" {
			return fmt.Errorf("%s is not an idle schedulable workflow", name)
		}
		description, err := c.DescribeWorkflowExecution(ctx, id, run)
		if err != nil {
			return err
		}
		if len(description.PendingActivities) > 0 || len(description.PendingChildren) > 0 {
			return fmt.Errorf("workflow %s has active work", id)
		}
		history := &historypb.History{}
		iterator := c.GetWorkflowHistory(ctx, id, run, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		for iterator.HasNext() {
			event, err := iterator.Next()
			if err != nil {
				return err
			}
			history.Events = append(history.Events, event)
		}
		deadline, err := timerDeadline(history, name == "SessionWorkflow")
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if time.Until(deadline) < 2*time.Minute {
			return fmt.Errorf("%s is due too soon for a safe handoff", id)
		}
		for _, bridge := range []bool{false, true} {
			r, err := replayer(bridge)
			if err != nil {
				return err
			}
			if err = r.ReplayWorkflowHistory(nil, history); err != nil {
				return fmt.Errorf("%s replay: %w", id, err)
			}
		}
		candidates = append(candidates, candidate{id, run, deadline})
	}
	if len(candidates) == 0 {
		return nil
	}
	w := worker.New(c, source, worker.Options{Interceptors: []interceptor.WorkerInterceptor{&queueInterceptor{}}, DisableRegistrationAliasing: false})
	register(w)
	if err = w.Start(); err != nil {
		return err
	}
	defer w.Stop()
	for _, item := range candidates {
		if err = c.SignalWorkflow(ctx, item.id, item.run, handoffSignal, handoff{Queue: target, ResumeAt: item.resume}); err != nil {
			return err
		}
		for {
			d, err := c.DescribeWorkflowExecution(ctx, item.id, "")
			if err != nil {
				return err
			}
			if d.WorkflowExecutionInfo.Execution.RunId != item.run && d.WorkflowExecutionInfo.TaskQueue == target {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		fmt.Printf("Continued %s on %s; original deadline preserved\n", item.id, target)
	}
	return nil
}
