package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Diagnostic contains only fixed, public descriptions; never upstream error bodies or URLs.
type Diagnostic struct {
	Code, Reason string
	Retryable    bool
	Status       int
}

func Classify(err error) Diagnostic {
	if err == nil {
		return Diagnostic{}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Diagnostic{Code: "timeout", Reason: "Operation deadline exceeded", Retryable: true}
	}
	if errors.Is(err, context.Canceled) {
		return Diagnostic{Code: "cancelled", Reason: "Operation was cancelled"}
	}
	var uncertain interface{ DeliveryUnknown() bool }
	if errors.As(err, &uncertain) && uncertain.DeliveryUnknown() {
		return Diagnostic{Code: "delivery_unknown", Reason: "Delivery may have succeeded; automatic repetition can duplicate the message"}
	}
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		return httpDiagnostic(status.HTTPStatus())
	}
	var coded interface{ DiagnosticCode() string }
	if errors.As(err, &coded) {
		if strings.HasPrefix(coded.DiagnosticCode(), "http_") {
			if code, err := strconv.Atoi(strings.TrimPrefix(coded.DiagnosticCode(), "http_")); err == nil && code >= 100 && code <= 599 {
				return httpDiagnostic(code)
			}
		}
		if d, ok := known[coded.DiagnosticCode()]; ok {
			return d
		}
	}
	var typed interface{ Type() string }
	if errors.As(err, &typed) {
		if d, ok := known[typed.Type()]; ok {
			return d
		}
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return Diagnostic{Code: "timeout", Reason: "Network deadline exceeded", Retryable: true}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return known["dns_failed"]
	}
	var connection *net.OpError
	if errors.As(err, &connection) {
		return known["connection_failed"]
	}
	// Exact matches only. Unknown errors are deliberately never copied into logs.
	for source := err; source != nil; source = errors.Unwrap(source) {
		if d, ok := known[source.Error()]; ok {
			return d
		}
		if code, ok := errorCodes[source.Error()]; ok {
			return known[code]
		}
	}
	return Diagnostic{Code: "operation_failed", Reason: "Operation failed; inspect the associated workflow and stored result"}
}

func httpDiagnostic(code int) Diagnostic {
	reason := map[int]string{
		400: "Upstream rejected the request format or parameters",
		401: "Upstream authentication failed",
		403: "Upstream denied access to the requested resource",
		404: "Upstream resource or endpoint was not found",
		415: "Upstream does not support the request media type",
		422: "Upstream could not process the supplied input",
		429: "Upstream rate limit was exceeded",
	}[code]
	if reason == "" {
		reason = "Upstream returned a non-success status"
		if code >= 500 {
			reason = "Upstream service failed to process the request"
		}
	}
	return Diagnostic{Code: fmt.Sprintf("http_%d", code), Reason: reason, Status: code, Retryable: code == 429 || code >= 500}
}

