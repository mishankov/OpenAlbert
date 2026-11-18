package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/widgets"
)

func TestWidgetToolsDispatchAndAuthorizeBeforeReplay(t *testing.T) {
	a, repo, _, conv := fixture(t)
	source, err := os.ReadFile("../widgets/testdata/custom-summary.html")
	if err != nil {
		t.Fatal(err)
	}
	state := `{"title":"Расходы за ноябрь","items":[{"label":"Кофе","amount":250},{"label":"Продукты","amount":1200}]}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/sendWidget") {
			var payload domain.Widget
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.HTML != string(source) || payload.ChatData != state || payload.AllowQueries {
				t.Error("generated HTML/JavaScript or its state did not reach Telegram intact")
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
		} else {
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	defer server.Close()
	tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	if err != nil {
		t.Fatal(err)
	}
	a.Widgets = &widgets.Controller{Repo: repo, Transport: tg}
	if err := repo.StartProgress(context.Background(), domain.Progress{TurnID: "widget", Conversation: conv}); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"kind": "html", "html": string(source), "chat_data": state, "caption": "Расходы за ноябрь: 1450 ₽. Кофе — 250 ₽, продукты — 1200 ₽."})
	in := ToolInput{RunKey: "widget", Conversation: conv, Call: domain.ToolCall{ID: "send", Name: "send_widget", Arguments: args}}
	for range 2 {
		if out, err := a.ToolCall(context.Background(), in); err != nil || ToolFailed(out) || !strings.Contains(out, `"message_id":42`) {
			t.Fatalf("%s %v", out, err)
		}
	}
	in.Conversation.DeliveryChannel = "desktop"
	if out, err := a.ToolCall(context.Background(), in); err != nil || !ToolFailed(out) {
		t.Fatalf("desktop replay bypassed access: %s %v", out, err)
	}
	in.Conversation.DeliveryChannel = ""
	in.Conversation.Private = false
	if out, err := a.ToolCall(context.Background(), in); err != nil || !ToolFailed(out) {
		t.Fatalf("group replay bypassed access: %s %v", out, err)
	}
	in.Conversation = conv
	in.Call = domain.ToolCall{ID: "edit", Name: "edit_widget_data", Arguments: json.RawMessage(`{"message_id":42,"chat_data":""}`)}
	if out, err := a.ToolCall(context.Background(), in); err != nil || ToolFailed(out) {
		t.Fatalf("%s %v", out, err)
	}
	if calls != 2 {
		t.Fatalf("duplicate widget send: %d", calls)
	}
	if err := a.Finish(context.Background(), FinishInput{Turn: Turn{Input: RunInput{Key: "widget"}, Conversation: conv, Sent: true}}); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.DueProgress(context.Background(), time.Now(), 10)
	if err != nil || len(rows) != 1 || rows[0].Outcome != "delivered" {
		t.Fatalf("widget progress: %+v %v", rows, err)
	}
	for _, channel := range []string{"", "desktop"} {
		route := conv
		route.DeliveryChannel = channel
		tools := a.toolsForTurn(Turn{Conversation: route})
		found := false
		for _, tool := range tools {
			if tool.Name == "send_widget" {
				found = true
			}
		}
		if found != (channel == "") {
			t.Fatalf("widget tool discovery for channel=%v: %v", channel, found)
		}
	}
}

func TestWidgetRateLimitRemainsAnUncachedActivityRetry(t *testing.T) {
	a, repo, _, conv := fixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprint(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":60}}`)
	}))
	defer server.Close()
	tg, _ := integrations.NewTelegram(integrations.TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	a.Widgets = &widgets.Controller{Repo: repo, Transport: tg}
	in := ToolInput{RunKey: "limited", Conversation: conv, Call: domain.ToolCall{ID: "send", Name: "send_widget", Arguments: json.RawMessage(`{"kind":"html","html":"ok","caption":"ok"}`)}}
	for range 2 {
		if out, err := a.ToolCall(context.Background(), in); err == nil || out != "" {
			t.Fatalf("429 cached as tool result: %s %v", out, err)
		}
	}
	if calls != 1 {
		t.Fatal("retry ignored cooldown")
	}
	if _, found, err := repo.LoadCheckpoint(context.Background(), "tool/"+ID(in.RunKey, "0", in.Call.ID)); err != nil || found {
		t.Fatal("retryable tool error was cached")
	}
}

func TestOverlappingWidgetCallCannotCacheOverLaterRateLimit(t *testing.T) {
	a, repo, _, conv := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = fmt.Fprint(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":60}}`)
	}))
	defer server.Close()
	tg, _ := integrations.NewTelegram(integrations.TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	a.Widgets = &widgets.Controller{Repo: repo, Transport: tg}
	if _, err := repo.CreateTask(context.Background(), domain.Task{ID: "nba", ConversationID: conv.ID, Title: "НБА", Goal: "Scores", Status: "waiting", ScheduleSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	in := ToolInput{RunKey: "overlap", Conversation: conv, Call: domain.ToolCall{ID: "send", Name: "send_widget", Arguments: json.RawMessage(`{"kind":"schedules","task_ids":["nba"]}`)}}
	done := make(chan error, 1)
	go func() { _, err := a.ToolCall(context.Background(), in); done <- err }()
	<-started
	task, _, _ := repo.Task(context.Background(), conv.ID, "nba")
	task.Status = "cancelled"
	if _, err := repo.UpdateTask(context.Background(), task); err != nil {
		close(release)
		t.Fatal(err)
	}
	_, overlapErr := a.ToolCall(context.Background(), in)
	close(release)
	if overlapErr == nil || <-done == nil {
		t.Fatal("overlapping observation or rate limit was finalized")
	}
	_, retryErr := a.ToolCall(context.Background(), in)
	if retryErr == nil || !strings.Contains(retryErr.Error(), "rate limit") {
		t.Fatalf("retry did not reach saved cooldown: %v", retryErr)
	}
	if _, found, err := repo.LoadCheckpoint(context.Background(), "tool/"+ID(in.RunKey, "0", in.Call.ID)); err != nil || found {
		t.Fatal("in-flight observation poisoned result cache")
	}
}
