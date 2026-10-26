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
	ctx := context.Background()
	before, err := store.Soul(ctx)
	if err != nil {
		t.Fatal(err)
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

func TestSoulChangesRejectedOutsidePrivateInteractiveTurn(t *testing.T) {
	for _, tc := range []struct {
		name                string
		private, background bool
		task                string
	}{
		{name: "group"},
		{name: "initiative", private: true, background: true},
		{name: "task", private: true, task: "task-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, store, conv := fixture(t)
			conv.Private = tc.private
			ctx := context.Background()
			before, _ := store.Soul(ctx)
			turn := Turn{Conversation: conv, Input: RunInput{Initiative: tc.background, TaskID: tc.task}}
			for _, def := range a.toolsForTurn(turn) {
				if def.Name == "soul_update" {
					t.Fatal("restricted turn advertised soul_update")
				}
			}
			_, err := a.execute(ctx, ToolInput{Conversation: conv, Background: tc.background, TaskID: tc.task,
				Call: domain.ToolCall{Name: "soul_update", Arguments: []byte(`{"old_text":"Ты — OpenAlbert,","new_text":"Ты — Атлас,"}`)}})
			if err == nil {
				t.Fatal("forged call bypassed scope restriction")
			}
			after, _ := store.Soul(ctx)
			if before != after {
				t.Fatal("restricted call changed personality")
			}
		})
	}
}