var known = map[string]Diagnostic{
	"initiative_task_update_denied":  {Code: "initiative_task_update_denied", Reason: "Initiative checks cannot modify existing tasks"},
	"logs_access_denied":             {Code: "logs_access_denied", Reason: "Log search requires the owner's private chat or its scheduled tasks"},
	"logs_disabled":                  {Code: "logs_disabled", Reason: "VictoriaLogs read access is not configured"},
	"logs_invalid_response":          {Code: "logs_invalid_response", Reason: "VictoriaLogs returned malformed data or data outside the authorized conversation"},
	"logs_response_limit":            {Code: "logs_response_limit", Reason: "VictoriaLogs response exceeds the read limit or could not be read"},
	"connection_failed":              {Code: "connection_failed", Reason: "Network connection to the upstream could not be established or maintained", Retryable: true},
	"unsupported_content_type":       {Code: "unsupported_content_type", Reason: "Page reader received a format other than HTML or plain text; use the browser or an artifact reader"},
	"response_too_large":             {Code: "response_too_large", Reason: "Upstream response exceeds the configured size limit"},
	"response_read_failed":           {Code: "response_read_failed", Reason: "Upstream response body could not be read", Retryable: true},
	"model_response_truncated":       {Code: "model_response_truncated", Reason: "Model response reached the provider token limit before completing"},
	"invalid_model_response":         {Code: "invalid_model_response", Reason: "Model returned missing, empty or malformed content or tool calls"},
	"model_response_too_large":       {Code: "model_response_too_large", Reason: "Model response exceeds the Activity payload limit"},
	"too_many_tool_calls":            {Code: "too_many_tool_calls", Reason: "Model requested more tool calls than one step permits"},
	"browser_unavailable":            {Code: "browser_unavailable", Reason: "Chromium could not start or has stopped; open a new browser session"},
	"browser_page_limit":             {Code: "browser_page_limit", Reason: "Browser has reached its page limit; close an unused page"},
	"browser_observation_failed":     {Code: "browser_observation_failed", Reason: "Browser page could not be observed; reopen it if the browser restarted"},
	"search_disabled":                {Code: "search_disabled", Reason: "Search API is disabled; use available browser tools or read a known URL"},
	"redirect_limit":                 {Code: "redirect_limit", Reason: "Upstream exceeded the allowed redirect count"},
	"telegram_request_failed":        {Code: "telegram_request_failed", Reason: "Telegram API request could not be completed", Retryable: true},
	"invalid_telegram_response":      {Code: "invalid_telegram_response", Reason: "Telegram API returned an invalid response envelope or result"},
	"recipient_not_allowed":          {Code: "recipient_not_allowed", Reason: "Telegram recipient is not allowed by this installation"},
	"invalid_message":                {Code: "invalid_message", Reason: "Outgoing message is empty or exceeds Telegram format or size limits"},
	"artifact_not_found":             {Code: "artifact_not_found", Reason: "Referenced artifact was not found in storage"},
	"invalid_arguments":              {Code: "invalid_arguments", Reason: "Tool arguments are invalid"},
	"stale_element":                  {Code: "stale_element", Reason: "Element reference expired; take a fresh snapshot"},
	"page_expired":                   {Code: "page_expired", Reason: "Browser page no longer exists"},
	"network_policy":                 {Code: "network_policy", Reason: "Destination is blocked by the network policy"},
	"uncertain_browser_action":       {Code: "uncertain_browser_action", Reason: "Previous action may have executed; observe before continuing"},
	"dns_failed":                     {Code: "dns_failed", Reason: "Destination name could not be resolved", Retryable: true},
	"upstream_unavailable":           {Code: "upstream_unavailable", Reason: "Upstream request could not be completed", Retryable: true},
	"invalid_upstream_response":      {Code: "invalid_upstream_response", Reason: "Upstream response is invalid"},
	"timeout":                        {Code: "timeout", Reason: "Operation deadline exceeded", Retryable: true},
	"cancelled":                      {Code: "cancelled", Reason: "Operation was cancelled"},
	"step_limit":                     {Code: "step_limit", Reason: "Agent exhausted its configured step budget"},
	"browser_capacity":               {Code: "browser_capacity", Reason: "All eligible browser slots are occupied", Retryable: true},
	"all browser slots are occupied": {Code: "browser_capacity", Reason: "All eligible browser slots are occupied", Retryable: true},
	"unknown or stale element reference; take a fresh browser_snapshot":                                {Code: "stale_element", Reason: "Element reference expired; take a fresh snapshot"},
	"previous browser action may already have executed; take a fresh snapshot instead of replaying it": {Code: "uncertain_browser_action", Reason: "Previous action may have executed; observe before continuing"},
	"page not found in this browser; use browser_open (page IDs expire after browser restart)":         {Code: "page_expired", Reason: "Browser page no longer exists"},
	"web host resolves to a non-public address":                                                        {Code: "network_policy", Reason: "Non-public destination is blocked"},
	"invalid tool JSON":              {Code: "invalid_arguments", Reason: "Tool arguments are not valid JSON"},
	"query required":                 {Code: "invalid_arguments", Reason: "Required search query is missing"},
	"upstream request failed":        {Code: "upstream_unavailable", Reason: "Upstream request could not be completed", Retryable: true},
	"web request failed":             {Code: "upstream_unavailable", Reason: "Public page request could not be completed", Retryable: true},
	"web DNS lookup failed":          {Code: "dns_failed", Reason: "Destination name could not be resolved", Retryable: true},
	"invalid upstream JSON response": {Code: "invalid_upstream_response", Reason: "Upstream response is not valid JSON"},
	"agent step limit reached":       {Code: "step_limit", Reason: "Agent exhausted its configured step budget"},
}

