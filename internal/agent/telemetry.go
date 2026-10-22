package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/telemetry"
)

func trigger(task string, initiative bool) string {
	if task != "" {
		return "task"
	}
	if initiative {
		return "initiative"
	}
	return "message"
}
func runContext(ctx context.Context, in RunInput) context.Context {
	return telemetry.WithFields(ctx, map[string]any{"turn_id": in.Key, "conversation_id": in.ConversationID, "session_id": in.SessionID, "task_id": in.TaskID, "trigger": trigger(in.TaskID, in.Initiative)})
}
func (in ToolInput) TriggerName() string { return trigger(in.TaskID, in.Background) }

type diagnosticError string

func (e diagnosticError) Error() string          { return string(e) }
func (e diagnosticError) DiagnosticCode() string { return string(e) }

func toolMetadata(ev *telemetry.Event, in ToolInput) {
	// Explicit allowlist: never include raw arguments, queries, page text or URL paths.
	var args struct {
		URL  string `json:"url"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(in.Call.Arguments, &args) != nil {
		return
	}
	if in.Call.Name == "web_read" || in.Call.Name == "browser_open" || in.Call.Name == "browser_navigate" || in.Call.Name == "browser_download" {
		if u, err := url.Parse(args.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil {
			ev.Set("destination_host", u.Hostname())
		}
	}
	if in.Call.Name == "memory_update" {
		for _, kind := range []string{"profile", "preference", "fact", "episode", "hypothesis"} {
			if args.Kind == kind {
				ev.Set("memory_kind", kind)
			}
		}
	}
}

func toolResultMetadata(ev *telemetry.Event, result any) {
	switch v := result.(type) {
	case domain.LogSearch:
		ev.Set("result_count", len(v.Events))
		ev.Set("observation_truncated", v.Truncated || v.SummaryTruncated)
	case map[string]string:
		if target := v["conversation_id"]; target != "" {
			ev.Set("target_conversation_id", target)
		}
	case domain.BrowserResult:
		if v.PageID != "" {
			ev.Set("page_id", v.PageID)
		}
		if v.InstanceID != "" {
			ev.Set("browser_instance_id", v.InstanceID)
		}
		if v.ArtifactID != "" {
			ev.Set("artifact_id", v.ArtifactID)
		}
		ev.Set("observation_truncated", v.Truncated)
		ev.Set("result_chars", len([]rune(v.Text)))
	case []domain.Participant:
		ev.Set("result_count", len(v))
	case []domain.MemoryEntry:
		ev.Set("result_count", len(v))
	case []domain.Task:
		ev.Set("result_count", len(v))
	case []domain.SearchResult:
		ev.Set("result_count", len(v))
	case string:
		ev.Set("result_chars", len([]rune(v)))
	}
}

func (a *Activities) turnSummary(ctx context.Context, in FinishInput, persistErr error) {
	t := in.Turn
	ev := a.event(ctx, "turn.complete")
	ev.Set("model_calls", t.Stats.ModelCalls)
	ev.Set("tool_calls", t.Stats.ToolCalls)
	ev.Set("tool_errors", t.Stats.ToolErrors)
	ev.Set("input_tokens", t.Stats.InputTokens)
	ev.Set("output_tokens", t.Stats.OutputTokens)
	ev.Set("tools", t.Stats.Tools)
	ev.Set("incoming_count", len(t.Input.IncomingIDs))
	reason := t.Stats.FinishReason
	if in.Failed {
		reason = "failed"
	}
	if in.FailureCode == "cancelled" {
		reason = "cancelled"
	}
	if reason == "" {
		reason = "no_reply"
		if t.Sent {
			reason = "reply_queued"
		}
	}
	ev.Set("finish_reason", reason)
	state := "not_requested"
	if t.Sent {
		state = "queued"
	}
	ev.Set("delivery_state", state)
	if !t.Input.StartedAt.IsZero() {
		ev.Set("processing_ms", sinceMS(t.Input.StartedAt))
	}
	if !t.ReceivedAt.IsZero() {
		ev.Set("since_received_ms", sinceMS(t.ReceivedAt))
	}
	if in.Failed {
		ev.Fail(diagnosticError(in.FailureCode))
	}
	ev.End(persistErr)
}
func sinceMS(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }

// ToolFailed only examines the top-level protocol error; quoted page text can contain "error".
func ToolFailed(raw string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return false
	}
	value, ok := fields["error"]
	return ok && string(value) != "null" && strings.TrimSpace(string(value)) != `""`
}
