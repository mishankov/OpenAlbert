package orchestration

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"jarvis/internal/agent"
	"jarvis/internal/domain"
	"jarvis/internal/telemetry"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const MessageSignal = "incoming"
const TaskSignal = "changed"

type SessionInput struct {
	ConversationID string
	IdleTimeout    time.Duration
	MaxSteps       int
}
type Event struct {
	ID   string
	Stop bool
}
type TaskInput struct {
	ConversationID string
	ID             string
	MaxSteps       int
}
type InitiativeInput struct {
	ConversationID string
	Interval       time.Duration
	MaxSteps       int
}

func options(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 3 * time.Minute, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 15 * time.Second, MaximumAttempts: 4}})
}

func SessionWorkflow(ctx workflow.Context, in SessionInput) error {
	ctx = options(ctx)
	sessionID := workflow.GetInfo(ctx).WorkflowExecution.RunID
	defer func() {
		cleanup, _ := workflow.NewDisconnectedContext(ctx)
		_ = workflow.ExecuteActivity(cleanup, "CloseSession", sessionID).Get(cleanup, nil)
	}()
	queue := []Event{}
	seen := map[string]bool{}
	channel := workflow.GetSignalChannel(ctx, MessageSignal)
	var activeCancel workflow.CancelFunc
	workflow.Go(ctx, func(ctx workflow.Context) {
		for {
			var ev Event
			if !channel.Receive(ctx, &ev) {
				return
			}
			if seen[ev.ID] {
				continue
			}
			seen[ev.ID] = true
			queue = append(queue, ev)
			if ev.Stop && activeCancel != nil {
				activeCancel()
			}
		}
	})
	turns := 0
	for {
		ready, err := workflow.AwaitWithTimeout(ctx, in.IdleTimeout, func() bool { return len(queue) > 0 })
		if err != nil {
			return err
		}
		if !ready {
			break
		}
		batch := queue
		queue = nil
		ids := []string{}
		stop := false
		lastStop := -1
		for i, ev := range batch {
			ids = append(ids, ev.ID)
			stop = stop || ev.Stop
			if ev.Stop {
				lastStop = i
			}
		}
		if stop {
			queue = append(batch[lastStop+1:], queue...)
			ids = ids[:lastStop+1]
			if err := workflow.ExecuteActivity(ctx, "AcknowledgeStop", in.ConversationID, sessionID, ids).Get(ctx, nil); err != nil {
				return err
			}
			continue
		}
		runctx, cancel := workflow.WithCancel(ctx)
		activeCancel = cancel
		key := agent.ID(sessionID, fmt.Sprint(turns))
		turns++
		turn, runErr := runTurn(runctx, agent.RunInput{Key: key, SessionID: sessionID, ConversationID: in.ConversationID, IncomingIDs: ids}, in.MaxSteps, func(turn *agent.Turn) error {
			if len(queue) == 0 {
				return nil
			}
			newIDs := []string{}
			for _, ev := range queue {
				if ev.Stop {
					return temporal.NewCanceledError()
				}
				newIDs = append(newIDs, ev.ID)
			}
			queue = nil
			steerInput := turn.Input
			steerInput.IncomingIDs = newIDs
			// Retain consumed inputs even if preparation is cancelled, so the
			// dispatcher cannot resurrect them after Finish.
			allIDs := append(append([]string{}, turn.Input.IncomingIDs...), newIDs...)
			turn.Input.IncomingIDs = allIDs
			updated, err := prepareTurn(runctx, steerInput)
			if err != nil {
				return err
			}
			updated.Input.IncomingIDs = allIDs
			updated.Stats = turn.Stats
			if !turn.ReceivedAt.IsZero() && (updated.ReceivedAt.IsZero() || turn.ReceivedAt.Before(updated.ReceivedAt)) {
				updated.ReceivedAt = turn.ReceivedAt
			}
			*turn = updated
			return nil
		})
		activeCancel = nil
		cancel()
		// Persist final state outside the cancelled context so stop cannot resurrect inbox items.
		if err := workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, runErr)).Get(ctx, nil); err != nil {
			return err
		}
		if runErr != nil && !temporal.IsCanceledError(runErr) {
			if !turn.Sent || workflow.GetVersion(ctx, "failure-after-reply/"+key, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
				if err := workflow.ExecuteActivity(ctx, "NotifyFailure", in.ConversationID, key).Get(ctx, nil); err != nil {
					return err
				}
			}
		}
		if turns >= 30 || workflow.GetInfo(ctx).GetContinueAsNewSuggested() || workflow.GetInfo(ctx).GetCurrentHistoryLength() > 12000 { // Bounded histories; buffered events remain durable in the inbox and will be re-signalled.
			break
		}
	}
	return workflow.ExecuteActivity(ctx, "CloseSession", sessionID).Get(ctx, nil)
}

