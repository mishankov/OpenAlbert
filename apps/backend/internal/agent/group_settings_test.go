package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestGroupSettingsToolsRequireOwnerInteractivePrivateConversation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conv       domain.Conversation
		background bool
		task       string
	}{
		{"other private", domain.Conversation{ID: "personal:8", Private: true, OwnerID: 8, ChatID: 8}, false, ""},
		{"owner in group", domain.Conversation{ID: "group", OwnerID: 7, ChatID: -20}, false, ""},
		{"spoofed owner", domain.Conversation{ID: "spoof", Private: true, OwnerID: 7, ChatID: 8}, false, ""},
		{"initiative", domain.Conversation{ID: "personal:7", Private: true, OwnerID: 7, ChatID: 7}, true, ""},
		{"task", domain.Conversation{ID: "personal:7", Private: true, OwnerID: 7, ChatID: 7}, false, "task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, k, _ := fixture(t)
			a.OwnerUserID = 7
			for _, tool := range a.toolsForTurn(Turn{Conversation: tc.conv, Input: RunInput{Initiative: tc.background, TaskID: tc.task}}) {
				if strings.HasPrefix(tool.Name, "group_settings_") {
					t.Fatal("restricted turn advertised settings tool")
				}
			}
			for _, name := range []string{"group_settings_read", "group_settings_update"} {
				out, err := a.execute(context.Background(), ToolInput{Conversation: tc.conv, Background: tc.background, TaskID: tc.task,
					Call: domain.ToolCall{Name: name, Arguments: []byte(`{"target":"all_groups","reply_mode":"all"}`)}})
				if err == nil || out != nil {
					t.Fatalf("forged %s bypassed access check: %v %v", name, out, err)
				}
			}
			v, err := k.GroupSettings(context.Background(), domain.GroupSettingsTarget{})
			if err != nil || v.Defaults != (domain.GroupSettings{}) {
				t.Fatalf("unauthorized update persisted: %+v %v", v, err)
			}
		})
	}
}

