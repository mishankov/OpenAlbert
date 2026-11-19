package orchestration

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/codemode"
	"openalbert/apps/backend/internal/domain"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	codemodeCallsPerTurn  = 64
	codemodeParallelism   = 4
	codemodeActivityBytes = 1856 << 10 // Leave headroom below Temporal's 2 MiB payload limit.
)

var (
	errCodemodeSteered     = errors.New("codemode interrupted by a new message")
	errCodemodeTaskStopped = errors.New("codemode interrupted by task state")
)

type codemodeAction struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	ExecutionID string `json:"execution_id,omitempty"`
}

type codemodeOutput struct {
	Output          json.RawMessage  `json:"output,omitempty"`
	OutputTruncated bool             `json:"output_truncated,omitempty"`
	Error           string           `json:"error,omitempty"`
	Actions         []codemodeAction `json:"actions,omitempty"`
}

func runCodemodeCalls(ctx workflow.Context, turn *agent.Turn, step int, calls []domain.ToolCall, finalize bool, steer ...turnSteerer) error {
	beforeAction := func(saveInterrupted func() error) error {
		if len(steer) > 0 {
			before := len(turn.Input.IncomingIDs)
			if err := steer[0](turn, saveInterrupted); err != nil {
				return err
			}
			if len(turn.Input.IncomingIDs) > before {
				return errCodemodeSteered
			}
		}
		if turn.Input.TaskID != "" {
			var task domain.Task
			if err := workflow.ExecuteActivity(ctx, "GetTask", turn.Input.ConversationID, turn.Input.TaskID).Get(ctx, &task); err != nil {
				return err
			}
			if task.Status == "paused" || task.Status == "cancelled" {
				turn.Stats.FinishReason = task.Status
				return errCodemodeTaskStopped
			}
		}
		return ctx.Err()
	}
	var views []domain.Message
	ran := false
	for i, call := range calls {
		out := codemodeOutput{Error: "Use codemode to call tools for this turn."}
		if call.Name == agent.CodemodeName {
			if ran {
				out.Error = "Only one Codemode script can run in a model step."
			} else {
				ran = true
				saveInterrupted := func(ctx workflow.Context, partial codemodeOutput) error {
					partial.Error = "Codemode was interrupted. Check the recorded actions before continuing."
					if err := persistCodemodeOutput(ctx, turn, step, call, partial); err != nil {
						return err
					}
					for _, skipped := range calls[i+1:] {
						if err := persistCodemodeOutput(ctx, turn, step, skipped, codemodeOutput{Error: "Tool call skipped after interruption."}); err != nil {
							return err
						}
					}
					return nil
				}
				var err error
				out, views, err = runCodemodeScript(ctx, turn, step, call, finalize, func(partial codemodeOutput) error {
					return beforeAction(func() error { return saveInterrupted(ctx, partial) })
				})
				if err != nil {
					if len(out.Actions) > 0 && (temporal.IsCanceledError(err) || errors.Is(err, errCodemodeTaskStopped)) {
						cleanup, cancel := workflow.NewDisconnectedContext(ctx)
						saveErr := saveInterrupted(cleanup, out)
						cancel()
						if saveErr != nil {
							return saveErr
						}
					}
					return err
				}
			}
		}
		if out.Error != "" {
			turn.Stats.ToolErrors++
		}
		if err := persistCodemodeOutput(ctx, turn, step, call, out); err != nil {
			return err
		}
	}
	for i, view := range views {
		key := fmt.Sprintf("%s/image/%d/%d", turn.Input.Key, step, i)
		if err := workflow.ExecuteActivity(ctx, "PersistMessage", turn.Input.SessionID, key, view).Get(ctx, nil); err != nil {
			return err
		}
		turn.Messages = append(turn.Messages, view)
	}
	return nil
}

func persistCodemodeOutput(ctx workflow.Context, turn *agent.Turn, step int, call domain.ToolCall, out codemodeOutput) error {
	raw, err := encodeCodemodeOutput(out)
	if err != nil {
		return err
	}
	message := domain.Message{Role: "tool", Content: string(raw), ToolCallID: call.ID}
	key := fmt.Sprintf("%s/tool/%d/%s", turn.Input.Key, step, call.ID)
	if err := workflow.ExecuteActivity(ctx, "PersistMessage", turn.Input.SessionID, key, message).Get(ctx, nil); err != nil {
		return err
	}
	turn.Messages = append(turn.Messages, message)
	return nil
}