// Version per turn preserves recorded activity sequences while later turns of
// existing sessions can adopt independent speech recognition.
func prepareTurn(ctx workflow.Context, in agent.RunInput) (turn agent.Turn, err error) {
	turn.Input = in
	if workflow.GetVersion(ctx, "audio-preparation/"+in.Key, workflow.DefaultVersion, 1) != workflow.DefaultVersion && len(in.IncomingIDs) > 0 {
		var jobs []agent.AudioInput
		prepOptions := workflow.GetActivityOptions(ctx)
		prepOptions.HeartbeatTimeout = 30 * time.Second
		prepCtx := workflow.WithActivityOptions(ctx, prepOptions)
		if err = workflow.ExecuteActivity(prepCtx, "PrepareAudio", in).Get(prepCtx, &jobs); err != nil {
			return
		}
		stt := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{TaskQueue: workflow.GetInfo(ctx).TaskQueueName + ".stt", StartToCloseTimeout: 12 * time.Minute, ScheduleToCloseTimeout: 30 * time.Minute, HeartbeatTimeout: 30 * time.Second, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{InitialInterval: 15 * time.Second, MaximumInterval: time.Minute, MaximumAttempts: 5}})
		for i, job := range jobs {
			var result domain.AudioResult
			e := workflow.ExecuteActivity(stt, "TranscribeAudio", job).Get(stt, &result)
			if e == nil {
				continue
			}
			code := "stt_failed"
			var app *temporal.ApplicationError
			if errors.As(e, &app) {
				code = app.Type()
			}
			cleanup := ctx
			remaining := jobs[i : i+1]
			if temporal.IsCanceledError(e) {
				code = "cancelled"
				cleanup, _ = workflow.NewDisconnectedContext(ctx)
				remaining = jobs[i:]
			}
			for _, pending := range remaining {
				if saveErr := workflow.ExecuteActivity(cleanup, "RecordAudioFailure", agent.AudioFailure{Input: pending, Code: code}).Get(cleanup, nil); saveErr != nil {
					return turn, saveErr
				}
			}
			if temporal.IsCanceledError(e) {
				return turn, e
			}
		}
	}
	err = workflow.ExecuteActivity(ctx, "Prepare", in).Get(ctx, &turn)
	return
}

