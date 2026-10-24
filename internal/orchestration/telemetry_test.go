package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/internal/domain"
	"openalbert/internal/telemetry"
)

type lockedLog struct {
	sync.Mutex
	bytes.Buffer
}

func (b *lockedLog) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedLog) snapshot() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }

func TestWorkflowSummaryCountsWorkAndPropagatesTurnToDelivery(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	var logs lockedLog
	a.Logger = telemetry.New(&logs)
	a.Model = &scriptedModel{run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 0 {
			return calls(call("read", "web_read", map[string]string{"url": "https://example.com"}), call("bad", "web_read", map[string]any{"url": []int{1}})), nil
		}
		return reply("Done"), nil
	}}
	env := newEnv(a)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, time.Millisecond)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Minute, MaxSteps: 4})
	requireWorkflowOK(t, env)
	var summary map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.snapshot()), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["operation"] == "turn.complete" {
			summary = row
		}
	}
	if summary == nil || summary["model_calls"] != float64(2) || summary["tool_calls"] != float64(3) || summary["tool_errors"] != float64(1) || summary["input_tokens"] != float64(20) || summary["output_tokens"] != float64(10) {
		t.Fatalf("wrong aggregate: %v", summary)
	}
	if summary["attempt"] != float64(1) || summary["workflow_id"] == nil || summary["trigger"] != "message" {
		t.Fatalf("missing activity context: %v", summary)
	}
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].TurnID != summary["turn_id"] || rows[0].StartedAt.IsZero() {
		t.Fatalf("delivery correlation missing: %+v %v", rows, err)
	}
}

func TestDeliveryLogIncludesTurnAndActualLatency(t *testing.T) {
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})
	var logs bytes.Buffer
	r.Logger = telemetry.New(&logs)
	start := time.Now().Add(-5 * time.Second)
	if err := repo.EnqueueOutgoing(context.Background(), domain.Outgoing{ID: "trace", Conversation: conv, Text: "Private content", TurnID: "turn-1", SessionID: "session", Trigger: "message", StartedAt: start, ReceivedAt: start.Add(-time.Second), EnqueuedAt: time.Now().Add(-time.Second), PartCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(logs.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["turn_id"] != "turn-1" || row["delivery_state"] != "sent" || row["since_received_ms"].(float64) < 6000 || row["queue_wait_ms"].(float64) < 1000 || row["first_part"] != true || row["attempt"] != float64(1) {
		t.Fatalf("wrong delivery event: %v", row)
	}
	if strings.Contains(logs.String(), "Private content") {
		t.Fatal("message body leaked")
	}
}
