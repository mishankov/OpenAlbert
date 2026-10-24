package agent

import (
	"context"
	"encoding/json"
	"openalbert/internal/domain"
	"strings"
	"testing"
	"time"
)

func groupPeople(t *testing.T) (*Activities, domain.Conversation) {
	t.Helper()
	a, _, _, _ := fixture(t)
	a.OwnerUserID = 7
	group := domain.Conversation{ID: "people-group", ChatID: -20}
	for _, person := range []struct {
		id            int64
		message, name string
	}{{8, "donuts-a", "Саша"}, {9, "donuts-b", "Саша"}} {
		source := domain.Incoming{ID: person.message, Conversation: group, AuthorName: person.name, AuthorUsername: "sasha", Text: "Я люблю пончики", CreatedAt: time.Now()}
		source.Conversation.OwnerID = person.id
		if _, err := a.Repo.AcceptIncoming(context.Background(), source, 2); err != nil {
			t.Fatal(err)
		}
	}
	turn, err := a.Prepare(context.Background(), RunInput{Key: "group-turn", SessionID: "group-turn", ConversationID: group.ID, IncomingIDs: []string{"donuts-a", "donuts-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(turn.Messages[1].Content, `"user_id":8`) || !strings.Contains(turn.Messages[2].Content, `"user_id":9`) || !strings.Contains(turn.Messages[1].Content, "donuts-a") {
		t.Fatal("real sender metadata absent from model context")
	}
	return a, group
}
func TestParticipantFactsRequireMatchingAuthorAndStayGroupScoped(t *testing.T) {
	ctx := context.Background()
	a, group := groupPeople(t)
	for _, p := range []struct {
		id           int64
		source, call string
	}{{8, "donuts-a", "a"}, {9, "donuts-b", "b"}} {
		args, _ := json.Marshal(map[string]any{"user_id": p.id, "source_message_id": p.source, "evidence": "Я люблю пончики", "kind": "preference", "content": "Любит пончики"})
		result := tool(t, a, group, "profile-run", p.call, "participant_update", string(args))
		if strings.Contains(result, `"error"`) {
			t.Fatal(result)
		}
	}
	for _, id := range []int64{8, 9} {
		profile, err := a.Knowledge.Participant(ctx, group.ID, id)
		if err != nil || len(profile.Facts) != 1 || profile.Facts[0].SubjectUserID != id {
			t.Fatalf("mixed participants: %v %v", profile, err)
		}
	}
	for _, args := range []string{
		`{"user_id":9,"source_message_id":"donuts-a","evidence":"Я люблю пончики","content":"wrong actor"}`,
		`{"user_id":8,"source_message_id":"donuts-a","evidence":"Люблю пиццу","content":"not in source"}`,
		`{"user_id":8,"source_message_id":"missing","evidence":"Я люблю пончики","content":"missing source"}`,
	} {
		_, err := a.execute(ctx, ToolInput{Conversation: group, Call: domain.ToolCall{Name: "participant_update", Arguments: []byte(args)}})
		if err == nil {
			t.Fatal("unattributed fact accepted")
		}
	}
	owner := domain.Conversation{ID: "personal:7", Private: true, ChatID: 7, OwnerID: 7}
	people, err := a.searchParticipants(ctx, owner, toolArgs{Query: "пончики"})
	if err != nil || len(people) != 2 {
		t.Fatalf("owner cannot find group participants: %v %v", people, err)
	}
	other := domain.Conversation{ID: "other-group", ChatID: -30}
	_, _ = a.Repo.AcceptIncoming(ctx, domain.Incoming{ID: "other-seed", Conversation: other, Text: "hello"}, 3)
	for _, current := range []domain.Conversation{other, {ID: "personal:9", Private: true, ChatID: 9, OwnerID: 9}} {
		_, err := a.participantScope(ctx, current, group.ID)
		if err == nil {
			t.Fatal("profile leaked to another conversation")
		}
	}
	// Existing group facts are supplied on the next message by the same author.
	source := domain.Incoming{ID: "next-message", Conversation: group, Text: "Что мне купить к чаю?", CreatedAt: time.Now()}
	source.Conversation.OwnerID = 8
	_, _ = a.Repo.AcceptIncoming(ctx, source, 4)
	turn, err := a.Prepare(ctx, RunInput{Key: "next-profile-turn", SessionID: "next-profile-turn", ConversationID: group.ID, IncomingIDs: []string{source.ID}})
	if err != nil || !strings.Contains(turn.Messages[0].Content, "Любит пончики") {
		t.Fatalf("personalization missing next turn: %v", err)
	}
}
func TestClosedGroupAnalysisBuildsDistinctProfilesAndRejectsInventedEvidence(t *testing.T) {
	ctx := context.Background()
	a, group := groupPeople(t)
	if err := a.CloseSession(ctx, "group-turn"); err != nil {
		t.Fatal(err)
	}
	a.Model = &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
		if !strings.Contains(messages[1].Content, "source_message_id") || !strings.Contains(messages[0].Content, "multiple distinct people") {
			t.Fatal("analysis lost authors")
		}
		return domain.Message{Content: `{"summary":"Two people like donuts","memories":[{"subject_user_id":8,"source_message_id":"donuts-a","evidence":"Я люблю пончики","content":"Любит пончики","kind":"preference"},{"subject_user_id":9,"source_message_id":"donuts-b","evidence":"Я люблю пончики","content":"Любит пончики","kind":"preference"},{"subject_user_id":9,"source_message_id":"donuts-a","evidence":"Я люблю пончики","content":"Invented fact","kind":"fact"}]}`}, nil
	}}
	for i := 0; i < 2; i++ {
		if err := a.Analyze(ctx, domain.Session{ID: "group-turn", ConversationID: group.ID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{8, 9} {
		profile, err := a.Knowledge.Participant(ctx, group.ID, id)
		if err != nil || len(profile.Facts) != 1 || profile.Facts[0].Content != "Любит пончики" {
			t.Fatalf("analysis mixed or duplicated profiles: %v %v", profile, err)
		}
	}
}

func TestParticipantDeletionRebuildsOwnProfileAndAnalysisDoesNotRestoreIt(t *testing.T) {
	ctx := context.Background()
	a, group := groupPeople(t)
	created := tool(t, a, group, "create", "fact", "participant_update", `{"user_id":8,"source_message_id":"donuts-a","evidence":"Я люблю пончики","content":"Любит пончики","kind":"preference"}`)
	if strings.Contains(created, `"error"`) {
		t.Fatal(created)
	}
	profile, _ := a.Knowledge.Participant(ctx, group.ID, 8)
	id := profile.Facts[0].ID
	// The ordinary memory tool cannot bypass participant attribution checks.
	args, _ := json.Marshal(map[string]any{"id": id, "delete": true})
	_, err := a.execute(ctx, ToolInput{Conversation: group, Call: domain.ToolCall{Name: "memory_update", Arguments: args}})
	if err == nil {
		t.Fatal("ordinary memory deletion bypassed participant checks")
	}
	source := domain.Incoming{ID: "forget-donuts", Conversation: group, Text: "Забудь про пончики", CreatedAt: time.Now()}
	source.Conversation.OwnerID = 8
	_, _ = a.Repo.AcceptIncoming(ctx, source, 4)
	if _, err = a.Prepare(ctx, RunInput{Key: "forget", SessionID: "forget-session", ConversationID: group.ID, IncomingIDs: []string{source.ID}}); err != nil {
		t.Fatal(err)
	}
	group.OwnerID = 8
	args, _ = json.Marshal(map[string]any{"id": id, "delete": true, "user_id": 8, "source_message_id": source.ID, "evidence": source.Text})
	call := domain.ToolCall{ID: "forget", Name: "participant_update", Arguments: args}
	result, err := a.ToolCall(ctx, ToolInput{RunKey: "forget", SessionID: "forget-session", Conversation: group, Call: call})
	if err != nil || strings.Contains(result, `"error"`) {
		t.Fatalf("forget failed %s %v", result, err)
	}
	if err = a.Repo.AppendMessage(ctx, "forget-session", "deletion", domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{call}}); err != nil {
		t.Fatal(err)
	}
	if err = a.CloseSession(ctx, "forget-session"); err != nil {
		t.Fatal(err)
	}
	a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
		return domain.Message{Content: `{"summary":"Forgot preference","memories":[{"subject_user_id":8,"source_message_id":"donuts-a","evidence":"Я люблю пончики","content":"Любит пончики","kind":"preference"}]}`}, nil
	}}
	if err = a.Analyze(ctx, domain.Session{ID: "forget-session", ConversationID: group.ID}); err != nil {
		t.Fatal(err)
	}
	profile, err = a.Knowledge.Participant(ctx, group.ID, 8)
	if err != nil || len(profile.Facts) != 0 {
		t.Fatal("analysis restored forgotten participant fact")
	}
}
