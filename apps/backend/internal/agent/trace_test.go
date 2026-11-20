package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

type tracedRepo struct {
	domain.Repository
	traces []domain.Trace
	fail   bool
}

func (r *tracedRepo) SaveTrace(_ context.Context, tr domain.Trace) error {
	r.traces = append(r.traces, tr)
	if r.fail {
		return errors.New("inspection unavailable")
	}
	return nil
}

type namedTestModel string

func (m namedTestModel) Complete(context.Context, []domain.Message, []domain.Tool) (domain.Completion, error) {
	return domain.Completion{Model: string(m), Message: domain.Message{Role: "assistant", Content: "reply"}}, nil
}

func TestModelAttributionInLogsAndTraces(t *testing.T) {
	ctx := context.Background()
	a, repo, _, conv := fixture(t)
	capture := &tracedRepo{Repository: repo}
	a.Repo = capture
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)
	in := ModelInput{Turn: Turn{Conversation: conv, Input: RunInput{Key: "trace-test", SessionID: "session", ConversationID: conv.ID}}}

	a.Model = namedTestModel("gpt-6.1-terra")
	if _, err := a.ModelCall(ctx, in); err != nil {
		t.Fatal(err)
	}
	a.Model = namedTestModel("another-chatgpt-model")
	if _, err := a.ModelCall(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := a.analysisModelCall(ctx, domain.Session{ID: "session", ConversationID: conv.ID}, nil); err != nil {
		t.Fatal(err)
	}

	if len(capture.traces) != 4 {
		t.Fatalf("expected two model attempts and no trace for the cache hit: %+v", capture.traces)
	}
	for i, want := range []string{"gpt-6.1-terra", "another-chatgpt-model"} {
		if tr := capture.traces[2*i+1]; tr.Status != "completed" || tr.Name != want {
			t.Errorf("trace has model %q and status %q, want model %q and completed status", tr.Name, tr.Status, want)
		}
	}
	dec := json.NewDecoder(&logs)
	for i, want := range []string{"gpt-6.1-terra", "gpt-6.1-terra", "another-chatgpt-model"} {
		var event struct {
			Operation string `json:"operation"`
			Model     string `json:"model"`
			Cached    bool   `json:"cached"`
		}
		if err := dec.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Operation != "llm.complete" || event.Model != want || event.Cached != (i == 1) {
			t.Errorf("event %d = %+v, want llm.complete with model %s and cached=%t", i, event, want, i == 1)
		}
	}
	if dec.More() {
		t.Fatal("unexpected extra completion event")
	}
}

func TestInspectionTraceCapturesContextAndResultWithoutImageBytes(t *testing.T) {
	a, repo, _, conv := fixture(t)
	rec := &tracedRepo{Repository: repo}
	a.Repo = rec
	a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
		return domain.Message{Role: "assistant", Content: "internal result", Reasoning: "provider explanation"}, nil
	}}
	in := ModelInput{Turn: Turn{Conversation: conv, Input: RunInput{Key: "trace-test", SessionID: "session", ConversationID: conv.ID}, Messages: []domain.Message{{Role: "system", Content: "system instructions"}, {Role: "user", Content: "question", Images: []string{"data:image/png;base64,PRIVATE_BINARY"}}}}}
	out, err := a.ModelCall(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.traces) != 2 || rec.traces[0].Status != "running" || rec.traces[1].Status != "completed" || rec.traces[1].EndedAt == nil {
		t.Fatalf("missing trace lifecycle: %+v", rec.traces)
	}
	raw, _ := json.Marshal(rec.traces[1])
	if strings.Contains(string(raw), "PRIVATE_BINARY") || !strings.Contains(string(raw), "provider explanation") || !strings.Contains(string(raw), "system instructions") {
		t.Fatalf("wrong snapshot: %s", raw)
	}
	if out.Message.Reasoning != "provider explanation" || in.Turn.Messages[1].Images[0] != "data:image/png;base64,PRIVATE_BINARY" {
		t.Fatal("snapshot changed actual model input or discarded reasoning")
	}
	if _, err = a.ModelCall(context.Background(), in); err != nil || len(rec.traces) != 2 {
		t.Fatal("cached completion generated a fictional new model call")
	}
	rec.fail = true
	result := tool(t, a, conv, "trace-failure", "tool", "memory_search", `{"query":"anything"}`)
	if ToolFailed(result) {
		t.Fatal("inspection failure changed tool behavior")
	}
}
