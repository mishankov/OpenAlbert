package domain

import (
	"bytes"
	"encoding/json"
	"strings"
)

const codemodeToolName = "codemode"

// SanitizeWebSearchTraceInput strips raw queries, domain filter values, and date
// strings so inspection traces persist only allowlisted non-sensitive metadata.
func SanitizeWebSearchTraceInput(input any, provider string) map[string]any {
	out := map[string]any{"query_redacted": true}
	if provider != "" {
		out["provider"] = provider
	}
	var raw []byte
	switch v := input.(type) {
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		raw, _ = json.Marshal(v)
	}
	var q struct {
		Mode               string   `json:"mode"`
		NumResults         *int     `json:"num_results"`
		IncludeDomains     []string `json:"include_domains"`
		ExcludeDomains     []string `json:"exclude_domains"`
		StartPublishedDate string   `json:"start_published_date"`
		EndPublishedDate   string   `json:"end_published_date"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &q) == nil {
		if mode := sanitizeSearchMode(q.Mode, provider); mode != "" {
			out["mode"] = mode
		}
		if q.NumResults != nil {
			out["num_results"] = *q.NumResults
		}
		if len(q.IncludeDomains) > 0 {
			out["include_domains_count"] = len(q.IncludeDomains)
		}
		if len(q.ExcludeDomains) > 0 {
			out["exclude_domains_count"] = len(q.ExcludeDomains)
		}
		if strings.TrimSpace(q.StartPublishedDate) != "" || strings.TrimSpace(q.EndPublishedDate) != "" {
			out["has_date_filter"] = true
		}
	}
	return out
}

func sanitizeSearchMode(raw, provider string) string {
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

// CollectRedactedToolCallIDs returns every web_search and codemode tool call ID
// appearing in messages (including ResponsesItems), so trace redaction remains
// complete even when context fitting drops an earlier assistant tool call while
// retaining its tool result message.
func CollectRedactedToolCallIDs(messages []Message) map[string]bool {
	ids := map[string]bool{}
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			if _, redacted := sanitizeTraceToolCall(tc); redacted && tc.ID != "" {
				ids[tc.ID] = true
			}
		}
		if len(m.ResponsesItems) > 0 {
			_ = sanitizeTraceResponsesItems(m.ResponsesItems, ids)
		}
	}
	return ids
}

// SanitizeTraceMessages prepares a safe inspection copy of model messages:
// binary image payloads are replaced with placeholders and web_search /
// codemode tool calls and tool results are redacted so inspection traces never
// persist raw search queries, domains, or excerpts (including dynamic/catalog
// Codemode calls such as searchTools('exa')).
func SanitizeTraceMessages(messages []Message) []Message {
	return SanitizeTraceMessagesWithRedactedIDs(messages, nil)
}

// SanitizeTraceMessagesWithRedactedIDs behaves like SanitizeTraceMessages while
// also redacting tool result messages matching any pre-collected tool call IDs
// whose originating assistant tool call was trimmed during context fitting.
func SanitizeTraceMessagesWithRedactedIDs(messages []Message, extraRedactedIDs map[string]bool) []Message {
	out := make([]Message, len(messages))
	redactedToolCallIDs := map[string]bool{}
	for id, ok := range extraRedactedIDs {
		if ok && id != "" {
			redactedToolCallIDs[id] = true
		}
	}
	for i, m := range messages {
		copyMsg := m
		if len(m.Images) > 0 {
			copyMsg.Images = make([]string, len(m.Images))
			for j := range copyMsg.Images {
				copyMsg.Images[j] = "[image supplied to model; binary omitted from inspection snapshot]"
			}
		}
		if len(m.ToolCalls) > 0 {
			copyMsg.ToolCalls = make([]ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				redactedTC, redacted := sanitizeTraceToolCall(tc)
				copyMsg.ToolCalls[j] = redactedTC
				if redacted && tc.ID != "" {
					redactedToolCallIDs[tc.ID] = true
				}
			}
		}
		if len(m.ResponsesItems) > 0 {
			copyMsg.ResponsesItems = sanitizeTraceResponsesItems(m.ResponsesItems, redactedToolCallIDs)
		}
		if m.Role == "tool" && (redactedToolCallIDs[m.ToolCallID] || (m.ParentToolCallID != "" && redactedToolCallIDs[m.ParentToolCallID])) {
			copyMsg.Content = sanitizeWebSearchToolResultContent(m.Content)
		}
		out[i] = copyMsg
	}
	return out
}

// SanitizeTraceCompletion prepares a safe inspection copy of a model completion
// by redacting web_search and codemode tool-call arguments and ResponsesItems.
func SanitizeTraceCompletion(comp Completion) Completion {
	if len(comp.Message.ToolCalls) == 0 && len(comp.Message.ResponsesItems) == 0 {
		return comp
	}
	out := comp
	out.Message = SanitizeTraceMessageToolCalls(comp.Message)
	return out
}

// SanitizeTraceMessageToolCalls returns a copy of msg with web_search and
// codemode tool-call arguments and ResponsesItems redacted.
func SanitizeTraceMessageToolCalls(msg Message) Message {
	out := msg
	redactedIDs := map[string]bool{}
	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = make([]ToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			redactedTC, redacted := sanitizeTraceToolCall(tc)
			out.ToolCalls[i] = redactedTC
			if redacted && tc.ID != "" {
				redactedIDs[tc.ID] = true
			}
		}
	}
	if len(msg.ResponsesItems) > 0 {
		out.ResponsesItems = sanitizeTraceResponsesItems(msg.ResponsesItems, redactedIDs)
	}
	return out
}

// SanitizeModelCheckpointJSON redacts web_search and codemode tool calls inside
// a persisted historical model checkpoint payload before returning it through
// Inspector read APIs.
func SanitizeModelCheckpointJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"Message", "message"} {
		msgRaw, ok := envelope[key]
		if !ok {
			continue
		}
		var msg Message
		if json.Unmarshal(msgRaw, &msg) != nil {
			continue
		}
		if len(msg.ToolCalls) == 0 && len(msg.ResponsesItems) == 0 {
			continue
		}
		sanitized := SanitizeTraceMessageToolCalls(msg)
		encoded, err := json.Marshal(sanitized)
		if err != nil {
			continue
		}
		envelope[key] = encoded
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return out
}

func sanitizeTraceToolCall(tc ToolCall) (ToolCall, bool) {
	if tc.Name == "web_search" {
		raw, _ := json.Marshal(SanitizeWebSearchTraceInput(tc.Arguments, ""))
		tc.Arguments = raw
		return tc, true
	}
	if tc.Name == codemodeToolName {
		tc.Arguments = json.RawMessage(`{"code_redacted":true,"reason":"codemode"}`)
		return tc, true
	}
	return tc, false
}

func sanitizeTraceResponsesItems(items []json.RawMessage, redactedIDs map[string]bool) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i, raw := range items {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			out[i] = raw
			continue
		}
		typ, _ := obj["type"].(string)
		name, _ := obj["name"].(string)
		callID, _ := obj["call_id"].(string)
		switch {
		case typ == "function_call" && name == "web_search":
			if argsStr, ok := obj["arguments"].(string); ok {
				sanitized, _ := json.Marshal(SanitizeWebSearchTraceInput(argsStr, ""))
				obj["arguments"] = string(sanitized)
			} else {
				obj["arguments"] = `{"query_redacted":true}`
			}
			if callID != "" {
				redactedIDs[callID] = true
			}
			encoded, _ := json.Marshal(obj)
			out[i] = encoded
		case typ == "function_call" && name == codemodeToolName:
			obj["arguments"] = `{"code_redacted":true,"reason":"codemode"}`
			if callID != "" {
				redactedIDs[callID] = true
			}
			encoded, _ := json.Marshal(obj)
			out[i] = encoded
		case typ == "function_call_output" && redactedIDs[callID]:
			if outStr, ok := obj["output"].(string); ok {
				obj["output"] = sanitizeWebSearchToolResultContent(outStr)
			} else {
				obj["output"] = `{"results_redacted":true}`
			}
			encoded, _ := json.Marshal(obj)
			out[i] = encoded
		default:
			out[i] = raw
		}
	}
	return out
}

func sanitizeWebSearchToolResultContent(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return `{"results_redacted":true}`
	}
	var arr []json.RawMessage
	if json.Unmarshal([]byte(trimmed), &arr) == nil {
		raw, _ := json.Marshal(map[string]any{"results_redacted": true, "result_count": len(arr)})
		return string(raw)
	}
	var payload struct {
		ErrorCode         string `json:"error_code"`
		Retryable         *bool  `json:"retryable"`
		Fallback          string `json:"fallback"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if json.Unmarshal([]byte(trimmed), &payload) == nil && payload.ErrorCode != "" {
		out := map[string]any{"results_redacted": true, "error_code": payload.ErrorCode}
		if payload.Retryable != nil {
			out["retryable"] = *payload.Retryable
		}
		if payload.Fallback == "browser" || payload.Fallback == "web_read" {
			out["fallback"] = payload.Fallback
		}
		if payload.RetryAfterSeconds > 0 {
			out["retry_after_seconds"] = payload.RetryAfterSeconds
		}
		raw, _ := json.Marshal(out)
		return string(raw)
	}
	return `{"results_redacted":true}`
}
