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
		code := status.HTTPStatus()
		return Diagnostic{Code: fmt.Sprintf("http_%d", code), Reason: "Upstream returned a non-success status", Status: code, Retryable: code == 429 || code >= 500}
	}
	var coded interface{ DiagnosticCode() string }
	if errors.As(err, &coded) {
		if strings.HasPrefix(coded.DiagnosticCode(), "http_") {
			if code, err := strconv.Atoi(strings.TrimPrefix(coded.DiagnosticCode(), "http_")); err == nil && code >= 100 && code <= 599 {
				return Diagnostic{Code: fmt.Sprintf("http_%d", code), Reason: "Upstream returned a non-success status", Status: code, Retryable: code == 429 || code >= 500}
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
	// Exact matches only. Unknown errors are deliberately never copied into logs.
	for source := err; source != nil; source = errors.Unwrap(source) {
		if d, ok := known[source.Error()]; ok {
			return d
		}
	}
	return Diagnostic{Code: "operation_failed", Reason: "Operation failed; inspect the associated workflow and stored result"}
}

var known = map[string]Diagnostic{
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