func encodeCodemodeOutput(out codemodeOutput) ([]byte, error) {
	raw, err := json.Marshal(out)
	if err != nil || len(raw) <= agent.MaxContextMessageRunes {
		return raw, err
	}
	// Keep the host's action receipts intact and return a complete JSON
	// envelope. The raw script output remains an all-or-nothing JSON value.
	out.Output, out.OutputTruncated = nil, true
	return json.Marshal(out)
}

func runCodemodeScript(ctx workflow.Context, turn *agent.Turn, step int, call domain.ToolCall, finalize bool, beforeAction func(codemodeOutput) error) (out codemodeOutput, views []domain.Message, err error) {
	in := agent.CodemodeInput{
		ToolInput: toolInput(*turn, step, call),
		Finalize:  finalize,
		Started:   workflow.Now(ctx),
	}
	callsLeft := codemodeCallsPerTurn - turn.Stats.ToolCalls
	if finalize {
		callsLeft = 12 // Reserve an independent allowance for remaining reply parts.
	}
	issued := 0
	for {
		if err := beforeAction(out); err != nil {
			return out, views, err
		}
		payload, err := converter.GetDefaultDataConverter().ToPayload(in)
		if err != nil {
			return out, views, err
		}
		if len(payload.Data) > codemodeActivityBytes {
			out.Error = "Codemode tool receipts exceeded the batch size limit. Use smaller batches."
			return out, views, nil
		}
		var result codemode.Result
		if err := workflow.ExecuteActivity(ctx, "CodemodeStep", in).Get(ctx, &result); err != nil {
			return out, views, err
		}
		if len(result.Pending) == 0 {
			out.Output, out.Error = result.Output, result.Error
			if out.Error == "" && result.State != nil {
				state := agent.CodemodeState{SessionID: turn.Input.SessionID, Values: result.State}
				if err := workflow.ExecuteActivity(ctx, "SaveCodemodeState", state).Get(ctx, nil); err != nil {
					return out, views, err
				}
			}
			return out, views, nil
		}
		if issued+len(result.Pending) > callsLeft {
			out.Error = "Codemode tool call budget exhausted. Use the evidence already collected."
			return out, views, nil
		}
		batch := make([]codemode.Receipt, len(result.Pending))
		for i := 0; i < len(batch); {
			if err := beforeAction(out); err != nil {
				return out, views, err
			}
			end := i + 1
			if agent.CodemodeParallelRead(result.Pending[i].Name) {
				for end < len(batch) && end-i < codemodeParallelism && agent.CodemodeParallelRead(result.Pending[end].Name) {
					end++
				}
			}
			receipts, actions, images, err := runCodemodeChildren(ctx, turn, in, issued+i, result.Pending[i:end], func() error {
				return beforeAction(out)
			})
			copy(batch[i:end], receipts)
			out.Actions = append(out.Actions, actions...)
			views = append(views, images...)
			if err != nil {
				return out, views, err
			}
			i = end
		}
		issued += len(batch)
		in.Stage++
		in.LastBatch = batch
	}
}