func runTurn(ctx workflow.Context, in agent.RunInput, maxSteps int, steer ...func(*agent.Turn) error) (out agent.Turn, err error) {
	if workflow.GetVersion(ctx, "turn-observability-v1", workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		in.StartedAt = workflow.Now(ctx)
	}
	defer func() {
		out.Messages = agent.CompactContext(out.Messages)
		if out.Stats.FinishReason == "" {
			out.Stats.FinishReason = "no_reply"
			if out.Sent {
				out.Stats.FinishReason = "reply_queued"
			}
			if err != nil {
				out.Stats.FinishReason = "failed"
			}
			if temporal.IsCanceledError(err) {
				out.Stats.FinishReason = "cancelled"
			}
		}
	}()
	turn, prepareErr := prepareTurn(ctx, in)
	if prepareErr != nil {
		return turn, prepareErr
	}
	if turn.Done {
		return turn, nil
	}
steps:
	for step := 0; step < maxSteps; step++ {
		if len(steer) > 0 {
			if err := steer[0](&turn); err != nil {
				return turn, err
			}
		}
		if in.TaskID != "" {
			var current domain.Task
			if err := workflow.ExecuteActivity(ctx, "GetTask", in.ConversationID, in.TaskID).Get(ctx, &current); err != nil {
				return turn, err
			}
			if current.Status == "cancelled" || current.Status == "paused" {
				turn.Stats.FinishReason = current.Status
				return turn, nil
			}
		}
		turn.Messages = agent.CompactContext(turn.Messages)
		var result agent.ModelOutput
		turn.Stats.ModelCalls++
		if err := workflow.ExecuteActivity(ctx, "ModelCall", agent.ModelInput{Turn: turn, Step: step}).Get(ctx, &result); err != nil {
			return turn, err
		}
		turn.Stats.InputTokens += result.InputTokens
		turn.Stats.OutputTokens += result.OutputTokens
		if len(steer) > 0 {
			before := len(turn.Input.IncomingIDs)
			if err := steer[0](&turn); err != nil {
				return turn, err
			}
			if len(turn.Input.IncomingIDs) > before {
				continue
			}
		}
		message := result.Message
		turn.Messages = append(turn.Messages, message)
		if err := workflow.ExecuteActivity(ctx, "PersistMessage", in.SessionID, fmt.Sprintf("%s/assistant/%d", in.Key, step), message).Get(ctx, nil); err != nil {
			return turn, err
		}
		if len(message.ToolCalls) == 0 {
			turn.LastText = message.Content
			// Version per turn: replay already-recorded deliveries, while even the next
			// turn of an existing session/initiative uses explicit delivery only.
			if workflow.GetVersion(ctx, "explicit-message-delivery/"+in.Key, workflow.DefaultVersion, 1) == workflow.DefaultVersion && message.Content != "" && !turn.Sent {
				args, _ := json.Marshal(map[string]string{"text": message.Content})
				call := domain.ToolCall{ID: "final-reply", Name: "send_message", Arguments: args}
				var output string
				recordTool(&turn, call.Name)
				if err := workflow.ExecuteActivity(ctx, "ToolCall", toolInput(turn, step, call)).Get(ctx, &output); err != nil {
					turn.Stats.ToolErrors++
					return turn, err
				}
				turn.Sent = !agent.ToolFailed(output)
				if !turn.Sent {
					turn.Stats.ToolErrors++
				}
			}
			return turn, nil
		}
		views := []domain.Message{}
		for _, call := range message.ToolCalls {
			if len(steer) > 0 {
				before := len(turn.Input.IncomingIDs)
				if err := steer[0](&turn); err != nil {
					return turn, err
				}
				if len(turn.Input.IncomingIDs) > before {
					continue steps
				}
			}
			if in.TaskID != "" {
				var current domain.Task
				if err := workflow.ExecuteActivity(ctx, "GetTask", in.ConversationID, in.TaskID).Get(ctx, &current); err != nil {
					return turn, err
				}
				if current.Status == "cancelled" || current.Status == "paused" {
					turn.Stats.FinishReason = current.Status
					return turn, nil
				}
			}
			var output string
			toolCtx := ctx
			activityName := "ToolCall"
			if strings.HasPrefix(call.Name, "browser_") && workflow.GetVersion(ctx, "browser-activities-v1", workflow.DefaultVersion, 1) != workflow.DefaultVersion {
				queue := workflow.GetInfo(ctx).TaskQueueName + ".browser-ui"
				if in.TaskID != "" || in.Initiative {
					queue = workflow.GetInfo(ctx).TaskQueueName + ".browser-bg"
				}
				toolCtx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{TaskQueue: queue, StartToCloseTimeout: 2 * time.Minute, ScheduleToCloseTimeout: 24 * time.Hour, HeartbeatTimeout: 10 * time.Second, WaitForCancellation: true, RetryPolicy: &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: 5 * time.Second, MaximumAttempts: 0}})
				activityName = "BrowserToolCall"
			}
			recordTool(&turn, call.Name)
			if err := workflow.ExecuteActivity(toolCtx, activityName, toolInput(turn, step, call)).Get(ctx, &output); err != nil {
				turn.Stats.ToolErrors++
				return turn, err
			}
			if agent.ToolFailed(output) {
				turn.Stats.ToolErrors++
			}
			toolMessage := domain.Message{Role: "tool", Content: output, ToolCallID: call.ID}
			turn.Messages = append(turn.Messages, toolMessage)
			if err := workflow.ExecuteActivity(ctx, "PersistMessage", in.SessionID, fmt.Sprintf("%s/tool/%d/%s", in.Key, step, call.ID), toolMessage).Get(ctx, nil); err != nil {
				return turn, err
			}
			if call.Name == "artifact_read" && !strings.Contains(output, `"error"`) {
				var args struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(call.Arguments, &args)
				var view domain.Message
				if err := workflow.ExecuteActivity(ctx, "ArtifactView", turn.Conversation.ID, args.ID).Get(ctx, &view); err != nil {
					return turn, err
				}
				if len(view.Images) > 0 {
					views = append(views, view)
				}
			}
			if call.Name == "send_message" && !strings.Contains(output, `"error"`) {
				turn.Sent = true
			}
		}
		for i, view := range views {
			turn.Messages = append(turn.Messages, view)
			if err := workflow.ExecuteActivity(ctx, "PersistMessage", in.SessionID, fmt.Sprintf("%s/image/%d/%d", in.Key, step, i), view).Get(ctx, nil); err != nil {
				return turn, err
			}
		}
		if turn.Sent && workflow.GetVersion(ctx, "finish-turn-after-reply", workflow.DefaultVersion, 1) != workflow.DefaultVersion {
			return turn, nil
		}
	}
	if in.TaskID == "" && !in.Initiative && !turn.Sent && workflow.GetVersion(ctx, "budget-finalization/"+in.Key, workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		return finalizeBudget(ctx, turn, maxSteps, steer...)
	}
	return turn, temporal.NewNonRetryableApplicationError("agent step limit reached", "step_limit", nil)
}

