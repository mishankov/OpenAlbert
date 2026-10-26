package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

// The contract suite also runs against a real Postgres database when configured.
// Use a disposable database; test records are namespaced by a unique run ID.
func TestRepositoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { repositoryContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL to test PostgreSQL persistence")
		}
		s, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		repositoryContract(t, s)
	})
}
func repositoryContract(t *testing.T, r domain.Repository) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	scope := nonce + ":private"
	other := nonce + ":group"
	conv := domain.Conversation{ID: scope, ChatID: 123, OwnerID: 321, Private: true}
	incoming := domain.Incoming{ID: nonce + ":in", Conversation: conv, Text: "hello", Attachments: []domain.Attachment{{Name: "image.png", Data: []byte("image bytes")}}}
	incoming.ReplyTo = &domain.ReplyContext{MessageID: 42, AuthorID: 7, AuthorName: "Альберт", AuthorIsBot: true, Text: "Исходное сообщение", MediaKinds: []string{"image"}}
	incoming.Quote = &domain.MessageQuote{Text: "сообщение", Position: 9, IsManual: true}
	accepted, err := r.AcceptIncoming(ctx, incoming, 101)
	if err != nil || !accepted {
		t.Fatalf("accept: %v %v", accepted, err)
	}
	accepted, err = r.AcceptIncoming(ctx, incoming, 103)
	if err != nil || accepted {
		t.Fatalf("duplicate accept: %v %v", accepted, err)
	}
	offset, err := r.PollOffset(ctx)
	if err != nil || offset < 103 {
		t.Fatalf("offset %d %v", offset, err)
	}
	_, err = r.AcceptIncoming(ctx, incoming, 99)
	if err != nil {
		t.Fatal(err)
	}
	offset2, _ := r.PollOffset(ctx)
	if offset2 != offset {
		t.Fatal("poll offset moved backwards")
	}
	got, ok, err := r.Incoming(ctx, incoming.ID)
	if err != nil || !ok || got.Text != "hello" || len(got.Attachments) != 1 || string(got.Attachments[0].Data) != "image bytes" {
		t.Fatalf("incoming %#v %v %v", got, ok, err)
	}
	if !reflect.DeepEqual(got.ReplyTo, incoming.ReplyTo) || !reflect.DeepEqual(got.Quote, incoming.Quote) {
		t.Fatalf("durable inbox lost reply/quote: reply=%+v quote=%+v", got.ReplyTo, got.Quote)
	}
	pending, err := r.PendingIncoming(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, in := range pending {
		if in.ID == incoming.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate durable incoming: %d", count)
	}
	if err = r.MarkIncomingHandled(ctx, incoming.ID); err != nil {
		t.Fatal(err)
	}
	pending, _ = r.PendingIncoming(ctx, 1000)
	for _, in := range pending {
		if in.ID == incoming.ID {
			t.Fatal("handled message pending")
		}
	}
	c, ok, err := r.Conversation(ctx, scope)
	if err != nil || !ok || c != conv {
		t.Fatalf("conversation %#v %v %v", c, ok, err)
	}
	private, err := r.ListPrivateConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range private {
		found = found || c.ID == scope
	}
	if !found {
		t.Fatal("private conversation missing")
	}
	session := domain.Session{ID: nonce + ":session", ConversationID: scope}
	if err = r.StartSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	activeSessions, err := r.ActiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundActive := false
	for _, active := range activeSessions {
		foundActive = foundActive || active.ID == session.ID
	}
	if !foundActive {
		t.Fatal("browser reconciliation cannot find active session")
	}
	if err = r.StartSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	msg := domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "call", Name: "web_search", Arguments: json.RawMessage(`{"query":"test"}`)}}}
	if err = r.AppendMessage(ctx, session.ID, "turn-1", msg); err != nil {
		t.Fatal(err)
	}
	msg.ToolCalls[0].Name = "mutated"
	if err = r.AppendMessage(ctx, session.ID, "turn-1", msg); err != nil {
		t.Fatal(err)
	}
	messages, err := r.Messages(ctx, session.ID)
	if err != nil || len(messages) != 1 || messages[0].ToolCalls[0].Name != "web_search" {
		t.Fatalf("messages %#v %v", messages, err)
	}
	if err = r.EndSession(ctx, session.ID, "summary"); err != nil {
		t.Fatal(err)
	}
	if err = r.EndSession(ctx, session.ID, "retry overwrite"); err != nil {
		t.Fatal(err)
	}
	activeSessions, err = r.ActiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, active := range activeSessions {
		if active.ID == session.ID {
			t.Fatal("closed session retained in browser reconciliation")
		}
	}
	analyses, err := r.PendingAnalysis(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, s := range analyses {
		if s.ID == session.ID {
			found = true
			if s.Summary != "summary" {
				t.Fatal("session close retry rewrote summary")
			}
		}
	}
	if !found {
		t.Fatal("mandatory analysis missing")
	}
	if err = r.MarkAnalyzed(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	analyses, _ = r.PendingAnalysis(ctx, 1000)
	for _, s := range analyses {
		if s.ID == session.ID {
			t.Fatal("analysis remains pending")
		}
	}
	task := domain.Task{ID: "shared-task", ConversationID: scope, Title: "Check Tickets", Goal: "Find morning trains"}
	created, err := r.CreateTask(ctx, task)
	if err != nil || created.Status != "queued" {
		t.Fatalf("create task %#v %v", created, err)
	}
	task.Title = "retry replacement"
	retry, err := r.CreateTask(ctx, task)
	if err != nil || retry.Title != created.Title {
		t.Fatal("create retry replaced task")
	}
	task.ConversationID = other
	if _, err = r.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = r.Task(ctx, "unrelated", task.ID); err != nil || ok {
		t.Fatal("task leaked across scopes")
	}
	tasks, err := r.FindTasks(ctx, scope, "TICKETS")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("task search %#v %v", tasks, err)
	}
	created.Status = "paused"
	updated, err := r.UpdateTask(ctx, created)
	if err != nil || updated.CreatedAt != created.CreatedAt {
		t.Fatalf("update %#v %v", updated, err)
	}
	if updated.Version != created.Version+1 {
		t.Fatal("task version did not advance")
	}
	created.Status = "running"
	if _, err := r.UpdateTask(ctx, created); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale task update overwrote pause: %v", err)
	}
	current, _, _ := r.Task(ctx, scope, created.ID)
	if current.Status != "paused" {
		t.Fatal("stale update resumed paused task")
	}
	runs, err := r.RunnableTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ConversationID == scope && run.ID == created.ID {
			t.Fatal("paused task restored as runnable")
		}
	}
	art := domain.Artifact{ID: "same-name", ConversationID: scope, Name: "notes.txt", MIME: "text/plain", Data: []byte("original")}
	if err = r.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	art.Data[0] = 'X'
	if err = r.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	artifact, ok, err := r.Artifact(ctx, scope, art.ID)
	if err != nil || !ok || string(artifact.Data) != "original" {
		t.Fatalf("artifact %#v %v %v", artifact, ok, err)
	}
	artifact.Data[0] = 'Z'
	artifact, _, _ = r.Artifact(ctx, scope, art.ID)
	if string(artifact.Data) != "original" {
		t.Fatal("artifact alias mutated state")
	}
	if _, ok, err = r.Artifact(ctx, other, art.ID); err != nil || ok {
		t.Fatal("artifact leaked across scopes")
	}
	out := domain.Outgoing{ID: nonce + ":out", Conversation: conv, Text: "done", Formatted: true, Entities: []domain.TextEntity{{Type: "bold", Length: 4}}}
	if err = r.EnqueueOutgoing(ctx, out); err != nil {
		t.Fatal(err)
	}
	out.Entities[0].Type = "italic"
	pendingFormatted, err := r.PendingOutgoing(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	foundFormatted := false
	for _, message := range pendingFormatted {
		if message.ID == out.ID {
			foundFormatted = true
			if !message.Formatted || len(message.Entities) != 1 || message.Entities[0].Type != "bold" {
				t.Fatalf("outbox lost formatting: %+v", message)
			}
			message.Entities[0].Type = "code"
		}
	}
	if !foundFormatted {
		t.Fatal("formatted outbox message missing")
	}
	pendingFormatted, err = r.PendingOutgoing(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range pendingFormatted {
		if message.ID == out.ID && message.Entities[0].Type != "bold" {
			t.Fatal("caller mutated stored formatting")
		}
	}
	if err = r.EnqueueOutgoing(ctx, out); err != nil {
		t.Fatal(err)
	}
	if err = r.SetOutgoingState(ctx, out.ID, "sent"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("accepted skipped send state: %v", err)
	}
	if err = r.SetOutgoingState(ctx, out.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = r.SetOutgoingState(ctx, out.ID, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = r.SetOutgoingState(ctx, out.ID, "unknown"); err != nil {
		t.Fatal(err)
	}
	if err = r.SetOutgoingState(ctx, out.ID, "sending"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("unknown delivery was retried")
	}
	outs, _ := r.PendingOutgoing(ctx, 1000)
	for _, o := range outs {
		if o.ID == out.ID {
			t.Fatal("unknown outbox item pending")
		}
	}
	key := nonce + ":receipt"
	value := json.RawMessage(`{"completed":true}`)
	if err = r.SaveCheckpoint(ctx, key, value); err != nil {
		t.Fatal(err)
	}
	value[2] = 'X'
	raw, ok, err := r.LoadCheckpoint(ctx, key)
	if err != nil || !ok || !json.Valid(raw) {
		t.Fatalf("checkpoint %s %v %v", raw, ok, err)
	}
}
func TestMemoryConcurrentInbox(t *testing.T) {
	r := NewMemory()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.AcceptIncoming(ctx, domain.Incoming{ID: fmt.Sprint(i % 20), Conversation: domain.Conversation{ID: "private"}}, int64(i))
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	pending, err := r.PendingIncoming(ctx, 100)
	if err != nil || len(pending) != 20 {
		t.Fatalf("pending %d %v", len(pending), err)
	}
	offset, _ := r.PollOffset(ctx)
	if offset != 99 {
		t.Fatalf("offset %d", offset)
	}
}

func TestPostgresRestartDurability(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("restart-%d", time.Now().UnixNano())
	conv := domain.Conversation{ID: id, Private: true}
	in := domain.Incoming{ID: id + ":in", Conversation: conv, Text: "durable"}
	if _, err = s.AcceptIncoming(ctx, in, 999); err != nil {
		t.Fatal(err)
	}
	if err = s.StartSession(ctx, domain.Session{ID: id, ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendMessage(ctx, id, "m1", domain.Message{Role: "user", Content: "remember"}); err != nil {
		t.Fatal(err)
	}
	if err = s.EndSession(ctx, id, "analyze this"); err != nil {
		t.Fatal(err)
	}
	if err = s.EnqueueOutgoing(ctx, domain.Outgoing{ID: id, Conversation: conv, Text: "may have arrived"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetOutgoingState(ctx, id, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetOutgoingState(ctx, id, "sending"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveCheckpoint(ctx, id, json.RawMessage(`{"step":3}`)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, ok, err := s.Incoming(ctx, in.ID); err != nil || !ok || got.Text != "durable" {
		t.Fatalf("lost inbox: %#v %v %v", got, ok, err)
	}
	if messages, err := s.Messages(ctx, id); err != nil || len(messages) != 1 {
		t.Fatalf("lost messages %v %v", messages, err)
	}
	if raw, ok, err := s.LoadCheckpoint(ctx, id); err != nil || !ok || !strings.Contains(string(raw), "3") {
		t.Fatalf("lost checkpoint %s %v %v", raw, ok, err)
	}
	var state string
	var attempts int
	var jsonState string
	if err = s.pool.QueryRow(ctx, "SELECT state,attempts,data->>'state' FROM openalbert_outgoing WHERE id=$1", id).Scan(&state, &attempts, &jsonState); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" || jsonState != "unknown" || attempts != 1 {
		t.Fatalf("unsafe recovery state=%s JSON=%s attempts=%d", state, jsonState, attempts)
	}
	analyses, err := s.PendingAnalysis(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, session := range analyses {
		found = found || session.ID == id
	}
	if !found {
		t.Fatal("lost post-session analysis")
	}
}
func TestPostgresConcurrentInbox(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prefix := fmt.Sprintf("parallel-%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AcceptIncoming(ctx, domain.Incoming{ID: fmt.Sprintf("%s-%d", prefix, i%10), Conversation: domain.Conversation{ID: prefix}}, int64(1000+i))
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	var count int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM openalbert_incoming WHERE id LIKE $1", prefix+"%").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 10 {
		t.Fatalf("concurrent duplicate count %d", count)
	}
	offset, err := s.PollOffset(ctx)
	if err != nil || offset < 1029 {
		t.Fatalf("lost offset %d %v", offset, err)
	}
}

func TestOffsetAdvancesWithoutPersistingIgnoredMessages(t *testing.T) {
	r := NewMemory()
	ctx := context.Background()
	if err := r.SetPollOffset(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPollOffset(ctx, 12); err != nil {
		t.Fatal(err)
	}
	if offset, _ := r.PollOffset(ctx); offset != 42 {
		t.Fatalf("offset %d", offset)
	}
	pending, _ := r.PendingIncoming(ctx, 10)
	if len(pending) != 0 {
		t.Fatal("ignored Telegram update created an inbox item")
	}
}
func TestConcurrentTaskUpdateDetectsConflict(t *testing.T) {
	r := NewMemory()
	ctx := context.Background()
	task, err := r.CreateTask(ctx, domain.Task{ID: "one", ConversationID: "private", Title: "one"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, status := range []string{"cancelled", "running"} {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			copy := task
			copy.Status = status
			_, err := r.UpdateTask(ctx, copy)
			results <- err
		}(status)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflicts)
	}
}

func TestPostgresTaskCancellationWinsOverStaleWorker(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	scope := fmt.Sprintf("cas-%d", time.Now().UnixNano())
	task, err := s.CreateTask(ctx, domain.Task{ID: "task", ConversationID: scope, Title: "watch", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	workerSnapshot := task
	task.Status = "cancelled"
	cancelled, err := s.UpdateTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	workerSnapshot.Status = "waiting"
	if _, err = s.UpdateTask(ctx, workerSnapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker overwrote cancellation: %v", err)
	}
	actual, _, _ := s.Task(ctx, scope, "task")
	if actual.Status != "cancelled" || actual.Version != cancelled.Version {
		t.Fatalf("cancellation lost %#v", actual)
	}
	// Two writers with the same fresh version cannot both commit.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, status := range []string{"paused", "running"} {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			copy := actual
			copy.Status = status
			_, err := s.UpdateTask(ctx, copy)
			results <- err
		}(status)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflicts)
	}
}
func TestPostgresSafeOutboxRetryAndSummary(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	safeOutboxRetryAndSummary(t, s)
}
func TestMemorySafeOutboxRetryAndSummary(t *testing.T) { safeOutboxRetryAndSummary(t, NewMemory()) }
func safeOutboxRetryAndSummary(t *testing.T, r domain.Repository) {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("retry-%d", time.Now().UnixNano())
	if err := r.EnqueueOutgoing(ctx, domain.Outgoing{ID: id, Conversation: domain.Conversation{ID: id}, Text: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"sending", "pending", "sending", "failed"} {
		if err := r.SetOutgoingState(ctx, id, state); err != nil {
			t.Fatalf("transition %s: %v", state, err)
		}
	}
	if err := r.SetOutgoingState(ctx, id, "sending"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("failed send resumed")
	}
	if err := r.StartSession(ctx, domain.Session{ID: id, ConversationID: id}); err != nil {
		t.Fatal(err)
	}
	if err := r.EndSession(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.EndSession(ctx, id, "analysis summary"); err != nil {
		t.Fatal(err)
	}
	pending, err := r.PendingAnalysis(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, session := range pending {
		if session.ID == id {
			found = true
			if session.Summary != "analysis summary" {
				t.Fatal("post-closing summary lost")
			}
		}
	}
	if !found {
		t.Fatal("analysis missing")
	}
}
