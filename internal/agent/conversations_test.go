package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"jarvis/internal/domain"
)

func TestOwnerReadsGroupsWhileGroupsAndOtherPrivateChatsStayIsolated(t *testing.T) {
	a, repo, know, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	ctx := context.Background()
	groupA := domain.Conversation{ID: "group:a", ChatID: -10, OwnerID: 8, Title: "Команда"}
	groupB := domain.Conversation{ID: "group:b", ChatID: -20, OwnerID: 7}
	otherPrivate := domain.Conversation{ID: "personal:8", ChatID: 8, OwnerID: 8, Private: true}
	for i, conv := range []domain.Conversation{owner, groupA, groupB, otherPrivate} {
		_, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: conv.ID + "-seed", Conversation: conv, Text: "seed"}, int64(i+2))
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.StartSession(ctx, domain.Session{ID: conv.ID + "-session", ConversationID: conv.ID}); err != nil {
			t.Fatal(err)
		}
		if err = repo.AppendMessage(ctx, conv.ID+"-session", "message", domain.Message{Role: "user", Content: "secret in " + conv.ID}); err != nil {
			t.Fatal(err)
		}
		if err = know.Update(ctx, domain.MemoryEntry{ID: "same-id", Scope: conv.ID, Kind: "fact", Content: "memory in " + conv.ID, Source: conv.ID + "-session", UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	visible, err := a.readableConversations(ctx, owner)
	if err != nil || len(visible) != 3 {
		t.Fatalf("owner visible conversations: %v %v", visible, err)
	}
	for _, c := range visible {
		if c.ID == otherPrivate.ID {
			t.Fatal("another user's private chat exposed")
		}
	}
	for _, group := range []domain.Conversation{groupA, groupB} {
		page, err := a.conversationHistory(ctx, owner, toolArgs{ConversationID: group.ID, Query: "secret"})
		if err != nil || len(page.Messages) != 1 || page.Messages[0].ConversationID != group.ID {
			t.Fatalf("owner cannot read group: %v %v", page, err)
		}
	}
	memories, err := a.searchMemories(ctx, owner, toolArgs{Query: "memory"})
	if err != nil || len(memories) != 3 {
		t.Fatalf("owner memory search: %v %v", memories, err)
	}
	// These are intentionally identical memory IDs in different scopes.
	result := tool(t, a, owner, "read-group", "read-memory", "memory_read", `{"id":"same-id","conversation_id":"group:a"}`)
	if !strings.Contains(result, "memory in group:a") {
		t.Fatal(result)
	}
	for _, current := range []domain.Conversation{groupA, groupB, otherPrivate} {
		listed, err := a.readableConversations(ctx, current)
		if err != nil || len(listed) != 1 || listed[0].ID != current.ID {
			t.Fatalf("nonowner list leaked: %v %v", listed, err)
		}
		memories, err := a.searchMemories(ctx, current, toolArgs{Query: "memory"})
		if err != nil || len(memories) != 1 || memories[0].Scope != current.ID {
			t.Fatalf("memory search leaked: %v %v", memories, err)
		}
		for _, target := range []domain.Conversation{owner, groupA, groupB, otherPrivate} {
			if target.ID == current.ID {
				continue
			}
			for _, name := range []string{"conversation_history", "memory_read", "memory_search"} {
				args, _ := json.Marshal(map[string]string{"conversation_id": target.ID, "id": "same-id", "query": "memory"})
				_, err := a.execute(ctx, ToolInput{Conversation: current, Call: domain.ToolCall{Name: name, Arguments: args}})
				if err == nil {
					t.Fatalf("%s from %s leaked %s", name, current.ID, target.ID)
				}
			}
		}
	}
	if _, err := a.readScope(ctx, owner, otherPrivate.ID); err == nil {
		t.Fatal("owner read another private chat")
	}
	if _, err := a.readScope(ctx, owner, "nonexistent"); err == nil {
		t.Fatal("missing scope accepted")
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "owner-context", SessionID: "owner-context", ConversationID: owner.ID})
	if err != nil || !strings.Contains(turn.Messages[0].Content, "speaking privately with your owner") {
		t.Fatalf("owner context missing: %v", err)
	}
	if strings.Contains(turn.Messages[0].Content, "secret in group:") {
		t.Fatal("whole group history automatically injected")
	}
}

func TestHistoryKeepsStructuredPageForLargeMarkupMessages(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	if err := repo.StartSession(ctx, domain.Session{ID: "large-history", ConversationID: conv.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendMessage(ctx, "large-history", "large", domain.Message{Role: "user", Content: strings.Repeat("<я>", 12000)}); err != nil {
		t.Fatal(err)
	}
	page, err := a.conversationHistory(ctx, conv, toolArgs{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(page)
	if err != nil || len(data) > 25000 || len(page.Messages) != 1 || !page.Messages[0].Truncated {
		t.Fatalf("invalid oversized page: %d %v", len(data), err)
	}
}
