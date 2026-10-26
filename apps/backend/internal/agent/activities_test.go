package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/knowledge"
	"openalbert/apps/backend/internal/storage"
)

type testModel struct {
	mu      sync.Mutex
	calls   int
	respond func([]domain.Message) (domain.Message, error)
}

func (m *testModel) Complete(ctx context.Context, msg []domain.Message, _ []domain.Tool) (domain.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	out, err := m.respond(msg)
	return domain.Completion{Message: out, InputTokens: 10, OutputTokens: 3}, err
}

type testSearch struct{}

func (testSearch) Search(context.Context, string) ([]domain.SearchResult, error) {
	return []domain.SearchResult{{Title: "Official docs", URL: "https://example.com", Snippet: "result"}}, nil
}
func (testSearch) Read(context.Context, string) (string, error) { return "Public page", nil }

type testMessenger struct{}

func (testMessenger) Send(context.Context, domain.Conversation, string) (string, error) {
	return "1", nil
}
func (testMessenger) SendFile(context.Context, domain.Conversation, string, []byte) (string, error) {
	return "2", nil
}
func (testMessenger) Download(context.Context, string) ([]byte, error) {
	return []byte("attachment"), nil
}
func fixture(t *testing.T) (*Activities, *storage.Memory, *knowledge.Store, domain.Conversation) {
	t.Helper()
	repo := storage.NewMemory()
	k, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
	_, err = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "seed", Conversation: conv, Text: "hello"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return &Activities{Repo: repo, Knowledge: k, Search: testSearch{}, Messenger: testMessenger{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MinTaskInterval: time.Minute}, repo, k, conv
}
func tool(t *testing.T, a *Activities, conv domain.Conversation, run, call, name, args string) string {
	t.Helper()
	result, err := a.ToolCall(context.Background(), ToolInput{RunKey: run, SessionID: "session", Conversation: conv, Call: domain.ToolCall{ID: call, Name: name, Arguments: json.RawMessage(args)}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestToolRetriesPersistExactlyOneTaskMemoryAndDelivery(t *testing.T) {
	a, repo, k, conv := fixture(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		for _, v := range []struct{ call, name, args string }{{"memory", "memory_update", `{"content":"Prefers morning meetings","kind":"preference"}`}, {"task", "task_create", `{"title":"Watch releases","goal":"Notify about Go updates","schedule_seconds":300}`}, {"send", "send_message", `{"text":"Started watching."}`}} {
			if out := tool(t, a, conv, "retry-run", v.call, v.name, v.args); strings.Contains(out, `"error"`) {
				t.Fatalf("tool failed: %s", out)
			}
		}
	}
	tasks, err := repo.FindTasks(ctx, conv.ID, "")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("duplicate tasks %+v %v", tasks, err)
	}
	entries, err := k.Search(ctx, conv.ID, "morning")
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate memories %+v %v", entries, err)
	}
	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 1 {
		t.Fatalf("duplicate sends %+v %v", out, err)
	}
}
func TestMemoryToolBuildsProfileForNextSession(t *testing.T) {
	a, _, k, conv := fixture(t)
	ctx := context.Background()
	// The model must be offered the same profile kind that the tool accepts.
	for _, definition := range Definitions() {
		if definition.Name == "memory_update" && !strings.Contains(string(definition.Parameters), `"profile"`) {
			t.Fatal("model cannot select profile memory")
		}
	}
	for _, entry := range []struct{ id, kind, content string }{
		{"name", "profile", "Пользователя зовут Денис."},
		{"city", "profile", "Живёт в Москве, часовой пояс UTC+3."},
		{"style", "preference", "Предпочитает короткие ответы."},
		{"project", "fact", "Для проекта выбрали Temporal."},
		{"reminder", "episode", "Попросил анекдот 20 октября в 07:10."},
		{"guess", "hypothesis", "Возможно, любит утренние анекдоты."},
	} {
		args, _ := json.Marshal(map[string]string{"id": entry.id, "kind": entry.kind, "content": entry.content})
		if out := tool(t, a, conv, "profile", entry.id, "memory_update", string(args)); strings.Contains(out, `"error"`) {
			t.Fatal(out)
		}
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "next-session", SessionID: "next-session", ConversationID: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	system := turn.Messages[0].Content
	for _, want := range []string{"Пользователя зовут Денис.", "Живёт в Москве", "Предпочитает короткие ответы."} {
		if !strings.Contains(system, want) {
			t.Fatalf("profile missing in new session: %s", want)
		}
	}
	for _, unwanted := range []string{"Для проекта выбрали Temporal.", "Попросил анекдот", "Возможно, любит"} {
		if strings.Contains(system, unwanted) {
			t.Fatalf("non-profile memory injected in new session: %s", unwanted)
		}
	}
	profile, err := k.Profile(ctx, "group:other")
	if err != nil || profile != "" {
		t.Fatalf("personal profile leaked to group: %q %v", profile, err)
	}
	if out := tool(t, a, conv, "correction", "city", "memory_update", `{"id":"city","kind":"profile","content":"Теперь живёт в Казани, UTC+3."}`); strings.Contains(out, `"error"`) {
		t.Fatal(out)
	}
	if out := tool(t, a, conv, "forget", "name", "memory_update", `{"id":"name","delete":true}`); strings.Contains(out, `"error"`) {
		t.Fatal(out)
	}
	profile, err = k.Profile(ctx, conv.ID)
	if err != nil || strings.Contains(profile, "Москве") || strings.Contains(profile, "Денис") || !strings.Contains(profile, "Казани") {
		t.Fatalf("profile correction/deletion not applied: %q %v", profile, err)
	}
}
func TestToolScopeAndUnsupportedCapabilities(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	_, err := repo.CreateTask(ctx, domain.Task{ID: "other-task", ConversationID: "other", Title: "Secret", Goal: "Secret goal", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.SaveArtifact(ctx, domain.Artifact{ID: "other-art", ConversationID: "other", Name: "secret.txt", MIME: "text/plain", Data: []byte("secret")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, args string }{{"task_read", `{"id":"other-task"}`}, {"artifact_read", `{"id":"other-art"}`}, {"send_message", `{"text":"leak","artifact_id":"other-art"}`}, {"computer_exec", `{"command":"pwd"}`}, {"soul_update", `{"content":"changed"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			out := tool(t, a, conv, "scope", tc.name, tc.name, tc.args)
			if !strings.Contains(out, `"error"`) || strings.Contains(out, "Secret goal") {
				t.Fatalf("scope escaped: %s", out)
			}
		})
	}
	out, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(out) != 0 {
		t.Fatalf("unauthorized delivery %+v %v", out, err)
	}
}
func TestInvalidToolRequestsCannotCreateWork(t *testing.T) {
	a, repo, _, conv := fixture(t)
	for _, tc := range []struct{ id, name, args string }{{"badjson", "task_create", `{"title":`}, {"missing", "task_create", `{"title":"missing goal"}`}, {"fast", "task_create", `{"title":"x","goal":"y","schedule_seconds":1}`}, {"negative", "task_create", `{"title":"x","goal":"y","schedule_seconds":-1}`}, {"date", "task_create", `{"title":"x","goal":"y","next_run_at":"tomorrow"}`}} {
		if out := tool(t, a, conv, "invalid", tc.id, tc.name, tc.args); !strings.Contains(out, `"error"`) {
			t.Errorf("invalid request succeeded: %s", out)
		}
	}
	tasks, _ := repo.FindTasks(context.Background(), conv.ID, "")
	if len(tasks) != 0 {
		t.Fatalf("invalid work persisted %+v", tasks)
	}
	out, err := a.ToolCall(context.Background(), ToolInput{RunKey: "nested", TaskID: "parent", Conversation: conv, Call: domain.ToolCall{ID: "n", Name: "task_create", Arguments: json.RawMessage(`{"title":"nested","goal":"work"}`)}})
	if err != nil || !strings.Contains(out, "cannot create nested tasks") {
		t.Fatalf("nested work accepted: %s %v", out, err)
	}
}
func TestPrepareDropsInterruptedToolExchangeAndKeepsValidTurns(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	if err := repo.StartSession(ctx, domain.Session{ID: "session", ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	messages := []domain.Message{{Role: "user", Content: "first"}, {Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "good", Name: "web_search", Arguments: json.RawMessage(`{}`)}}}, {Role: "tool", ToolCallID: "good", Content: "result"}, {Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "done", Name: "web_read", Arguments: json.RawMessage(`{}`)}, {ID: "interrupted", Name: "web_read", Arguments: json.RawMessage(`{}`)}}}, {Role: "tool", ToolCallID: "done", Content: "partial"}, {Role: "user", Content: "continue"}}
	for i, msg := range messages {
		if err := repo.AppendMessage(ctx, "session", ID("history", string(rune(i))), msg); err != nil {
			t.Fatal(err)
		}
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "recovered", SessionID: "session", ConversationID: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(turn.Messages)
	if strings.Contains(string(raw), "interrupted") || strings.Contains(string(raw), "partial") {
		t.Fatalf("poisoned history replayed: %s", raw)
	}
	if !strings.Contains(string(raw), `"tool_call_id":"good"`) || !strings.Contains(string(raw), "continue") {
		t.Fatalf("valid history lost: %s", raw)
	}
}
func TestModelCheckpointAvoidsPaidRetry(t *testing.T) {
	a, _, _, conv := fixture(t)
	m := &testModel{respond: func([]domain.Message) (domain.Message, error) {
		return domain.Message{Role: "assistant", Content: "answer"}, nil
	}}
	a.Model = m
	in := ModelInput{Turn: Turn{Input: RunInput{Key: "paid", SessionID: "session", ConversationID: conv.ID}, Messages: []domain.Message{{Role: "user", Content: "question"}}}, Step: 0}
	for i := 0; i < 2; i++ {
		if _, err := a.ModelCall(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if m.calls != 1 {
		t.Fatalf("paid request retried%d times", m.calls)
	}
}
func TestPostSessionAnalysisRetainsMissedMemoryIdempotently(t *testing.T) {
	a, repo, k, conv := fixture(t)
	ctx := context.Background()
	s := domain.Session{ID: "closed-session", ConversationID: conv.ID}
	if err := repo.StartSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendMessage(ctx, s.ID, "user", domain.Message{Role: "user", Content: "I prefer morning meetings"}); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	pending, _ := repo.PendingAnalysis(ctx, 10)
	if len(pending) != 1 {
		t.Fatal("closed session not scheduled for analysis")
	}
	model := &testModel{respond: func(msg []domain.Message) (domain.Message, error) {
		return domain.Message{Role: "assistant", Content: `{"summary":"Discussed meeting preference","memories":[{"content":"Prefers morning meetings","kind":"preference"}]}`}, nil
	}}
	a.Model = model
	for i := 0; i < 2; i++ {
		if err := a.Analyze(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ = repo.PendingAnalysis(ctx, 10)
	if len(pending) != 0 {
		t.Fatal("analysis left pending")
	}
	memories, err := k.Search(ctx, conv.ID, "morning")
	if err != nil || len(memories) != 1 || memories[0].Source != s.ID {
		t.Fatalf("missed or duplicated memory %+v %v", memories, err)
	}
	if model.calls != 1 {
		t.Fatalf("analysis called model%d times", model.calls)
	}
}
func TestOutgoingEmojiRichMessagePreservesOriginal(t *testing.T) {
	a, repo, _, conv := fixture(t)
	args, _ := json.Marshal(map[string]string{"text": strings.Repeat("😀", 4000)})
	out := tool(t, a, conv, "unicode", "send", "send_message", string(args))
	if strings.Contains(out, `"error"`) {
		t.Fatal(out)
	}
	messages, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var combined strings.Builder
	for _, msg := range messages {
		if utf8.RuneCountInString(msg.Text) > 32768 || msg.Formatted || len(msg.Entities) != 0 {
			t.Fatal("outbox rich content was transformed or exceeds Telegram limit")
		}
		combined.WriteString(msg.Text)
	}
	if combined.String() != strings.Repeat("😀", 4000) {
		t.Fatal("message split lost text")
	}
}
