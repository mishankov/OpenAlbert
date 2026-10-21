package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"jarvis/internal/domain"
	"jarvis/internal/inspector"
	"os"
	"testing"
	"time"
)

func TestInspectionPostgres(t *testing.T) {
	url := os.Getenv("JARVIS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set JARVIS_TEST_DATABASE_URL to a disposable database")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	id := fmt.Sprintf("inspection-%d", time.Now().UnixNano())
	conv := domain.Conversation{ID: id, Title: "Личный чат", Private: true, ChatID: 7, OwnerID: 7}
	if _, err = repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv}, 0); err != nil {
		t.Fatal(err)
	}
	if err = repo.StartSession(ctx, domain.Session{ID: id, ConversationID: id}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		m := domain.Message{Role: "user", Content: fmt.Sprintf("Сообщение %d", i)}
		if i == 0 {
			m = domain.Message{Role: "assistant", Reasoning: "Provider reasoning", ToolCalls: []domain.ToolCall{{ID: "c", Name: "memory_search", Arguments: json.RawMessage(`{"query":"пончики"}`)}}}
		}
		if err = repo.AppendMessage(ctx, id, fmt.Sprint(i), m); err != nil {
			t.Fatal(err)
		}
	}
	turn := json.RawMessage(fmt.Sprintf(`{"Input":{"SessionID":%q,"Key":%q,"Initiative":true},"Stats":{"ModelCalls":1}}`, id, id))
	if err = repo.SaveCheckpoint(ctx, "turn-done/"+id, turn); err != nil {
		t.Fatal(err)
	}
	key := "model/" + id + "/0"
	if err = repo.SaveCheckpoint(ctx, key, json.RawMessage(`{"Message":{"role":"assistant","content":"Молчу"},"InputTokens":100}`)); err != nil {
		t.Fatal(err)
	}
	legacy, ok, err := repo.InspectTrace(ctx, id, key)
	if err != nil || !ok || !legacy.Historical {
		t.Fatalf("legacy trace: %+v %v", legacy, err)
	}
	if _, ok, err = repo.InspectTrace(ctx, "another-session", key); err != nil || ok {
		t.Fatal("legacy trace escaped its session")
	}
	tr := domain.Trace{ID: id, SessionID: id, TurnID: id, Trigger: "initiative", Kind: "model", Name: "test-model", Status: "running", StartedAt: time.Now().UTC(), Input: json.RawMessage(`{"messages":[{"role":"system","content":"PRIVATE CONTEXT"}]}`)}
	if err = repo.SaveTrace(ctx, tr); err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC()
	tr.EndedAt = &end
	tr.Status = "completed"
	tr.Output = json.RawMessage(`{"Message":{"content":"внутренний итог"}}`)
	if err = repo.SaveTrace(ctx, tr); err != nil {
		t.Fatal(err)
	}
	full, ok, err := repo.InspectTrace(ctx, id, id)
	if err != nil || !ok || full.Status != "completed" || len(full.Input) == 0 {
		t.Fatalf("full trace: %+v %v", full, err)
	}
	if _, ok, err = repo.InspectTrace(ctx, "another", id); err != nil || ok {
		t.Fatal("trace escaped its session")
	}
	rows, err := repo.InspectSessions(ctx, inspector.Filter{Query: "пончики", Trigger: "initiative"})
	if err != nil || len(rows) != 1 || rows[0].MessageCount != 205 || rows[0].ToolCount != 1 {
		t.Fatalf("list: %+v %v", rows, err)
	}
	if rows, err = repo.InspectSessions(ctx, inspector.Filter{Query: id, Before: rows[0].Seq}); err != nil || len(rows) != 0 {
		t.Fatalf("session cursor ignored: %+v %v", rows, err)
	}
	d, ok, err := repo.InspectSession(ctx, id, 0)
	if err != nil || !ok || len(d.Messages) != 200 || d.NextAfter == 0 || len(d.Traces) != 1 || len(d.Traces[0].Input) != 0 || len(d.Traces[0].Output) != 0 || len(d.Turns) != 1 {
		t.Fatalf("detail pagination/metadata: %+v %v", d, err)
	}
	if d.Messages[0].Message.Reasoning != "Provider reasoning" || d.Messages[0].RecordedAt == nil {
		t.Fatal("reasoning or actual recording timestamp lost")
	}
	d2, ok, err := repo.InspectSession(ctx, id, d.NextAfter)
	if err != nil || !ok || len(d2.Messages) != 5 || d2.NextAfter != 0 || d2.Messages[0].Seq <= d.Messages[199].Seq {
		t.Fatalf("message cursor: %+v %v", d2, err)
	}
	if _, ok, err = repo.InspectSession(ctx, "missing", 0); err != nil || ok {
		t.Fatal("missing session not distinguished")
	}
}
