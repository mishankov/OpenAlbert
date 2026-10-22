package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"jarvis/internal/domain"
	"jarvis/internal/telemetry"
)

// Inspection must not change whether a side effect succeeds. Failed trace writes
// are operational warnings; the durable tool checkpoints remain authoritative.
func (a *Activities) startTrace(ctx context.Context, tr domain.Trace, input any, event *telemetry.Event) func(any, error) {
	tr.Attempt = 1
	if activity.IsActivity(ctx) {
		tr.Attempt = int(activity.GetInfo(ctx).Attempt)
	}
	tr.ID = ID(tr.Kind, tr.TurnID, fmt.Sprint(tr.Step), tr.CallID, fmt.Sprint(tr.Attempt))
	if event != nil {
		event.Set("trace_id", tr.ID)
		base := a.InspectorPublicURL
		if base == "" {
			base = "http://localhost:8080/inspect/"
		}
		if tr.SessionID != "" {
			event.Set("inspector_url", strings.TrimRight(base, "/")+"/#/session/"+url.PathEscape(tr.SessionID)+"?"+url.Values{"trace": {tr.ID}}.Encode())
		}
	}
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
			if temporal.IsCanceledError(err) || ctx.Err() != nil {
				tr.Status = "cancelled"
			}
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
	ctx = telemetry.WithFields(ctx, map[string]any{"session_id": session.ID, "turn_id": "analysis/" + session.ID, "conversation_id": session.ConversationID, "trigger": "analysis"})
	event := a.event(ctx, "llm.complete")
	event.Set("model", a.ModelName)
	finish := a.startTrace(ctx, domain.Trace{SessionID: session.ID, TurnID: "analysis/" + session.ID, Trigger: "analysis", Kind: "analysis", Name: a.ModelName}, map[string]any{"messages": traceMessages(messages)}, event)
	defer func() {
		finish(map[string]any{"Message": out.Message, "InputTokens": out.InputTokens, "OutputTokens": out.OutputTokens}, err)
		event.Set("input_tokens", out.InputTokens)
		event.Set("output_tokens", out.OutputTokens)
		event.End(err)
	}()
	return a.Model.Complete(domain.WithModelSession(ctx, ID(session.ConversationID)), messages, nil)
}
