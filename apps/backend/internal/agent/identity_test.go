package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/knowledge"
)

func TestIdentityComesFromOwnerSoulAndReloadsWithoutBuiltInName(t *testing.T) {
	a, _, _, conv := fixture(t)
	soulPath := filepath.Join(t.TempDir(), "SOUL.md")
	if err := os.WriteFile(soulPath, []byte("# Ирис\nТы — Ирис, персональный помощник.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := knowledge.NewWithSoulFile(t.TempDir(), soulPath)
	if err != nil {
		t.Fatal(err)
	}
	a.Knowledge = store
	for _, name := range []string{"Ирис", "Атлас"} {
		if err := os.WriteFile(soulPath, []byte("# "+name+"\nТы — "+name+", персональный помощник.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		turn, err := a.Prepare(context.Background(), RunInput{Key: name, SessionID: name, ConversationID: conv.ID})
		if err != nil {
			t.Fatal(err)
		}
		system := turn.Messages[0].Content
		if !strings.Contains(system, "Ты — "+name) || strings.Contains(system, "OpenAlbert") {
			t.Fatal("system prompt does not use the owner-defined identity exclusively")
		}
		if name == "Атлас" && strings.Contains(system, "Ирис") {
			t.Fatal("previous identity survived SOUL reload")
		}
	}
}

func TestSoulToolRenamesAndNextTurnLoadsNewIdentity(t *testing.T) {
	a, _, store, conv := fixture(t)
	a.OwnerUserID = conv.OwnerID
	ctx := context.Background()
	before, err := store.Soul(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := tool(t, a, conv, "read-soul", "read-call", "soul_read", `{}`)
	if !strings.Contains(read, "Ты — OpenAlbert,") {
		t.Fatalf("owner could not read personality: %s", read)
	}
	args := `{"old_text":"Ты — OpenAlbert,","new_text":"Ты — Атлас,"}`
	for i := 0; i < 2; i++ {
		result := tool(t, a, conv, "rename", "rename-call", "soul_update", args)
		if !strings.Contains(result, `"state":"updated"`) {
			t.Fatalf("rename failed: %s", result)
		}
	}
	after, err := store.Soul(ctx)
	if err != nil || after != strings.Replace(before, "Ты — OpenAlbert,", "Ты — Атлас,", 1) {
		t.Fatalf("rename changed unrelated personality text: %q, %v", after, err)
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "after-rename", SessionID: "after-rename", ConversationID: conv.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(turn.Messages[0].Content, "Ты — Атлас,") || strings.Contains(turn.Messages[0].Content, "Ты — OpenAlbert,") {
		t.Fatal("next turn did not load the renamed identity")
	}
}

func TestSoulToolsRequireOwnerInteractivePrivateTurn(t *testing.T) {
	for _, tc := range []struct {
		name                string
		private, background bool
		task                string
		owner, sender, chat int64
	}{
		{name: "group", owner: 7, sender: 8, chat: -10},
		{name: "owner in group", owner: 7, sender: 7, chat: -10},
		{name: "other private", private: true, owner: 7, sender: 8, chat: 8},
		{name: "spoofed owner", private: true, owner: 7, sender: 7, chat: 8},
		{name: "owner not configured", private: true, sender: 7, chat: 7},
		{name: "initiative", private: true, background: true, owner: 7, sender: 7, chat: 7},
		{name: "task", private: true, task: "task-id", owner: 7, sender: 7, chat: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, store, conv := fixture(t)
			a.OwnerUserID = tc.owner
			conv.Private = tc.private
			conv.OwnerID, conv.ChatID = tc.sender, tc.chat
			ctx := context.Background()
			before, _ := store.Soul(ctx)
			turn := Turn{Conversation: conv, Input: RunInput{Initiative: tc.background, TaskID: tc.task}}
			for _, def := range a.toolsForTurn(turn) {
				if def.Name == "soul_update" || def.Name == "soul_read" {
					t.Fatalf("restricted turn advertised %s", def.Name)
				}
			}
			for _, name := range []string{"soul_read", "soul_update"} {
				out, err := a.execute(ctx, ToolInput{Conversation: conv, Background: tc.background, TaskID: tc.task,
					Call: domain.ToolCall{Name: name, Arguments: []byte(`{"old_text":"Ты — OpenAlbert,","new_text":"Ты — Атлас,"}`)}})
				if err == nil || out != nil {
					t.Fatalf("forged %s bypassed scope restriction: %v, %v", name, out, err)
				}
			}
			after, _ := store.Soul(ctx)
			if before != after {
				t.Fatal("restricted call changed personality")
			}
		})
	}
}
