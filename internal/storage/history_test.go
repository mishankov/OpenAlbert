package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"openalbert/internal/domain"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConversationHistoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { historyContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL")
		}
		repo, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		historyContract(t, repo)
	})
}
func historyContract(t *testing.T, repo domain.Repository) {
	ctx := context.Background()
	nonce := fmt.Sprintf("history-%d", time.Now().UnixNano())
	group := domain.Conversation{ID: nonce + "-group", ChatID: -12, Title: "Команда"}
	other := domain.Conversation{ID: nonce + "-other", Private: true, ChatID: 7}
	for _, conv := range []domain.Conversation{group, other} {
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: conv.ID, Conversation: conv, Text: "seed"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range []string{"old", "active", "private"} {
		scope := group.ID
		if session == "private" {
			scope = other.ID
		}
		if err := repo.StartSession(ctx, domain.Session{ID: nonce + session, ConversationID: scope}); err != nil {
			t.Fatal(err)
		}
	}
	args := json.RawMessage(`{"text":"Ответ команды: решение 100%_ готово"}`)
	cases := []struct {
		session string
		msg     domain.Message
	}{
		{"old", domain.Message{Role: "user", Content: "Старый вопрос"}},
		{"active", domain.Message{AuthorID: 7, Role: "user", Content: "Новый вопрос"}},
		{"active", domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "reply", Name: "send_message", Arguments: args}}}},
		{"active", domain.Message{Role: "tool", Content: "INTERNAL TOOL RESULT"}},
		{"active", domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "search", Name: "memory_search", Arguments: json.RawMessage(`{"query":"secret"}`)}}}},
		{"private", domain.Message{Role: "user", Content: "PRIVATE SECRET"}},
	}
	for i, c := range cases {
		for retry := 0; retry < 2; retry++ {
			if err := repo.AppendMessage(ctx, nonce+c.session, fmt.Sprint(i), c.msg); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := repo.EndSession(ctx, nonce+"old", "summary"); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ConversationHistory(ctx, group.ID, "", 0, 2)
	if err != nil || len(rows) != 2 || len(rows[0].ToolCalls) != 1 || rows[0].ToolCalls[0].Name != "send_message" || rows[0].ToolCalls[0].ID != "reply" || !strings.Contains(rows[0].Content, "Ответ команды") {
		t.Fatalf("first page: %v %v", rows, err)
	}
	if rows[1].AuthorID != 7 {
		t.Fatal("message author lost")
	}
	for _, row := range rows {
		if row.ConversationID != group.ID || row.SessionStartedAt.IsZero() {
			t.Fatal("source lost")
		}
	}
	older, err := repo.ConversationHistory(ctx, group.ID, "", rows[1].Seq, 20)
	if err != nil || len(older) != 1 || older[0].Content != "Старый вопрос" {
		t.Fatalf("cursor: %v %v", older, err)
	}
	for _, q := range []string{"решение", "100%_"} {
		hits, err := repo.ConversationHistory(ctx, group.ID, q, 0, 20)
		if err != nil || len(hits) != 1 {
			t.Fatalf("literal Unicode search %s: %v %v", q, hits, err)
		}
	}
	hits, err := repo.ConversationHistory(ctx, group.ID, "PRIVATE SECRET", 0, 20)
	if err != nil || len(hits) != 0 {
		t.Fatal("private history leaked into group query")
	}
	listed, err := repo.ListConversations(ctx)
	found := false
	for _, conv := range listed {
		if conv.ID == group.ID && conv.Title == group.Title {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatal("conversation catalog missing title")
	}
	historyCallsContract(t, repo, group.ID, nonce+"active")
}

func historyCallsContract(t *testing.T, repo domain.Repository, scope, session string) {
	t.Helper()
	ctx := context.Background()
	cases := []struct {
		key     string
		message domain.Message
	}{
		{"draft", domain.Message{Role: "assistant", Content: "Identical visible text", ToolCalls: []domain.ToolCall{{ID: "find", Name: "task_find", Arguments: json.RawMessage(`{}`)}}}},
		{"reply", domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "search", Name: "memory_search", Arguments: json.RawMessage(`{"query":"other tool"}`)}, {ID: "actual-send", Name: "send_message", Arguments: json.RawMessage(`{"text":"Identical visible text"}`)}}}},
		{"artifact", domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "send-file", Name: "send_message", Arguments: json.RawMessage(`{"text":"","artifact_id":"file"}`)}}}},
	}
	for _, tc := range cases {
		if err := repo.AppendMessage(ctx, session, "call-contract-"+tc.key, tc.message); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repo.ConversationHistory(ctx, scope, "Identical visible text", 0, 10)
	if err != nil || len(rows) != 2 || len(rows[0].ToolCalls) != 1 || rows[0].ToolCalls[0].Name != "send_message" || rows[0].ToolCalls[0].ID != "actual-send" || len(rows[1].ToolCalls) != 0 || rows[0].Content != rows[1].Content {
		t.Fatalf("actual reply calls not distinguished from internal text: %+v %v", rows, err)
	}
	encoded, _ := json.Marshal(rows)
	var serialized []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &serialized); err != nil {
		t.Fatal(err)
	}
	for _, row := range serialized {
		if _, exists := row["reply_requested"]; exists {
			t.Fatal("obsolete flag still emitted")
		}
	}
	// Returned metadata must not alias mutable in-memory storage.
	rows[0].ToolCalls[0].Name = "mutated"
	rows, err = repo.ConversationHistory(ctx, scope, "", 0, 1)
	if err != nil || len(rows) != 1 || rows[0].Content != "" || len(rows[0].ToolCalls) != 1 || rows[0].ToolCalls[0].ID != "send-file" {
		t.Fatalf("artifact-only send omitted from history: %+v %v", rows, err)
	}
	rows, err = repo.ConversationHistory(ctx, scope, "Identical visible text", 0, 10)
	if err != nil || len(rows) != 2 || len(rows[0].ToolCalls) != 1 || rows[0].ToolCalls[0].Name != "send_message" {
		t.Fatal("caller mutated stored call metadata")
	}
}

func TestGroupOutgoingHistoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { groupOutgoingContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL")
		}
		repo, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		groupOutgoingContract(t, repo)
	})
}
func groupOutgoingContract(t *testing.T, repo domain.Repository) {
	ctx := context.Background()
	nonce := fmt.Sprintf("group-post-%d", time.Now().UnixNano())
	out := domain.Outgoing{ID: nonce, Conversation: domain.Conversation{ID: nonce + "-group", ChatID: -20, ThreadID: 3, OwnerID: 7}, Text: "Only the public text", RecordInHistory: true, SessionID: nonce + "-private"}
	for i := 0; i < 2; i++ {
		if err := repo.EnqueueOutgoing(ctx, out); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repo.ConversationHistory(ctx, out.Conversation.ID, "", 0, 20)
	if err != nil || len(rows) != 1 || rows[0].Content != out.Text || len(rows[0].ToolCalls) != 1 || rows[0].ToolCalls[0].Name != "send_message" || rows[0].ToolCalls[0].ID != out.ID {
		t.Fatalf("wrong group history: %v %v", rows, err)
	}
	messages, err := repo.Messages(ctx, rows[0].SessionID)
	if err != nil || len(messages) != 1 {
		t.Fatal("missing stored message")
	}
	encoded, _ := json.Marshal(messages)
	if strings.Contains(string(encoded), out.SessionID) {
		t.Fatal("private session reference copied into group context")
	}
	pending, err := repo.PendingAnalysis(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range pending {
		if session.ID == rows[0].SessionID {
			t.Fatal("group post needs no analysis")
		}
	}
	outgoing, err := repo.PendingOutgoing(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range outgoing {
		if item.ID == out.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate outbox records")
	}
}