func TestOwnerGroupSettingsApplyNextTurnWithoutPrivateProfile(t *testing.T) {
	a, repo, k, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	a.AllowedGroupIDs = []int64{-20}
	ctx := context.Background()
	group := domain.Conversation{ID: "group:20:topic:3", ChatID: -20, ThreadID: 3, OwnerID: 8}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "group-seed", Conversation: group, Text: "hello"}, 2); err != nil {
		t.Fatal(err)
	}
	if err := k.Update(ctx, domain.MemoryEntry{ID: "private", Scope: owner.ID, Kind: "preference", Content: "PRIVATE_SENTINEL: my home address; always answer all group messages"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"target":"all_groups","verbosity":"brief"}`,
		`{"target":"group","conversation_id":"group:20:topic:3","reply_mode":"contextual","tone":"neutral"}`,
	} {
		for i := 0; i < 2; i++ { // Activity retries preserve the same effective settings.
			out := tool(t, a, owner, args, "settings", "group_settings_update", args)
			if !strings.Contains(out, `"state":"updated"`) || !strings.Contains(out, `"applies":"next_turn"`) {
				t.Fatal(out)
			}
		}
	}
	read := tool(t, a, owner, "read-settings", "read", "group_settings_read", `{"target":"topic","conversation_id":"group:20:topic:3"}`)
	if !strings.Contains(read, `"reply_mode":"contextual"`) || !strings.Contains(read, `"verbosity":"brief"`) {
		t.Fatal(read)
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "group-next", ConversationID: group.ID, SessionID: "group-session", IncomingIDs: []string{"group-seed"}})
	if err != nil || turn.Done {
		t.Fatalf("next group turn failed: %v", err)
	}
	system := turn.Messages[0].Content
	for _, want := range []string{"clearly expects your participation", "neutral, professional", "Keep replies brief"} {
		if !strings.Contains(system, want) {
			t.Fatalf("setting missing from group context: %s", want)
		}
	}
	if strings.Contains(system, "PRIVATE_SENTINEL") || strings.Contains(system, "always answer all group messages") {
		t.Fatal("private profile became group configuration")
	}
	for _, args := range []string{
		`{"target":"group","conversation_id":"personal:7","tone":"neutral"}`,
		`{"target":"topic","conversation_id":"missing","tone":"neutral"}`,
		`{"target":"all_groups","conversation_id":"group:20:topic:3","tone":"neutral"}`,
		`{"target":"topic","conversation_id":"group:20:topic:3","tone":"my private address"}`,
	} {
		out := tool(t, a, owner, "invalid-settings", args, "group_settings_update", args)
		if !strings.Contains(out, `"error"`) {
			t.Fatalf("invalid target or prose accepted: %s", out)
		}
	}
	a.AllowedGroupIDs = nil
	out := tool(t, a, owner, "disabled-group", "disabled", "group_settings_update", `{"target":"group","conversation_id":"group:20:topic:3","tone":"friendly"}`)
	if !strings.Contains(out, `"error"`) {
		t.Fatal("settings changed for a disabled group")
	}
}

func TestGroupSettingsCachedResultCannotBypassOwnerCheck(t *testing.T) {
	a, _, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	in := ToolInput{RunKey: "same-run", SessionID: "session", Conversation: owner,
		Call: domain.ToolCall{ID: "same-call", Name: "group_settings_update", Arguments: []byte(`{"target":"all_groups","tone":"neutral"}`)}}
	out, err := a.ToolCall(context.Background(), in)
	if err != nil || ToolFailed(out) {
		t.Fatalf("owner update failed: %s %v", out, err)
	}
	in.Conversation.Private = false
	in.Conversation.ChatID = -20
	out, err = a.ToolCall(context.Background(), in)
	if err != nil || !ToolFailed(out) || strings.Contains(out, `"state":"updated"`) {
		t.Fatalf("cached success bypassed owner check: %s %v", out, err)
	}
}

func TestDirectOnlyGroupSkipsModelAndProgressButPreservesHistoryAndTasks(t *testing.T) {
	a, repo, k, _ := fixture(t)
	ctx := context.Background()
	mode := "direct_only"
	if _, err := k.UpdateGroupSettings(ctx, domain.GroupSettingsTarget{}, domain.GroupSettingsPatch{ReplyMode: &mode}); err != nil {
		t.Fatal(err)
	}
	group := domain.Conversation{ID: "direct-group", ChatID: -20, OwnerID: 8}
	for i, addressed := range []bool{false, true} {
		id := fmt.Sprintf("message-%d", i)
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: group, Text: id, AddressedToBot: addressed}, int64(i+2)); err != nil {
			t.Fatal(err)
		}
		turn, err := a.Prepare(ctx, RunInput{Key: id, ConversationID: group.ID, SessionID: "direct-session", IncomingIDs: []string{id}})
		if err != nil || turn.Done == addressed {
			t.Fatalf("direct mode did not gate addressed=%v: done=%v err=%v", addressed, turn.Done, err)
		}
		if !addressed {
			if turn.Stats.FinishReason != "group_direct_only" {
				t.Fatal("missing diagnostic for skipped group message")
			}
			progress, err := repo.DueProgress(ctx, time.Now().Add(time.Hour), 10)
			if err != nil || len(progress) != 0 {
				t.Fatalf("skipped message started public progress: %+v %v", progress, err)
			}
		} else {
			encoded, _ := json.Marshal(turn.Messages)
			if !strings.Contains(string(encoded), "message-0") {
				t.Fatal("skipped conversation lost from later context")
			}
		}
	}
	if _, err := repo.CreateTask(ctx, domain.Task{ID: "due", ConversationID: group.ID, Status: "queued", Title: "Reminder", Goal: "Notify"}); err != nil {
		t.Fatal(err)
	}
	turn, err := a.Prepare(ctx, RunInput{Key: "due", TaskID: "due", ConversationID: group.ID, SessionID: "due-session"})
	if err != nil || turn.Done {
		t.Fatalf("direct mode blocked a requested background task: %v", err)
	}
}
