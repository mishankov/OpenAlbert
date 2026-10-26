package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

type logReaderSpy struct{ filters []domain.LogFilter }

func (s *logReaderSpy) Search(_ context.Context, f domain.LogFilter) (domain.LogSearch, error) {
	s.filters = append(s.filters, f)
	return domain.LogSearch{ConversationID: f.ConversationID, Summary: []domain.LogCount{{Operation: "llm.complete", Result: "error", ErrorCode: "http_400", Count: 2}}, Events: []map[string]string{{"turn_id": f.TurnID, "error_code": "http_400"}}}, nil
}

func TestLogsAreOnlyAvailableInOwnersPrivateChatAndScheduledTasks(t *testing.T) {
	a, _, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	spy := &logReaderSpy{}
	a.Logs = spy
	for _, tc := range []struct {
		name       string
		conv       domain.Conversation
		task       string
		background bool
		allowed    bool
	}{
		{"owner", owner, "", false, true},
		{"owner initiative", owner, "", true, false},
		{"owner task", owner, "task", false, true},
		{"owner initiative with task", owner, "task", true, false},
		{"group task", domain.Conversation{ID: "group:1", ChatID: -1, OwnerID: owner.OwnerID}, "task", false, false},
		{"other private task", domain.Conversation{ID: "personal:8", Private: true, ChatID: 8, OwnerID: 8}, "task", false, false},
		{"group owned by owner", domain.Conversation{ID: "group:1", ChatID: -1, OwnerID: owner.OwnerID}, "", false, false},
		{"other private", domain.Conversation{ID: "personal:8", Private: true, ChatID: 8, OwnerID: 8}, "", false, false},
		{"spoofed owner", domain.Conversation{ID: "personal:8", Private: true, ChatID: 8, OwnerID: owner.OwnerID}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			visible := false
			for _, def := range a.toolsForTurn(Turn{Conversation: tc.conv, Input: RunInput{TaskID: tc.task, Initiative: tc.background}}) {
				if def.Name == "logs_search" {
					if !json.Valid(def.Parameters) {
						t.Fatal("log tool schema cannot be sent to the model API")
					}
					visible = true
				}
			}
			if visible != tc.allowed {
				t.Fatalf("visibility=%v want %v", visible, tc.allowed)
			}
			before := len(spy.filters)
			_, err := a.execute(context.Background(), ToolInput{Conversation: tc.conv, TaskID: tc.task, Background: tc.background, Call: domain.ToolCall{Name: "logs_search", Arguments: json.RawMessage(`{}`)}})
			if (err == nil) != tc.allowed || len(spy.filters)-before != map[bool]int{true: 1, false: 0}[tc.allowed] {
				t.Fatalf("unauthorized backend call: %v %v", spy.filters, err)
			}
		})
	}
	a.Logs = nil
	for _, def := range a.toolsForTurn(Turn{Conversation: owner}) {
		if def.Name == "logs_search" {
			t.Fatal("disabled reader advertised")
		}
	}
	if _, err := a.execute(context.Background(), ToolInput{Conversation: owner, Call: domain.ToolCall{Name: "logs_search", Arguments: json.RawMessage(`{}`)}}); err == nil {
		t.Fatal("disabled tool executed")
	}
}

func TestOwnerLogSearchScopesAndStrictArguments(t *testing.T) {
	a, repo, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	spy := &logReaderSpy{}
	a.Logs = spy
	for _, conv := range []domain.Conversation{{ID: "group:a", ChatID: -1}, {ID: "personal:8", ChatID: 8, OwnerID: 8, Private: true}} {
		if _, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: conv.ID, Conversation: conv}, 2); err != nil {
			t.Fatal(err)
		}
	}
	args := `{"conversation_id":"group:a","turn_id":"turn-1","since":"2026-10-22T12:00:00+03:00","until":"2026-10-22T13:00:00+03:00","limit":5}`
	result := tool(t, a, owner, "diagnose", "logs", "logs_search", args)
	if !strings.Contains(result, `"summary"`) || !strings.Contains(result, "http_400") {
		t.Fatal(result)
	}
	f := spy.filters[0]
	if f.ConversationID != "group:a" || f.TurnID != "turn-1" || f.Limit != 5 || f.Until.Sub(f.Since) != time.Hour {
		t.Fatalf("bad filters: %+v", f)
	}
	if rows, err := repo.PendingOutgoing(context.Background(), 10); err != nil || len(rows) != 0 {
		t.Fatal("read tool sent a message")
	}
	for _, bad := range []string{`{"conversation_id":"personal:8"}`, `{"conversation_id":"unknown"}`, `{"query":"* | delete"}`, `{"url":"http://secret.example"}`, `{"limit":51}`, `{"limit":-1}`, `{"result":"anything"}`, `{"since":"yesterday"}`, `{} {}`, `null`, `{"turn_id":"` + strings.Repeat("a", 4100) + `"}`} {
		before := len(spy.filters)
		_, err := a.execute(context.Background(), ToolInput{Conversation: owner, Call: domain.ToolCall{Name: "logs_search", Arguments: json.RawMessage(bad)}})
		if err == nil || len(spy.filters) != before {
			t.Fatalf("invalid/foreign query accepted: %s %v", bad, err)
		}
	}
}

func TestCachedLogResultCannotBypassOwnerChecks(t *testing.T) {
	a, _, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	spy := &logReaderSpy{}
	a.Logs = spy
	in := ToolInput{RunKey: "cached-diagnostics", SessionID: "session", Conversation: owner, Call: domain.ToolCall{ID: "read", Name: "logs_search", Arguments: json.RawMessage(`{"turn_id":"private-owner-turn"}`)}}
	if out, err := a.ToolCall(context.Background(), in); err != nil || !strings.Contains(out, "private-owner-turn") {
		t.Fatalf("owner read failed: %s %v", out, err)
	}
	in.Conversation = domain.Conversation{ID: "group:1", ChatID: -1, OwnerID: owner.OwnerID}
	out, err := a.ToolCall(context.Background(), in)
	if err != nil || strings.Contains(out, "private-owner-turn") || !strings.Contains(out, "logs_access_denied") || len(spy.filters) != 1 {
		t.Fatalf("cache bypassed owner check: %s %v", out, err)
	}
	in.Conversation = owner
	in.Background = true
	out, err = a.ToolCall(context.Background(), in)
	if err != nil || strings.Contains(out, "private-owner-turn") || !strings.Contains(out, "logs_access_denied") || len(spy.filters) != 1 {
		t.Fatalf("initiative replayed diagnostics from cache: %s %v", out, err)
	}
	in.Background = false
	a.Logs = nil
	out, err = a.ToolCall(context.Background(), in)
	if err != nil || strings.Contains(out, "private-owner-turn") || !strings.Contains(out, "logs_disabled") {
		t.Fatalf("disabled diagnostics replayed a cache: %s %v", out, err)
	}
}
