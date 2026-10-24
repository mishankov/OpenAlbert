package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"openalbert/internal/domain"
)

func TestOwnerQueuesGroupPostOnceAndOnlyPublicTextEntersGroupHistory(t *testing.T) {
	a, repo, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	a.AllowedGroupIDs = []int64{-20}
	ctx := context.Background()
	group := domain.Conversation{ID: "group:20:topic:3", ChatID: -20, ThreadID: 3, OwnerID: 8, Title: "Команда"}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "group-seed", Conversation: group, Text: "hello"}, 2); err != nil {
		t.Fatal(err)
	}
	for retry := 0; retry < 2; retry++ {
		result := tool(t, a, owner, "private-command", "group-post", "send_group_message", `{"conversation_id":"group:20:topic:3","text":"**Встреча в 15:00**"}`)
		if !strings.Contains(result, `"state":"queued"`) {
			t.Fatal(result)
		}
	}
	outgoing, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(outgoing) != 1 {
		t.Fatalf("duplicate or missing post: %v %v", outgoing, err)
	}
	out := outgoing[0]
	if out.Conversation.ID != group.ID || out.Conversation.ChatID != -20 || out.Conversation.ThreadID != 3 || out.Conversation.OwnerID != owner.OwnerID || out.Text != "**Встреча в 15:00**" || out.SessionID != "session" {
		t.Fatalf("wrong destination or trace: %+v", out)
	}
	history, err := repo.ConversationHistory(ctx, group.ID, "", 0, 20)
	if err != nil || len(history) != 1 || history[0].Content != out.Text || !history[0].ReplyRequested {
		t.Fatalf("group history wrong: %v %v", history, err)
	}
	analyses, err := repo.PendingAnalysis(ctx, 20)
	if err != nil || len(analyses) != 0 {
		t.Fatal("post created unwanted session analysis")
	}
	// It does not prevent the ordinary reply to the owner's private chat.
	if result := tool(t, a, owner, "private-command", "private-ack", "send_message", `{"text":"Сообщение для команды поставлено в очередь."}`); strings.Contains(result, `"error"`) {
		t.Fatal(result)
	}
	outgoing, _ = repo.PendingOutgoing(ctx, 10)
	if len(outgoing) != 2 || outgoing[1].Conversation != owner {
		t.Fatal("private acknowledgement misrouted")
	}
}

func TestGroupPostingRejectsUnprivilegedOrInvalidTargets(t *testing.T) {
	cases := []struct {
		name                string
		private, background bool
		owner               int64
		task, target, text  string
		allow               bool
	}{
		{name: "group", target: "group", text: "hello", allow: true},
		{name: "other private", private: true, owner: 8, target: "group", text: "hello", allow: true},
		{name: "initiative", private: true, owner: 7, background: true, target: "group", text: "hello", allow: true},
		{name: "task", private: true, owner: 7, task: "task-id", target: "group", text: "hello", allow: true},
		{name: "unlisted group", private: true, owner: 7, target: "group", text: "hello"},
		{name: "private target", private: true, owner: 7, target: "personal:7", text: "hello", allow: true},
		{name: "unknown", private: true, owner: 7, target: "missing", text: "hello", allow: true},
		{name: "missing target", private: true, owner: 7, text: "hello", allow: true},
		{name: "empty", private: true, owner: 7, target: "group", text: " \n\u200b", allow: true},
		{name: "too large", private: true, owner: 7, target: "group", text: strings.Repeat("я", 16001), allow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, current := fixture(t)
			a.OwnerUserID = 7
			if tc.allow {
				a.AllowedGroupIDs = []int64{-20}
			}
			current.Private, current.OwnerID, current.ChatID = tc.private, tc.owner, tc.owner
			if !current.Private {
				current.ID = "group"
				current.ChatID = -20
			}
			_, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "target", Conversation: domain.Conversation{ID: "group", ChatID: -20}}, 2)
			if err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"conversation_id": tc.target, "text": tc.text})
			_, err = a.execute(context.Background(), ToolInput{Conversation: current, TaskID: tc.task, Background: tc.background, Call: domain.ToolCall{Name: "send_group_message", Arguments: args}})
			if err == nil {
				t.Fatal("restricted post accepted")
			}
			pending, _ := repo.PendingOutgoing(context.Background(), 10)
			if len(pending) != 0 {
				t.Fatal("rejected post entered outbox")
			}
			if !a.isOwnerPrivate(current) || tc.background || tc.task != "" {
				for _, def := range a.toolsForTurn(Turn{Conversation: current, Input: RunInput{TaskID: tc.task, Initiative: tc.background}}) {
					if def.Name == "send_group_message" {
						t.Fatal("restricted turn advertised post tool")
					}
				}
			}
		})
	}
}
