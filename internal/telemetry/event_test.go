package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestWideEventAccumulatesAndEmitsOnce(t *testing.T) {
	var out bytes.Buffer
	e := Start(context.Background(), New(&out), "agent_turn")
	e.Set("session_id", "session-123")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); e.Set("tool_count", 2) }()
	}
	wg.Wait()
	e.End(errors.New("never log https://token:secret@example.com"))
	e.End(nil)
	if strings.Count(strings.TrimSpace(out.String()), "\n") != 0 {
		t.Fatal("expected one event")
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatal("error text leaked")
	}
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["operation"] != "agent_turn" || record["result"] != "error" || record["session_id"] != "session-123" || record["duration_ms"] == nil {
		t.Fatal("event lacks operation context")
	}
}

type statusError int

func (e statusError) Error() string {
	return "secret body https://user:secret@example.com/path?token=secret"
}
func (e statusError) HTTPStatus() int { return int(e) }

func TestActionFailureReadableAndRedacted(t *testing.T) {
	for _, code := range []int{403, 429, 503} {
		var out bytes.Buffer
		ctx := WithFields(context.Background(), map[string]any{"turn_id": "turn-7", "conversation_id": "personal:7"})
		e := Start(ctx, New(&out), "tool.execute")
		e.Set("tool", "web_read")
		e.Set("activity_result", "success")
		e.Fail(statusError(code))
		e.End(nil)
		var row map[string]any
		if err := json.Unmarshal(out.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row["result"] != "error" || row["activity_result"] != "success" || row["http_status"] != float64(code) || row["turn_id"] != "turn-7" {
			t.Fatalf("wrong outcome/context: %v", row)
		}
		if row["retryable"] != (code == 429 || code >= 500) {
			t.Fatalf("wrong retryability: %v", row)
		}
		if !strings.Contains(row["msg"].(string), "http_") || !strings.Contains(row["msg"].(string), "Ошибка") || strings.Contains(out.String(), "secret") {
			t.Fatalf("unreadable or unsafe event: %s", out.String())
		}
	}
}

func TestCancellationAndCapacityAreNotSuccessOrFatalErrors(t *testing.T) {
	for _, tc := range []struct {
		err     error
		outcome string
	}{{context.Canceled, "cancelled"}, {errors.New("all browser slots are occupied"), "waiting"}} {
		var out bytes.Buffer
		e := Start(context.Background(), New(&out), "tool.execute")
		e.End(tc.err)
		var row map[string]any
		if err := json.Unmarshal(out.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row["result"] != tc.outcome || row["level"] != "WARN" {
			t.Fatalf("bad classification: %v", row)
		}
	}
}