// Only exact errors authored by this project are translated. Arbitrary error
// strings, URLs and upstream bodies never become diagnostic log fields.
var errorCodes = map[string]string{
	"initiative checks cannot update existing tasks; read tasks or create a justified new task, and keep the check result in the run history": "initiative_task_update_denied",
	"log search is only available in the owner's private chat or its scheduled tasks":                                                         "logs_access_denied",
	"log search is disabled; VICTORIALOGS_URL is not configured":                                                                              "logs_disabled",
	"invalid log filters":                                                    "invalid_arguments",
	"log timestamps must be RFC3339 with a timezone":                         "invalid_arguments",
	"log time range must be positive and at most 24 hours":                   "invalid_arguments",
	"invalid VictoriaLogs response":                                          "logs_invalid_response",
	"VictoriaLogs response exceeds limit or could not be read":               "logs_response_limit",
	"VictoriaLogs query failed":                                              "upstream_unavailable",
	"web response is not HTML or plain text":                                 "unsupported_content_type",
	"upstream body read failed":                                              "response_read_failed",
	"upstream response exceeds size limit":                                   "response_too_large",
	"model response truncated by token limit":                                "model_response_truncated",
	"model returned no choices":                                              "invalid_model_response",
	"model returned empty response":                                          "invalid_model_response",
	"invalid model content":                                                  "invalid_model_response",
	"model returned invalid tool call":                                       "invalid_model_response",
	"model returned invalid analysis JSON":                                   "invalid_model_response",
	"model response exceeds activity limit":                                  "model_response_too_large",
	"too many tool calls":                                                    "too_many_tool_calls",
	"Chromium startup failed":                                                "browser_unavailable",
	"Chromium is missing; set BROWSER_EXECUTABLE or disable BROWSER_ENABLED": "browser_unavailable",
	"Chromium stopped; old page IDs expired, use browser_open":               "browser_unavailable",
	"no active browser; old page IDs expired, use browser_open":              "page_expired",
	"browser closed; open a new page":                                        "page_expired",
	"browser closed; old page IDs have expired":                              "page_expired",
	"maximum eight pages per browser; close an unused page":                  "browser_page_limit",
	"browser page creation failed":                                           "browser_unavailable",
	"cannot observe browser page; reopen it if Chromium restarted":           "browser_observation_failed",
	"element no longer exists; take a fresh snapshot":                        "stale_element",
	"browser operation failed; for clicks the action may already have occurred, inspect a fresh snapshot before retrying": "uncertain_browser_action",
	"browser operation cancelled or timed out; browser closed, previous action outcome may be unknown":                    "uncertain_browser_action",
	"browser is not on a public HTTP(S) page":            "network_policy",
	"authenticated services are not supported":           "network_policy",
	"web URL must be public HTTP(S) without credentials": "network_policy",
	"web URL port must be 80 or 443":                     "network_policy",
	"too many redirects":                                 "redirect_limit",
	"web_search API tool is disabled; use listed browser tools to find sources or web_read for a known public URL": "search_disabled",
	"Telegram request failed":                                         "telegram_request_failed",
	"invalid Telegram response":                                       "invalid_telegram_response",
	"invalid Telegram response envelope":                              "invalid_telegram_response",
	"invalid Telegram result":                                         "invalid_telegram_response",
	"Telegram recipient is not allowed":                               "recipient_not_allowed",
	"Telegram message is empty":                                       "invalid_message",
	"Telegram rich text exceeds 32768 characters or is invalid UTF-8": "invalid_message",
	"outgoing artifact missing":                                       "artifact_not_found",
	"artifact not found":                                              "artifact_not_found",
}
