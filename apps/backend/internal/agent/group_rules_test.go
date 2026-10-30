package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

func TestGroupRulesToolsRequireOwnerInteractivePrivateChat(t *testing.T) {
	for _, tc := range []struct {
		name                string
		private, background bool
		owner, sender, chat int64
		task                string
	}{
		{name: "group", owner: 7, sender: 8, chat: -20},
		{name: "owner in group", owner: 7, sender: 7, chat: -20},
		{name: "other private", private: true, owner: 7, sender: 8, chat: 8},
		{name: "spoofed owner", private: true, owner: 7, sender: 7, chat: 8},
		{name: "unconfigured owner", private: true, sender: 7, chat: 7},
		{name: "initiative", private: true, background: true, owner: 7, sender: 7, chat: 7},
		{name: "task", private: true, owner: 7, sender: 7, chat: 7, task: "task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, k, conv := fixture(t)
			a.OwnerUserID = tc.owner
			conv.Private, conv.OwnerID, conv.ChatID = tc.private, tc.sender, tc.chat
			for _, def := range a.toolsForTurn(Turn{Conversation: conv, Input: RunInput{Initiative: tc.background, TaskID: tc.task}}) {
				if def.Name == "group_read" || def.Name == "group_update" {
					t.Fatal("restricted turn advertised GROUP.md tools")
				}
			}
			for _, name := range []string{"group_read", "group_update"} {
				out, err := a.execute(context.Background(), ToolInput{Conversation: conv, Background: tc.background, TaskID: tc.task,
					Call: domain.ToolCall{Name: name, Arguments: []byte(`{"conversation_id":"group","old_text":"","new_text":"rules"}`)}})
				if err == nil || out != nil {
					t.Fatalf("forged %s bypassed owner check: %v %v", name, out, err)
				}
			}
			if got, err := k.GroupRules(context.Background(), -20); err != nil || got != "" {
				t.Fatalf("unauthorized rules persisted: %q %v", got, err)
			}
		})
	}
}

func TestOwnerGroupRulesApplyAcrossTopicsAndReloadWithoutPrivateContext(t *testing.T) {
	a, repo, k, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	a.AllowedGroupIDs = []int64{-20}
	ctx := context.Background()
	groups := []domain.Conversation{
		{ID: "group:20:topic:0", ChatID: -20, OwnerID: 8},
		{ID: "group:20:topic:9", ChatID: -20, ThreadID: 9, OwnerID: 8},
		{ID: "group:30:topic:0", ChatID: -30, OwnerID: 8},
	}
	for _, conv := range groups {
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: conv.ID + "-seed", Conversation: conv, Text: "hello"}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "private", Scope: owner.ID, Kind: "preference", Content: "PRIVATE_SENTINEL: owner's home address"}); err != nil {
		t.Fatal(err)
	}
	before, err := a.Prepare(ctx, RunInput{Key: "before", ConversationID: groups[0].ID, SessionID: groups[0].ID})
	if err != nil || strings.Contains(before.Messages[0].Content, "GROUP.md:") {
		t.Fatalf("missing GROUP.md changed defaults: %v", err)
	}
	readArgs := `{"conversation_id":"group:20:topic:0"}`
	read := tool(t, a, owner, "read-empty", "read", "group_read", readArgs)
	if !strings.Contains(read, `"content":""`) || !strings.Contains(read, `"scope":"whole_group"`) {
		t.Fatal(read)
	}
	const first = "GROUP_SENTINEL: Отвечай только по обращению.\nСохраняй характер."
	args, _ := json.Marshal(map[string]string{"conversation_id": groups[0].ID, "old_text": "", "new_text": first})
	for i := 0; i < 2; i++ {
		out := tool(t, a, owner, "create-rules", "create", "group_update", string(args))
		if !strings.Contains(out, `"state":"updated"`) || !strings.Contains(out, `"applies":"next_turn"`) {
			t.Fatal(out)
		}
	}
	// Editing through another topic changes the same file, preserving other rules.
	args, _ = json.Marshal(map[string]string{"conversation_id": groups[1].ID, "old_text": "Сохраняй характер.", "new_text": "Отвечай кратко."})
	if out := tool(t, a, owner, "edit-rules", "edit", "group_update", string(args)); ToolFailed(out) {
		t.Fatal(out)
	}
	for _, conv := range append(groups, owner) {
		turn, err := a.Prepare(ctx, RunInput{Key: "after-" + conv.ID, ConversationID: conv.ID, SessionID: conv.ID})
		if err != nil {
			t.Fatal(err)
		}
		system := turn.Messages[0].Content
		if strings.Contains(system, "GROUP_SENTINEL") != (conv.ChatID == -20) {
			t.Fatalf("GROUP.md scope violated for %s", conv.ID)
		}
		if strings.Contains(system, "PRIVATE_SENTINEL") != conv.Private {
			t.Fatalf("private USER.md scope violated for %s", conv.ID)
		}
		if conv.ChatID == -20 && (!strings.Contains(system, "Отвечай кратко.") || strings.Contains(system, "Сохраняй характер.")) {
			t.Fatal("existing session did not reload the updated rules")
		}
	}
	for _, target := range []string{"", "missing", owner.ID, groups[2].ID} {
		for _, name := range []string{"group_read", "group_update"} {
			args, _ := json.Marshal(map[string]string{"conversation_id": target, "old_text": "", "new_text": "rules"})
			out := tool(t, a, owner, name+target, "invalid", name, string(args))
			if !ToolFailed(out) {
				t.Fatalf("invalid or disabled target accepted: %s", out)
			}
		}
	}
	current, err := k.GroupRules(ctx, -20)
	if err != nil {
		t.Fatal(err)
	}
	args, _ = json.Marshal(map[string]string{"conversation_id": groups[0].ID, "old_text": current, "new_text": ""})
	if out := tool(t, a, owner, "clear-rules", "clear", "group_update", string(args)); ToolFailed(out) {
		t.Fatal(out)
	}
	cleared, err := a.Prepare(ctx, RunInput{Key: "cleared", ConversationID: groups[1].ID, SessionID: groups[1].ID})
	if err != nil || strings.Contains(cleared.Messages[0].Content, "GROUP.md:") {
		t.Fatalf("cleared rules still in topic context: %v", err)
	}
}