func runCodemodeChildren(ctx workflow.Context, turn *agent.Turn, parent agent.CodemodeInput, first int, requests []codemode.Request, beforeDispatch func() error) ([]codemode.Receipt, []codemodeAction, []domain.Message, error) {
	type child struct {
		call   domain.ToolCall
		prefix string
		future workflow.Future
	}
	children := make([]child, len(requests))
	for i, request := range requests {
		id := agent.ID(parent.Call.ID, fmt.Sprint(first+i), "codemode")
		call := domain.ToolCall{ID: id, Name: request.Name, Arguments: request.Arguments}
		prefix := fmt.Sprintf("%s/codemode/%s/%s", turn.Input.Key, agent.ID(parent.Call.ID), id)
		message := domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{call}, ParentToolCallID: parent.Call.ID}
		key := fmt.Sprintf("%s/assistant/%d", prefix, parent.Step)
		if err := workflow.ExecuteActivity(ctx, "PersistMessage", turn.Input.SessionID, key, message).Get(ctx, nil); err != nil {
			return nil, nil, nil, err
		}
		children[i] = child{call: call, prefix: prefix}
	}
	if err := beforeDispatch(); err != nil {
		return nil, nil, nil, err
	}
	for i := range children {
		c := &children[i]
		recordTool(turn, c.call.Name)
		in := toolInput(*turn, parent.Step, c.call)
		in.ParentToolCallID = parent.Call.ID
		c.future = executeToolActivity(ctx, in)
	}
	receipts := make([]codemode.Receipt, 0, len(children))
	var actions []codemodeAction
	var views []domain.Message
	for i, c := range children {
		var output string
		if err := c.future.Get(ctx, &output); err != nil {
			if temporal.IsCanceledError(err) || ctx.Err() != nil {
				return receipts, actions, views, err
			}
			output = codemodeMissingResult(toolInput(*turn, parent.Step, c.call))
		}
		failed := agent.ToolFailed(output)
		if failed {
			turn.Stats.ToolErrors++
		}
		if !agent.CodemodeParallelRead(c.call.Name) {
			actions = append(actions, codemodeActionReceipt(c.call, output, failed))
		}
		if !failed && (domain.IsReplyTool(c.call.Name) || c.call.Name == "send_widget") {
			turn.Sent = true
		}
		key := fmt.Sprintf("%s/tool/%d/%s", c.prefix, parent.Step, c.call.ID)
		message := domain.Message{Role: "tool", Content: output, ToolCallID: c.call.ID, ParentToolCallID: parent.Call.ID}
		if err := workflow.ExecuteActivity(ctx, "PersistMessage", turn.Input.SessionID, key, message).Get(ctx, nil); err != nil {
			return receipts, actions, views, err
		}
		if !failed && c.call.Name == "artifact_read" {
			var args struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(c.call.Arguments, &args)
			var view domain.Message
			if err := workflow.ExecuteActivity(ctx, "ArtifactView", turn.Conversation.ID, args.ID).Get(ctx, &view); err != nil {
				return receipts, actions, views, err
			}
			if len(view.Images) > 0 {
				views = append(views, view)
			}
		}
		receipts = append(receipts, codemode.Receipt{Request: requests[i], Result: json.RawMessage(output), Failed: failed})
	}
	return receipts, actions, views, nil
}

func codemodeMissingResult(in agent.ToolInput) string {
	if in.Call.Name == "computer_exec" {
		return fmt.Sprintf(`{"error":"Computer command outcome is unknown. Inspect this execution before attempting it again.","error_code":"tool_outcome_unknown","state":"unknown","execution_id":%q}`,
			agent.ComputerExecutionID(in))
	}
	if agent.CodemodeParallelRead(in.Call.Name) {
		return `{"error":"Read activity did not return a result.","error_code":"tool_activity_failed"}`
	}
	return agent.CodemodeUnknownOutcome
}

func codemodeActionReceipt(call domain.ToolCall, output string, failed bool) codemodeAction {
	var result struct {
		ErrorCode   string `json:"error_code"`
		State       string `json:"state"`
		ExecutionID string `json:"execution_id"`
	}
	_ = json.Unmarshal([]byte(output), &result)
	action := codemodeAction{ID: call.ID, Name: call.Name, Status: "returned"}
	switch {
	case result.State == "unknown" || result.ErrorCode == "tool_outcome_unknown":
		action.Status = "unknown"
	case failed:
		action.Status = "failed"
	case domain.IsReplyTool(call.Name) || call.Name == "send_group_message":
		action.Status = "queued"
	}
	if strings.HasPrefix(call.Name, "computer_") && len(result.ExecutionID) == 32 {
		action.ExecutionID = result.ExecutionID
		if !failed {
			switch result.State {
			case "queued", "running", "completed", "failed", "cancelled", "timed_out", "unknown":
				action.Status = result.State
			}
		}
	}
	return action
}
