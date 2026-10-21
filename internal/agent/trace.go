package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"jarvis/internal/domain"
)

// Inspection must not change whether a side effect succeeds. Failed trace writes
// are operational warnings; the durable tool checkpoints remain authoritative.
func (a *Activities) startTrace(ctx context.Context, tr domain.Trace, input any) func(any, error) {
	tr.Attempt = 1
	if activity.IsActivity(ctx) {
		tr.Attempt = int(activity.GetInfo(ctx).Attempt)
	}
	tr.ID = ID(tr.Kind, tr.TurnID, fmt.Sprint(tr.Step), tr.CallID, fmt.Sprint(tr.Attempt))
	tr.StartedAt = time.Now().UTC()
	tr.Status = "running"
	tr.Input, _ = json.Marshal(input)
	save := func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.Repo.SaveTrace(c, tr); err != nil && a.Logger != nil {
			a.Logger.Warn("Inspection trace could not be saved", "operation", "inspection.trace", "trace_id", tr.ID)
		}
	}
	save()
	return func(output any, err error) {
		now := time.Now().UTC()
		tr.EndedAt = &now
		tr.Status = "completed"
		tr.Output, _ = json.Marshal(output)
		if err != nil {
			tr.Status = "failed"
			tr.Error = err.Error()
		}
		if value, ok := output.(string); ok && tr.Kind == "tool" && ToolFailed(value) {
			tr.Status = "failed"
			tr.Error = "Tool returned an error; see output"
		}
		save()
	}
}

func traceMessages(messages []domain.Message) []domain.Message {
	// Keep exact text and tool calls, but avoid duplicating binary image data in
	// every model snapshot. The original artifact remains available in storage.
	out := append([]domain.Message(nil), messages...)
	for i := range out {
		if len(out[i].Images) > 0 {
			out[i].Images = make([]string, len(messages[i].Images))
			for j := range out[i].Images {
				out[i].Images[j] = "[image supplied to model; binary omitted from inspection snapshot]"
			}
		}
	}
	return out
}

func (a *Activities) analysisModelCall(ctx context.Context, session domain.Session, messages []domain.Message) (out domain.Completion, err error) {
	finish := a.startTrace(ctx, domain.Trace{SessionID: session.ID, TurnID: "analysis/" + session.ID, Trigger: "analysis", Kind: "analysis", Name: a.ModelName}, map[string]any{"messages": traceMessages(messages)})
	defer func() {
		finish(map[string]any{"Message": out.Message, "InputTokens": out.InputTokens, "OutputTokens": out.OutputTokens}, err)
	}()
	return a.Model.Complete(domain.WithModelSession(ctx, ID(session.ConversationID)), messages, nil)
}
