package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/inspector"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestInspectionLegacyOrderAndArtifactScopePostgres(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL to a disposable database")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	id := fmt.Sprintf("inspection-scope-%d", time.Now().UnixNano())
	for _, session := range []string{id, id + "-other"} {
		if err = repo.StartSession(ctx, domain.Session{ID: session, ConversationID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate turn references must not duplicate historical traces. Similar
	// run-key prefixes in another session must not expose its model checkpoints.
	for _, turn := range []struct{ key, session, run string }{
		{id, id, id}, {id + "-duplicate", id, id}, {id + "-other", id + "-other", id + "-other"},
	} {
		raw, _ := json.Marshal(map[string]any{"Input": map[string]string{"SessionID": turn.session, "Key": turn.run}})
		if err = repo.SaveCheckpoint(ctx, "turn-done/"+turn.key, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{id, id + "-other"} {
		for _, step := range []int{2, 10, 11, 12} {
			if err = repo.SaveCheckpoint(ctx, fmt.Sprintf("model/%s/%d", run, step), json.RawMessage(`{"Message":{"role":"assistant","content":"historical"}}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, step := range []int{10, 2} {
		if err = repo.AppendMessage(ctx, id, fmt.Sprintf("%s/assistant/%d", id, step), domain.Message{Role: "assistant", Content: id + "-referenced"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []int{2, 10} {
		status := "running"
		if step == 2 {
			status = "completed"
		}
		if err = repo.SaveTrace(ctx, domain.Trace{ID: fmt.Sprintf("%s-trace-%d", id, step), SessionID: id, TurnID: id, Step: step, Kind: "model", Status: status, StartedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, artifact := range []struct{ name, conversation, session string }{
		{"direct", id, id}, {"referenced", id, id + "-other"},
		{"unrelated", id, id + "-other"}, {"foreign", id + "-other", id},
	} {
		if err = repo.SaveArtifact(ctx, domain.Artifact{ID: id + "-" + artifact.name, SessionID: artifact.session, ConversationID: artifact.conversation, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	d, ok, err := repo.InspectSession(ctx, id, 0)
	if err != nil || !ok || len(d.Messages) != 2 {
		t.Fatalf("detail: found=%v err=%v", ok, err)
	}
	var historical []string
	for _, trace := range d.Traces {
		if trace.Historical {
			historical = append(historical, trace.ID)
		}
	}
	want := []string{"model/" + id + "/10", "model/" + id + "/11", "model/" + id + "/12"}
	if !reflect.DeepEqual(historical, want) || len(d.Traces) != len(want)+2 {
		t.Fatalf("historical order, scope or completed-trace deduplication: got %v, want %v; total=%d", historical, want, len(d.Traces))
	}
	// A later page must still include artifacts referenced by earlier messages.
	d, ok, err = repo.InspectSession(ctx, id, d.Messages[1].Seq)
	if err != nil || !ok || len(d.Messages) != 0 {
		t.Fatalf("later page: found=%v err=%v", ok, err)
	}
	var artifacts []string
	for _, artifact := range d.Artifacts {
		artifacts = append(artifacts, artifact.ID)
	}
	if want := []string{id + "-direct", id + "-referenced"}; !reflect.DeepEqual(artifacts, want) {
		t.Fatalf("artifact scope across pages: got %v, want %v", artifacts, want)
	}
}

func TestInspectionPostgres(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL to a disposable database")
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

func TestPendingAudioIsVisibleBeforeMessagePreparation(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("disposable PostgreSQL only")
	}
	ctx := context.Background()
	repo, e := Open(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	id := fmt.Sprintf("pending-audio-%d", time.Now().UnixNano())
	conv := domain.Conversation{ID: id, OwnerID: 42, Private: true}
	received := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	caption := "Моя подпись [Voice transcript] <script>"
	_, e = repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv, Text: caption, CreatedAt: received}, 0)
	if e != nil {
		t.Fatal(e)
	}
	if e = repo.StartSession(ctx, domain.Session{ID: id, ConversationID: id}); e != nil {
		t.Fatal(e)
	}
	if e = repo.SaveArtifact(ctx, domain.Artifact{ID: "source", SessionID: id, ConversationID: id, Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("x")}); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(domain.AudioResult{SessionID: id, ConversationID: id, IncomingID: id, ArtifactID: "source", Status: "queued", AuthorID: 42, AuthorName: "Анна", QueuedAt: time.Now()})
	if e = repo.SaveCheckpoint(ctx, "audio/"+id, raw); e != nil {
		t.Fatal(e)
	}
	d, ok, e := repo.InspectSession(ctx, id, 0)
	if e != nil || !ok || len(d.Messages) != 0 || len(d.Audio) != 1 || len(d.Artifacts) != 1 || d.Audio[0].AuthorID != 42 {
		t.Fatalf("pending audio missing %+v %v", d, e)
	}
	if d.Audio[0].OriginalText == nil || *d.Audio[0].OriginalText != caption || d.Audio[0].ReceivedAt == nil || !d.Audio[0].ReceivedAt.Equal(received) {
		t.Fatal("original caption or Telegram timestamp lost")
	}
	// Even a mismatched source ID in a diagnostic projection cannot expose the
	// caption of another personal/group conversation.
	other := domain.Conversation{ID: id + "-other", OwnerID: 43, Private: true}
	_, e = repo.AcceptIncoming(ctx, domain.Incoming{ID: other.ID, Conversation: other, Text: "PRIVATE_OTHER_SCOPE"}, 0)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ = json.Marshal(domain.AudioResult{SessionID: id, ConversationID: id, IncomingID: other.ID, ArtifactID: "other-source", Status: "failed"})
	if e = repo.SaveCheckpoint(ctx, "audio/"+other.ID, raw); e != nil {
		t.Fatal(e)
	}
	d, _, e = repo.InspectSession(ctx, id, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, audio := range d.Audio {
		if audio.ArtifactID == "other-source" && (audio.OriginalText != nil || audio.ReceivedAt != nil) {
			t.Fatal("source metadata crossed conversation scope")
		}
	}
}