func TestGroupRulesCachedResultsCannotBypassOwnerCheck(t *testing.T) {
	for _, name := range []string{"group_read", "group_update"} {
		t.Run(name, func(t *testing.T) {
			a, repo, _, owner := fixture(t)
			a.OwnerUserID = owner.OwnerID
			a.AllowedGroupIDs = []int64{-20}
			group := domain.Conversation{ID: "group", ChatID: -20, OwnerID: 8}
			if _, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "group-seed", Conversation: group, Text: "hello"}, 2); err != nil {
				t.Fatal(err)
			}
			in := ToolInput{RunKey: "same-run", SessionID: "session", Conversation: owner,
				Call: domain.ToolCall{ID: "same-call", Name: name, Arguments: []byte(`{"conversation_id":"group","old_text":"","new_text":"rules"}`)}}
			out, err := a.ToolCall(context.Background(), in)
			if err != nil || ToolFailed(out) {
				t.Fatalf("owner call failed: %s %v", out, err)
			}
			in.Conversation = group
			out, err = a.ToolCall(context.Background(), in)
			if err != nil || !ToolFailed(out) {
				t.Fatalf("cached result bypassed owner check: %s %v", out, err)
			}
		})
	}
}

func TestGroupRulesSurviveLargePersonalityAndProfile(t *testing.T) {
	a, repo, k, _ := fixture(t)
	ctx := context.Background()
	group := domain.Conversation{ID: "large-context-group", ChatID: -20, OwnerID: 8}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "group-seed", Conversation: group, Text: "hello"}, 2); err != nil {
		t.Fatal(err)
	}
	soul, err := k.Soul(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.UpdateSoul(ctx, soul, strings.Repeat("s", 12000)); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "large", Scope: group.ID, Kind: "profile", Content: strings.Repeat("p", 8000)}); err != nil {
		t.Fatal(err)
	}
	rules := strings.Repeat("я", 3980) + "GROUP_RULES_END"
	if err := k.UpdateGroupRules(ctx, group.ChatID, "", rules); err != nil {
		t.Fatal(err)
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "large-context", ConversationID: group.ID, SessionID: "large-context"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(turn.Messages[0].Content, rules) {
		t.Fatal("context compaction truncated GROUP.md")
	}
}
