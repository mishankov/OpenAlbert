package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
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

func modelUsageMetadata(ev *telemetry.Event, usage domain.ModelUsage) {
	ev.Set("model_role", usage.Role)
	if usage.Profile != "" {
		ev.Set("model_profile", usage.Profile)
	}
	if usage.Model != "" {
		ev.Set("model", usage.Model)
	}
	ev.Set("cached_input_tokens", usage.CachedInputTokens)
	ev.Set("token_usage_known", usage.TokenUsageKnown)
	ev.Set("dropped_messages", usage.DroppedMessages)
	if usage.EstimatedInputTokens > 0 {
		ev.Set("estimated_input_tokens", usage.EstimatedInputTokens)
	}
	if usage.EstimatedCostUSD != nil {
		ev.Set("estimated_cost_usd", *usage.EstimatedCostUSD)
	}
}

type diagnosticError string

func (e diagnosticError) Error() string          { return string(e) }
func (e diagnosticError) DiagnosticCode() string { return string(e) }

func (a *Activities) toolMetadata(ev *telemetry.Event, in ToolInput) {
	// Explicit allowlist: never include raw arguments, queries, page text or URL paths.
	if in.Call.Name == "web_search" {
		if provider := a.searchProvider(); provider != "" {
			ev.Set("search_provider", provider)
		}
		var q domain.SearchQuery
		if json.Unmarshal(in.Call.Arguments, &q) == nil {
			if mode := safeSearchMode(q.Mode, a.searchProvider()); mode != "" {
				ev.Set("search_mode", mode)
			}
			if q.NumResults != nil && *q.NumResults >= 1 && *q.NumResults <= 10 {
				ev.Set("search_num_results_requested", *q.NumResults)
			}
			ev.Set("search_has_domain_filter", len(q.IncludeDomains) > 0 || len(q.ExcludeDomains) > 0)
			ev.Set("search_has_date_filter", strings.TrimSpace(q.StartPublishedDate) != "" || strings.TrimSpace(q.EndPublishedDate) != "")
		}
		return
	}
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

func safeSearchMode(raw, provider string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "lite":
		return strings.ToLower(strings.TrimSpace(raw))
	case "":
		if provider == "exa" {
			return "auto"
		}
	}
	return ""
}

func safeResolvedSearchMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "lite", "neural", "keyword", "fast", "deep", "deep-lite":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func safeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 {
		return ""
	}
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return ""
		}
	}
	return raw
}

func (a *Activities) buildSearchTrace(rawArgs json.RawMessage, result any, callErr error) *domain.SearchTrace {
	provider := a.searchProvider()
	if provider == "" {
		return nil
	}
	trace := &domain.SearchTrace{Provider: provider}
	var q domain.SearchQuery
	if json.Unmarshal(rawArgs, &q) == nil {
		trace.Mode = safeSearchMode(q.Mode, provider)
	}
	if callErr == nil {
		if resp, ok := result.(domain.SearchResponse); ok {
			if resp.Provider != "" {
				trace.Provider = resp.Provider
			}
			trace.ResolvedMode = safeResolvedSearchMode(resp.ResolvedMode)
			trace.HTTPStatus = 200
			trace.RateLimited = resp.RateLimited
			trace.ResultCount = len(resp.Results)
			trace.RequestID = safeRequestID(resp.RequestID)
			if resp.CostUSD != nil && !math.IsNaN(*resp.CostUSD) && !math.IsInf(*resp.CostUSD, 0) && *resp.CostUSD >= 0 {
				cost := *resp.CostUSD
				trace.CostUSD = &cost
			}
			completed := resp.CompletedAt.UTC()
			if completed.IsZero() {
				completed = time.Now().UTC()
			}
			trace.CompletedAt = &completed
		}
		return trace
	}
	diag := telemetry.Classify(callErr)
	if diag.Status != 0 {
		trace.HTTPStatus = diag.Status
	}
	var rl interface{ WasRateLimited() bool }
	if diag.Status == 429 || (errors.As(callErr, &rl) && rl.WasRateLimited()) {
		trace.RateLimited = true
	}
	completed := time.Now().UTC()
	var withCompleted interface{ SearchCompletedAt() time.Time }
	if errors.As(callErr, &withCompleted) {
		if t := withCompleted.SearchCompletedAt().UTC(); !t.IsZero() {
			completed = t
		}
	}
	trace.CompletedAt = &completed
	return trace
}

