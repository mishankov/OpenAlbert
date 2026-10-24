package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"openalbert/internal/domain"
	"openalbert/internal/integrations"
	"openalbert/internal/telemetry"
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
		if row["processing_ms"].(float64) < 5000 || row["model_calls"] != float64(2) || row["tool_errors"] != float64(1) || row["delivery_state"] != "queued" || !strings.Contains(row["msg"].(string), "вызовов модели: 2, инструментов: 3") {
			t.Fatalf("wrong summary: %v", row)
		}
		return
	}
	t.Fatal("summary missing")
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