// One reserved synthesis call can use existing evidence but cannot continue
// research or mutate task state. The caller retains NotifyFailure as a fallback.
func finalizeBudget(ctx workflow.Context, turn agent.Turn, step int, steer ...func(*agent.Turn) error) (agent.Turn, error) {
	if len(steer) > 0 {
		if err := steer[0](&turn); err != nil {
			return turn, err
		}
	}
	turn.Messages = append(turn.Messages, domain.Message{Role: "user", Content: "The research step budget is exhausted. This is the single reserved finalization step. Stop researching and use send_message now to answer the latest user request from the evidence already collected. Give a useful partial result when possible, clearly mark gaps and uncertainty, and never invent missing facts or claim unfinished work succeeded. If the evidence is insufficient, say what prevented completion. Only send_message is available; do not request other tools."})
	turn.Messages = agent.CompactContext(turn.Messages)
	var result agent.ModelOutput
	turn.Stats.ModelCalls++
	if err := workflow.ExecuteActivity(ctx, "ModelCall", agent.ModelInput{Turn: turn, Step: step, Finalize: true}).Get(ctx, &result); err != nil {
		return turn, err
	}
	turn.Stats.InputTokens += result.InputTokens
	turn.Stats.OutputTokens += result.OutputTokens
	if len(steer) > 0 {
		before := len(turn.Input.IncomingIDs)
		if err := steer[0](&turn); err != nil {
			return turn, err
		}
		if len(turn.Input.IncomingIDs) > before {
			return turn, temporal.NewNonRetryableApplicationError("agent step limit reached after new input", "step_limit", nil)
		}
	}
	message := result.Message
	turn.Messages = append(turn.Messages, message)
	if err := workflow.ExecuteActivity(ctx, "PersistMessage", turn.Input.SessionID, fmt.Sprintf("%s/assistant/%d", turn.Input.Key, step), message).Get(ctx, nil); err != nil {
		return turn, err
	}
	for _, call := range message.ToolCalls {
		// Validate even a forged model response; advertised tools are not a guard.
		var output string
		if call.Name != "send_message" || turn.Sent {
			output = `{"error":"Finalization only permits one successful send_message; no further actions are executed."}`
		} else {
			recordTool(&turn, call.Name)
			if err := workflow.ExecuteActivity(ctx, "ToolCall", toolInput(turn, step, call)).Get(ctx, &output); err != nil {
				turn.Stats.ToolErrors++
				return turn, err
			}
			if agent.ToolFailed(output) {
				turn.Stats.ToolErrors++
			} else {
				turn.Sent = true
			}
		}
		toolMessage := domain.Message{Role: "tool", Content: output, ToolCallID: call.ID}
		turn.Messages = append(turn.Messages, toolMessage)
		if err := workflow.ExecuteActivity(ctx, "PersistMessage", turn.Input.SessionID, fmt.Sprintf("%s/tool/%d/%s", turn.Input.Key, step, call.ID), toolMessage).Get(ctx, nil); err != nil {
			return turn, err
		}
	}
	if turn.Sent {
		return turn, nil
	}
	return turn, temporal.NewNonRetryableApplicationError("agent finalization did not queue a reply", "step_limit", nil)
}