// fitSearchResponseBytes trims highlights/snippets (and trailing items only as
// a last resort) so web_search tool outputs always stay within byte limits
// without losing their JSON array shape or corrupting URLs.
func fitSearchResponseBytes(resp domain.SearchResponse, limit int) domain.SearchResponse {
	if limit <= 0 || len(resp.Results) == 0 {
		return resp
	}
	if raw, err := json.Marshal(resp); err == nil && len(raw) <= limit {
		return resp
	}
	for _, maxRunes := range []int{700, 450, 260, 140, 60, 0} {
		candidate := resp
		candidate.Results = make([]domain.SearchResult, len(resp.Results))
		for i, r := range resp.Results {
			item := r
			if maxRunes == 0 {
				item.Highlights = nil
				item.Snippet = ""
			} else {
				item.Highlights = clipSearchHighlights(r.Highlights, maxRunes)
				if item.Snippet != "" {
					item.Snippet = clipRunes(item.Snippet, maxRunes)
				}
			}
			candidate.Results[i] = item
		}
		if raw, err := json.Marshal(candidate); err == nil && len(raw) <= limit {
			return candidate
		}
	}
	// If titles/URLs alone still exceed the budget, keep as many intact results as fit.
	trimmed := resp
	trimmed.Results = make([]domain.SearchResult, len(resp.Results))
	for i, r := range resp.Results {
		trimmed.Results[i] = domain.SearchResult{
			Title:         clipRunes(r.Title, 200),
			URL:           r.URL,
			PublishedDate: r.PublishedDate,
		}
	}
	for len(trimmed.Results) > 0 {
		if raw, err := json.Marshal(trimmed); err == nil && len(raw) <= limit {
			return trimmed
		}
		trimmed.Results = trimmed.Results[:len(trimmed.Results)-1]
	}
	return trimmed
}

func clipRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	if maxRunes == 1 {
		return "…"
	}
	return strings.TrimSpace(string(r[:maxRunes-1])) + "…"
}

func clipSearchHighlights(items []string, maxRunes int) []string {
	if len(items) == 0 || maxRunes <= 0 {
		return nil
	}
	remaining := maxRunes
	var out []string
	for _, item := range items {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		r := []rune(text)
		if len(r) <= remaining {
			out = append(out, text)
			remaining -= len(r)
			if remaining == 0 {
				break
			}
			continue
		}
		if remaining > 1 {
			out = append(out, strings.TrimSpace(string(r[:remaining-1]))+"…")
		}
		break
	}
	return out
}

// sanitizeWebSearchTraceOutput strips search result titles, URLs, snippets, and
// highlights from persisted trace outputs while keeping allowlisted counters
// and structured limit/error codes.
func sanitizeWebSearchTraceOutput(output any, meta *domain.SearchTrace, failed bool) map[string]any {
	out := map[string]any{"results_redacted": true}
	if meta != nil {
		if meta.Provider != "" {
			out["provider"] = meta.Provider
		}
		if meta.Mode != "" {
			out["mode"] = meta.Mode
		}
		if meta.ResolvedMode != "" {
			out["resolved_mode"] = meta.ResolvedMode
		}
		if meta.HTTPStatus != 0 {
			out["http_status"] = meta.HTTPStatus
		}
		if meta.RateLimited {
			out["rate_limited"] = true
		}
		if meta.RequestID != "" {
			out["request_id"] = meta.RequestID
		}
		if !failed {
			out["result_count"] = meta.ResultCount
		}
		if meta.CostUSD != nil {
			out["cost_usd"] = *meta.CostUSD
		}
	}
	if failed {
		if raw, ok := output.(string); ok {
			var payload struct {
				ErrorCode         string `json:"error_code"`
				Retryable         *bool  `json:"retryable"`
				Fallback          string `json:"fallback"`
				RetryAfterSeconds int    `json:"retry_after_seconds"`
			}
			if json.Unmarshal([]byte(raw), &payload) == nil {
				if payload.ErrorCode != "" {
					out["error_code"] = payload.ErrorCode
				}
				if payload.Retryable != nil {
					out["retryable"] = *payload.Retryable
				}
				if payload.Fallback == "browser" || payload.Fallback == "web_read" {
					out["fallback"] = payload.Fallback
				}
				if payload.RetryAfterSeconds > 0 {
					out["retry_after_seconds"] = payload.RetryAfterSeconds
				}
			}
		}
	}
	return out
}

