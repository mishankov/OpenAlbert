package agent

import (
	"context"
	"encoding/json"
	"errors"
	"openalbert/internal/domain"
	"strings"
	"testing"
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
func TestInspectionTraceCapturesContextAndResultWithoutImageBytes(t *testing.T) {
	a, repo, _, conv := fixture(t)
	rec := &tracedRepo{Repository: repo}
	a.Repo = rec
	a.ModelName = "configured-model"
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