func TaskWorkflow(ctx workflow.Context, in TaskInput) error {
	ctx = options(ctx)
	changed := workflow.GetSignalChannel(ctx, TaskSignal)
	for check := 0; check < 30; check++ {
		var t domain.Task
		if err := workflow.ExecuteActivity(ctx, "GetTask", in.ConversationID, in.ID).Get(ctx, &t); err != nil {
			return err
		}
		if t.Status == "completed" || t.Status == "cancelled" || t.Status == "failed" || t.Status == "paused" {
			return nil
		}
		wait := t.NextRunAt.Sub(workflow.Now(ctx))
		if wait > 0 {
			timerCtx, cancel := workflow.WithCancel(ctx)
			timer := workflow.NewTimer(timerCtx, wait)
			wake := false
			selector := workflow.NewSelector(ctx)
			selector.AddFuture(timer, func(workflow.Future) { wake = true })
			selector.AddReceive(changed, func(c workflow.ReceiveChannel, _ bool) { var ignored string; c.Receive(ctx, &ignored) })
			selector.Select(ctx)
			cancel()
			if !wake {
				continue
			}
		}
		if err := workflow.ExecuteActivity(ctx, "BeginTask", in.ConversationID, in.ID, workflow.Now(ctx)).Get(ctx, &t); err != nil {
			return err
		}
		if t.Status != "running" {
			continue
		}
		session := agent.ID(in.ID, workflow.GetInfo(ctx).WorkflowExecution.RunID, fmt.Sprint(check))
		run := agent.RunInput{Key: session, SessionID: session, ConversationID: in.ConversationID, TaskID: in.ID}
		turn, runErr := runTurn(ctx, run, in.MaxSteps)
		if err := workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, runErr)).Get(ctx, nil); err != nil {
			return err
		}
		if err := workflow.ExecuteActivity(ctx, "CloseSession", session).Get(ctx, nil); err != nil {
			return err
		}
		if err := workflow.ExecuteActivity(ctx, "AdvanceTask", in.ConversationID, in.ID, runErr != nil, workflow.Now(ctx)).Get(ctx, &t); err != nil {
			return err
		}
	}
	return workflow.NewContinueAsNewError(ctx, TaskWorkflow, in)
}
func AnalysisWorkflow(ctx workflow.Context, session domain.Session) error {
	ctx = options(ctx)
	return workflow.ExecuteActivity(ctx, "Analyze", session).Get(ctx, nil)
}
func InitiativeWorkflow(ctx workflow.Context, in InitiativeInput) error {
	ctx = options(ctx)
	if in.Interval <= 0 {
		return nil
	}
	for i := 0; i < 20; i++ {
		if err := workflow.Sleep(ctx, in.Interval); err != nil {
			return err
		}
		session := agent.ID("initiative", in.ConversationID, workflow.GetInfo(ctx).WorkflowExecution.RunID, fmt.Sprint(i))
		turn, runErr := runTurn(ctx, agent.RunInput{Key: session, SessionID: session, ConversationID: in.ConversationID, Initiative: true}, in.MaxSteps)
		if err := workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, runErr)).Get(ctx, nil); err != nil {
			return err
		}
		if err := workflow.ExecuteActivity(ctx, "CloseSession", session).Get(ctx, nil); err != nil {
			return err
		}
	}
	return workflow.NewContinueAsNewError(ctx, InitiativeWorkflow, in)
}

func toolInput(t agent.Turn, step int, call domain.ToolCall) agent.ToolInput {
	return agent.ToolInput{RunKey: t.Input.Key, Step: step, Call: call, Conversation: t.Conversation, SessionID: t.Input.SessionID, TaskID: t.Input.TaskID, Background: t.Input.Initiative, StartedAt: t.Input.StartedAt, ReceivedAt: t.ReceivedAt}
}
func recordTool(t *agent.Turn, name string) {
	t.Stats.ToolCalls++
	for _, old := range t.Stats.Tools {
		if old == name {
			return
		}
	}
	t.Stats.Tools = append(t.Stats.Tools, name)
}
func finishInput(t agent.Turn, err error) agent.FinishInput {
	code := ""
	if err != nil {
		code = telemetry.Classify(err).Code
	}
	if temporal.IsCanceledError(err) {
		code = "cancelled"
	}
	return agent.FinishInput{Turn: t, Failed: err != nil, FailureCode: code}
}