func (a *Activities) webSearchErrorResult(err error, diag telemetry.Diagnostic) map[string]any {
	fallback := "web_read"
	fallbackHint := "use web_read for known public URLs or ask the user for a source link"
	if a.Browser != nil {
		fallback = "browser"
		fallbackHint = "use listed browser tools to search public websites or web_read for known public URLs"
	}
	switch diag.Status {
	case 402:
		return map[string]any{
			"error":      "web_search credits are exhausted for the current billing period; do not retry web_search in this turn — " + fallbackHint,
			"error_code": diag.Code,
			"retryable":  false,
			"fallback":   fallback,
		}
	case 429:
		out := map[string]any{
			"error":      "web_search rate limit exceeded; wait briefly before retrying or " + fallbackHint,
			"error_code": diag.Code,
			"retryable":  true,
			"fallback":   fallback,
		}
		var withRetry interface{ RetryAfterDuration() time.Duration }
		if errors.As(err, &withRetry) {
			if d := withRetry.RetryAfterDuration(); d > 0 {
				out["retry_after_seconds"] = int(math.Ceil(d.Seconds()))
			}
		}
		return out
	case 401, 403:
		return map[string]any{
			"error":      "web_search authentication or access failed; do not retry web_search in this turn — " + fallbackHint,
			"error_code": diag.Code,
			"retryable":  false,
			"fallback":   fallback,
		}
	default:
		return nil
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
		if executionIDPattern.MatchString(v["execution_id"]) {
			ev.Set("execution_id", v["execution_id"])
		}
	case domain.ComputerResult:
		if executionIDPattern.MatchString(v.ExecutionID) {
			ev.Set("execution_id", v.ExecutionID)
		}
		switch v.State {
		case "queued", "running", "completed", "failed", "cancelled", "timed_out", "unknown":
			ev.Set("computer_state", v.State)
		}
		if v.ExitCode != nil {
			ev.Set("exit_code", *v.ExitCode)
		}
		ev.Set("output_truncated", v.OutputTruncated)
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
	case domain.SearchResponse:
		ev.Set("result_count", len(v.Results))
		chars := 0
		for _, item := range v.Results {
			chars += utf8.RuneCountInString(item.Snippet)
			for _, h := range item.Highlights {
				chars += utf8.RuneCountInString(h)
			}
		}
		ev.Set("result_chars", chars)
		if v.Provider != "" {
			ev.Set("search_provider", v.Provider)
		}
		if resolved := safeResolvedSearchMode(v.ResolvedMode); resolved != "" {
			ev.Set("search_resolved_mode", resolved)
		}
		if v.CostUSD != nil && !math.IsNaN(*v.CostUSD) && !math.IsInf(*v.CostUSD, 0) && *v.CostUSD >= 0 {
			ev.Set("search_cost_usd", *v.CostUSD)
		}
		if v.RateLimited {
			ev.Set("search_rate_limited", true)
		}
		if reqID := safeRequestID(v.RequestID); reqID != "" {
			ev.Set("search_request_id", reqID)
		}
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
