// Package telemetry emits one wide event per completed operation. Callers supply
// identifiers, counts and classified outcomes, never prompts, credentials or bodies.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
)

func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "openalbert", "log_schema_version", 4)
}

type Event struct {
	ctx       context.Context
	logger    *slog.Logger
	operation string
	start     time.Time
	mu        sync.Mutex
	fields    map[string]any
	once      sync.Once
	failure   error
	parent    *Event
	sql       sqlStats
	ended     bool
}

type eventKey struct{}

func Start(ctx context.Context, logger *slog.Logger, operation string) *Event {
	if logger == nil {
		logger = slog.Default()
	}
	e := &Event{ctx: ctx, logger: logger, operation: operation, start: time.Now(), fields: map[string]any{}}
	e.parent, _ = ctx.Value(eventKey{}).(*Event)
	e.ctx = context.WithValue(ctx, eventKey{}, e)
	if fields, ok := ctx.Value(fieldsKey{}).(map[string]any); ok {
		for k, v := range fields {
			e.fields[k] = v
		}
	}
	if activity.IsActivity(ctx) {
		info := activity.GetInfo(ctx)
		e.fields["workflow_id"] = info.WorkflowExecution.ID
		e.fields["workflow_run_id"] = info.WorkflowExecution.RunID
		e.fields["activity_id"] = info.ActivityID
		e.fields["attempt"] = info.Attempt
	}
	return e
}

// Context carries this operation's SQL collector through repository calls and
// child operations. It retains the caller's cancellation and deadline.
func (e *Event) Context() context.Context { return e.ctx }

type fieldsKey struct{}

// WithFields carries diagnostic metadata to nested operations, never message bodies.
func WithFields(ctx context.Context, fields map[string]any) context.Context {
	merged := map[string]any{}
	if old, ok := ctx.Value(fieldsKey{}).(map[string]any); ok {
		for k, v := range old {
			merged[k] = v
		}
	}
	for k, v := range fields {
		merged[k] = v
	}
	return context.WithValue(ctx, fieldsKey{}, merged)
}

// Fail records an action failure even when the Activity successfully returns it to the model.
func (e *Event) Fail(err error) { e.mu.Lock(); defer e.mu.Unlock(); e.failure = err }

// Set is safe for parallel subtasks. Do not supply untrusted text or secrets.
func (e *Event) Set(key string, value any) { e.mu.Lock(); defer e.mu.Unlock(); e.fields[key] = value }

// End emits exactly once, even if both deferred and called explicitly. Error text
// can contain HTTP URLs or provider credentials, so only its concrete type is logged.
func (e *Event) End(err error) {
	e.once.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.ended = true
		e.sql.fields(e.fields)
		if e.failure != nil {
			err = e.failure
		}
		args := make([]any, 0, len(e.fields)*2+8)
		for k, v := range e.fields {
			args = append(args, k, v)
		}
		result := "success"
		level := slog.LevelInfo
		if err != nil {
			result = "error"
			level = slog.LevelError
			args = append(args, "error_type", fmt.Sprintf("%T", err))
			diagnostic := Classify(err)
			args = append(args, "error_code", diagnostic.Code, "error_reason", diagnostic.Reason, "retryable", diagnostic.Retryable)
			source := strings.SplitN(e.operation, ".", 2)[0]
			if source == "llm" {
				source = "model"
			} else if source == "tool" {
				source = "agent"
				switch name, _ := e.fields["tool"].(string); {
				case strings.HasPrefix(name, "browser_"):
					source = "browser"
				case strings.HasPrefix(name, "computer_"):
					source = "computer"
				case name == "web_read", name == "web_search":
					source = "web"
				case name == "send_message", name == "send_group_message", name == "report_progress":
					source = "telegram"
				case name == "logs_search":
					source = "logs"
				}
			}
			args = append(args, "error_source", source, "error_stage", e.operation)
			if diagnostic.Status != 0 {
				args = append(args, "http_status", diagnostic.Status)
			}
			if diagnostic.Code == "browser_capacity" {
				result, level = "waiting", slog.LevelWarn
			}
			if diagnostic.Code == "cancelled" {
				result, level = "cancelled", slog.LevelWarn
			}
		}
		elapsed := time.Since(e.start)
		durationMS := float64(elapsed.Microseconds()) / 1000
		if ms, ok := e.fields["processing_ms"].(float64); ok && e.operation == "turn.complete" {
			durationMS = ms
			elapsed = time.Duration(ms * float64(time.Millisecond))
		}
		args = append(args, "operation", e.operation, "result", result)
		// Historical turns may not have a start time. Do not report the time to
		// construct this summary as the duration of processing the user's request.
		if _, known := e.fields["processing_ms"]; e.operation != "turn.complete" || known {
			args = append(args, "duration_ms", durationMS)
		}
		e.logger.Log(e.ctx, level, readableMessage(e.operation, result, elapsed, e.fields, Classify(err)), args...)
	})
}
