package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/integrations"
	"jarvis/internal/telemetry"
)

type rejectedSearch struct{ testSearch }

func (rejectedSearch) Read(context.Context, string) (string, error) {
	return "", &integrations.HTTPError{Status: 403, Source: "web"}
}

func TestToolErrorSurvivesCacheAndKeepsActivitySuccessful(t *testing.T) {
	a, _, _, conv := fixture(t)
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)
	a.Search = rejectedSearch{}
	for range 2 {
		tool(t, a, conv, "turn", "call", "web_read", `{"url":"https://example.com/private?token=secret"}`)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "/private") {
		t.Fatalf("wrong or unsafe logs: %s", logs.String())
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["result"] != "error" || row["activity_result"] != "success" || row["error_code"] != "http_403" || row["destination_host"] != "example.com" || row["turn_id"] != "turn" {
			t.Fatalf("wrong error: %v", row)
		}
		if i == 1 && row["cached"] != true {
			t.Fatal("retry not identified as cached")
		}
	}
}

func TestQueuedRichReplyPersistsTraceAndAttachmentMetadata(t *testing.T) {
	a, repo, _, conv := fixture(t)
	start := time.Now().Add(-3 * time.Second).UTC()
	in := ToolInput{RunKey: "turn", SessionID: "session", Conversation: conv, StartedAt: start, ReceivedAt: start.Add(-time.Second), Call: domain.ToolCall{ID: "send", Name: "send_message", Arguments: json.RawMessage(`{"text":"` + strings.Repeat("x", 4500) + `","artifact_id":"document"}`)}}
	if err := repo.SaveArtifact(context.Background(), domain.Artifact{ID: "document", ConversationID: conv.ID, Name: "test.txt", MIME: "text/plain", Data: []byte("test")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ToolCall(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("parts: %v %v", rows, err)
	}
	for i, row := range rows {
		if row.TurnID != "turn" || row.SessionID != "session" || row.Trigger != "message" || row.PartCount != 2 || row.PartIndex != i || !row.StartedAt.Equal(start) || row.EnqueuedAt.IsZero() {
			t.Fatalf("trace lost: %+v", row)
		}
	}
}

func TestTurnSummaryMeasuresProcessingNotPersistence(t *testing.T) {
	a, _, _, conv := fixture(t)
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)
	turn := Turn{Input: RunInput{Key: "turn", SessionID: "session", ConversationID: conv.ID, StartedAt: time.Now().Add(-5 * time.Second)}, Sent: true, Stats: TurnStats{ModelCalls: 2, ToolCalls: 3, ToolErrors: 1, InputTokens: 100, OutputTokens: 20, Tools: []string{"web_read", "send_message"}, FinishReason: "reply_queued"}}
	if err := a.Finish(context.Background(), FinishInput{Turn: turn}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["operation"] != "turn.complete" {
			continue
		}
		if row["processing_ms"].(float64) < 5000 || row["duration_ms"] != row["processing_ms"] || row["model_calls"] != float64(2) || row["tool_errors"] != float64(1) || row["delivery_state"] != "queued" || !strings.Contains(row["msg"].(string), "вызовов модели: 2, инструментов: 3") {
			t.Fatalf("wrong summary: %v", row)
		}
		return
	}
	t.Fatal("summary missing")
}

type traceCapture struct {
	domain.Repository
	traces map[string]domain.Trace
}

func (r *traceCapture) SaveTrace(_ context.Context, tr domain.Trace) error {
	r.traces[tr.ID] = tr
	return nil
}

func TestFailureLogsLinkToTheActualPersistedModelAndToolTrace(t *testing.T) {
	a, repo, _, conv := fixture(t)
	capture := &traceCapture{Repository: repo, traces: map[string]domain.Trace{}}
	a.Repo = capture
	a.InspectorPublicURL = "https://localhost:8443/jarvis/inspect/"
	a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
		return domain.Message{}, errors.New("model response truncated by token limit")
	}}
	a.Search = rejectedSearch{}
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)
	in := ModelInput{Turn: Turn{Input: RunInput{Key: "turn", SessionID: "session", ConversationID: conv.ID}}, Step: 2}
	if _, err := a.ModelCall(context.Background(), in); err == nil {
		t.Fatal("expected a model failure")
	}
	tool(t, a, conv, "turn", "read", "web_read", `{"url":"https://example.com/private?secret=hidden"}`)
	if _, err := a.analysisModelCall(context.Background(), domain.Session{ID: "session", ConversationID: conv.ID}, []domain.Message{{Role: "user", Content: "private analysis input"}}); err == nil {
		t.Fatal("expected an analysis model failure")
	}
	if len(capture.traces) != 3 {
		t.Fatalf("expected model, tool and analysis traces: %v", capture.traces)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		id, _ := row["trace_id"].(string)
		tr, ok := capture.traces[id]
		if !ok || tr.Status != "failed" || tr.SessionID != row["session_id"] || tr.TurnID != row["turn_id"] {
			t.Fatalf("log does not identify its persisted trace: %v %+v", row, tr)
		}
		link, err := url.Parse(row["inspector_url"].(string))
		if err != nil || link.Host != "localhost:8443" || link.Path != "/jarvis/inspect/" || link.Fragment != "/session/session?trace="+id {
			t.Fatalf("invalid inspector link: %v %v", link, err)
		}
		if row["error_code"] == "operation_failed" || row["error_source"] == nil || row["error_stage"] != row["operation"] || strings.Contains(line, "hidden") {
			t.Fatalf("missing or unsafe diagnostics: %v", row)
		}
	}
}

func TestToolFailureIgnoresErrorTextInsideSuccessfulResult(t *testing.T) {
	for _, raw := range []string{`{"text":"an example with \"error\""}`, `[{"title":"error"}]`, `"page error text"`} {
		if ToolFailed(raw) {
			t.Fatalf("false error: %s", raw)
		}
	}
	if !ToolFailed(`{"error":"forbidden","error_code":"http_403"}`) {
		t.Fatal("missed error")
	}
}
