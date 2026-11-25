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
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

type toolTraceOutput struct {
	output string
	search *domain.SearchTrace
}

// Inspection must not change whether a side effect succeeds. Failed trace writes
// are operational warnings; the durable tool checkpoints remain authoritative.
func (a *Activities) startTrace(ctx context.Context, tr domain.Trace, input any, event *telemetry.Event) func(output any, err error, name string) {
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
	if tr.Kind == "tool" && tr.Name == "web_search" {
		tr.Input, _ = json.Marshal(domain.SanitizeWebSearchTraceInput(input, a.searchProvider()))
	} else {
		tr.Input, _ = json.Marshal(input)
	}
	save := func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := a.Repo.SaveTrace(c, tr); err != nil && a.Logger != nil {
			a.Logger.Warn("Inspection trace could not be saved", "operation", "inspection.trace", "trace_id", tr.ID)
		}
	}
	save()
	return func(output any, err error, name string) {
		if wrapped, ok := output.(toolTraceOutput); ok {
			tr.Search = wrapped.search
			output = wrapped.output
		}
		if name != "" {
			tr.Name = name
		}
		var usage domain.ModelUsage
		switch value := output.(type) {
		case ModelOutput:
			usage = value.Usage
		case domain.Completion:
			usage = value.Usage
		}
		if usage.Model != "" {
			tr.Name = usage.Model
		}
		if usage.Role != "" {
			tr.ModelRole = usage.Role
		}
		tr.Profile = usage.Profile
		now := time.Now().UTC()
		tr.EndedAt = &now
		tr.Status = "completed"
		toolFailed := false
		if value, ok := output.(string); ok && tr.Kind == "tool" && ToolFailed(value) {
			toolFailed = true
		}
		if tr.Kind == "tool" && tr.Name == "web_search" {
			tr.Output, _ = json.Marshal(sanitizeWebSearchTraceOutput(output, tr.Search, err != nil || toolFailed))
		} else {
			tr.Output, _ = json.Marshal(sanitizeTraceOutput(output))
		}
		if err != nil {
			tr.Status = "failed"
			if temporal.IsCanceledError(err) || ctx.Err() != nil {
				tr.Status = "cancelled"
			}
			tr.Error = err.Error()
		}
		if toolFailed {
			tr.Status = "failed"
			tr.Error = "Tool returned an error; see output"
		}
		save()
	}
}

func traceMessages(messages []domain.Message) []domain.Message {
	return domain.SanitizeTraceMessages(messages)
}

// SanitizeTraceMessages prepares a safe inspection copy of model messages.
func SanitizeTraceMessages(messages []domain.Message) []domain.Message {
	return domain.SanitizeTraceMessages(messages)
}

// SanitizeTraceCompletion prepares a safe inspection copy of a model completion.
func SanitizeTraceCompletion(comp domain.Completion) domain.Completion {
	return domain.SanitizeTraceCompletion(comp)
}

func sanitizeTraceOutput(output any) any {
	switch v := output.(type) {
	case ModelOutput:
		if len(v.Message.ToolCalls) == 0 && len(v.Message.ResponsesItems) == 0 {
			return v
		}
		out := v
		out.Message = domain.SanitizeTraceMessageToolCalls(v.Message)
		return out
	case domain.Completion:
		return domain.SanitizeTraceCompletion(v)
	default:
		return output
	}
}

func sanitizeAnalysisTraceMessages(messages []domain.Message, extraRedactedIDs map[string]bool) []domain.Message {
	out := domain.SanitizeTraceMessagesWithRedactedIDs(messages, extraRedactedIDs)
	for i, msg := range out {
		if msg.Role != "user" || strings.TrimSpace(msg.Content) == "" {
			continue
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal([]byte(msg.Content), &payload) != nil {
			continue
		}
		rawTranscript, ok := payload["transcript"]
		if !ok {
			continue
		}
		var transcript []domain.Message
		if json.Unmarshal(rawTranscript, &transcript) != nil {
			continue
		}
		encodedTranscript, err := json.Marshal(domain.SanitizeTraceMessagesWithRedactedIDs(transcript, extraRedactedIDs))
		if err != nil {
			continue
		}
		payload["transcript"] = encodedTranscript
		if encodedPayload, err := json.Marshal(payload); err == nil {
			out[i].Content = string(encodedPayload)
		}
	}
	return out
}

func (a *Activities) analysisModelCall(ctx context.Context, session domain.Session, messages []domain.Message, extraRedactedIDs ...map[string]bool) (out domain.Completion, err error) {
	var redactedIDs map[string]bool
	if len(extraRedactedIDs) > 0 {
		redactedIDs = extraRedactedIDs[0]
	}
	ctx = telemetry.WithFields(ctx, map[string]any{"session_id": session.ID, "turn_id": "analysis/" + session.ID, "conversation_id": session.ConversationID, "trigger": "analysis"})
	ctx = domain.WithModelRole(ctx, domain.RoleAnalysis)
	event := a.event(ctx, "llm.complete")
	ctx = event.Context()
	finish := a.startTrace(ctx, domain.Trace{SessionID: session.ID, TurnID: "analysis/" + session.ID, Trigger: "analysis", Kind: "analysis", Name: string(domain.RoleAnalysis), ModelRole: domain.RoleAnalysis}, map[string]any{"messages": sanitizeAnalysisTraceMessages(messages, redactedIDs), "context_stage": "before_profile_fitting"}, event)
	defer func() {
		if out.Usage.Model == "" {
			out.Usage.Model = out.Model
		}
		if out.Usage.Role == "" {
			out.Usage.Role = domain.RoleAnalysis
		}
		finish(out, err, out.Model)
		modelUsageMetadata(event, out.Usage)
		event.Set("input_tokens", out.InputTokens)
		event.Set("output_tokens", out.OutputTokens)
		event.End(err)
	}()
	out, err = a.Model.Complete(domain.WithModelSession(ctx, ID(session.ConversationID)), messages, nil)
	return out, modelCallError(err)
}
