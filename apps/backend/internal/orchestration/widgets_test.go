package orchestration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/widgets"
)

type slowWidgetRegistry struct{ domain.Repository }

func (r slowWidgetRegistry) LoadCheckpoint(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if key == "widget/7/41" {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return r.Repository.LoadCheckpoint(ctx, key)
}

func TestSlowQueryDoesNotDelayPollingOrAnswersInNextBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls atomic.Int32
	answered := make(chan struct{}, 1)
	r, repo, _ := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getUpdates"):
			switch polls.Add(1) {
			case 1:
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"widget_query":{"id":"slow","from":{"id":7},"chat":{"id":7,"type":"private"},"message_id":41,"type":{"type":"data"},"data":"{}"}}]}`)
			case 2:
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":2,"widget_query":{"id":"fast","from":{"id":7},"chat":{"id":7,"type":"private"},"message_id":42,"type":{"type":"data"},"data":"{}"}}]}`)
			default:
				<-ctx.Done()
			}
		case strings.HasSuffix(req.URL.Path, "/answerWidgetQuery"):
			var p struct {
				ID string `json:"widget_query_id"`
			}
			_ = json.NewDecoder(req.Body).Decode(&p)
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
			if p.ID == "fast" {
				answered <- struct{}{}
			}
		default:
			t.Errorf("unexpected call: %s", req.URL.Path)
		}
	})
	r.Activities = &agent.Activities{Widgets: &widgets.Controller{Repo: slowWidgetRegistry{repo}, Transport: r.Telegram}}
	r.wg.Add(1)
	go func() { defer r.wg.Done(); r.poll(ctx) }()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Error("slow query blocked the next batch's answer")
	}
	cancel()
	r.Wait()
}

func TestWidgetKeyboardCommandsBypassSessionWorkflow(t *testing.T) {
	calls := 0
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		if !strings.HasSuffix(req.URL.Path, "/sendWidget") {
			t.Errorf("unexpected method: %s", req.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	ctx := context.Background()
	if _, err := repo.CreateTask(ctx, domain.Task{ID: "nba", ConversationID: conv.ID, Title: "НБА", Goal: "Scores", Status: "paused", ScheduleSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "keyboard", Conversation: conv, Text: "/schedules", Command: "schedules"}, 1); err != nil {
		t.Fatal(err)
	}
	r.Activities = &agent.Activities{Repo: repo, Widgets: &widgets.Controller{Repo: repo, Transport: r.Telegram}}
	// Client and Model are deliberately absent. A command must never start a
	// session workflow; the paused task must also remain paused.
	if err := r.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	incoming, err := repo.PendingIncoming(ctx, 10)
	if err != nil || len(incoming) != 0 || calls != 1 {
		t.Fatalf("command not handled exactly once: calls=%d inbox=%v err=%v", calls, incoming, err)
	}
}

func TestWidgetCooldownDoesNotBlockFollowingStopCommand(t *testing.T) {
	for _, panel := range []string{"populated", "empty", "invalid"} {
		t.Run(panel, func(t *testing.T) {
			requests := 0
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				requests++
				_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":60}}`)
			})
			ctx := context.Background()
			if _, err := repo.CreateTask(ctx, domain.Task{ID: "nba", ConversationID: conv.ID, Title: "НБА", Goal: "Scores", Status: "paused", ScheduleSeconds: 3600}); err != nil {
				t.Fatal(err)
			}
			if panel == "empty" {
				task, _, _ := repo.Task(ctx, conv.ID, "nba")
				task.Status = "cancelled"
				if _, err := repo.UpdateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			text := "/schedules"
			if panel == "invalid" {
				text = "/schedules invalid-page"
			}
			other := domain.Conversation{ID: "personal:8", ChatID: 8, OwnerID: 8, Private: true}
			for i, in := range []domain.Incoming{
				{ID: "a-panel", Conversation: conv, Text: text, Command: "schedules"},
				{ID: "b-stop", Conversation: other, Text: "/stop", Command: "stop"},
			} {
				if _, err := repo.AcceptIncoming(ctx, in, int64(i+1)); err != nil {
					t.Fatal(err)
				}
			}
			client := &commandClient{}
			r.Client = client
			r.Activities = &agent.Activities{Widgets: &widgets.Controller{Repo: repo, Transport: r.Telegram}}
			for attempt := range 2 {
				if err := r.DispatchOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if attempt == 0 && panel == "populated" {
					task, _, _ := repo.Task(ctx, conv.ID, "nba")
					task.Status = "cancelled"
					if _, err := repo.UpdateTask(ctx, task); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(client.events) != 2 || !client.events[0].Stop || !client.events[1].Stop {
				t.Fatalf("cooldown held up stop: %+v", client.events)
			}
			if requests != 1 {
				t.Fatal("cooldown retried HTTP")
			}
			incoming, err := repo.PendingIncoming(ctx, 10)
			if err != nil || len(incoming) != 2 {
				t.Fatalf("pending inputs lost: %+v %v", incoming, err)
			}
		})
	}
}
