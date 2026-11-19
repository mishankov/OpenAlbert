package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

type interruptedDispatchRepo struct{ domain.Repository }

func (r interruptedDispatchRepo) MarkIncomingHandled(ctx context.Context, id string) error {
	// Like PostgreSQL, reject writes after shutdown instead of changing the
	// in-memory inbox and returning the cancellation afterward.
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Repository.MarkIncomingHandled(ctx, id)
}

func TestWidgetCommandAcknowledgementExpiresAfterInterruption(t *testing.T) {
	ctx := context.Background()
	sending := make(chan struct{})
	var acknowledgements, typing atomic.Int32
	r, memory, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/sendMessage"):
			if acknowledgements.Add(1) != 1 {
				t.Error("interrupted acknowledgement was resent")
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			close(sending)
			<-req.Context().Done() // Telegram received the send, but its result was lost.
		case strings.HasSuffix(req.URL.Path, "/sendChatAction"):
			typing.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected method: %s", req.URL.Path)
		}
	})
	repo := interruptedDispatchRepo{memory}
	r.Repo = repo
	r.Activities = &agent.Activities{Widgets: &widgets.Controller{Repo: repo, Transport: r.Telegram}}
	task, err := repo.CreateTask(ctx, domain.Task{ID: "nba", ConversationID: conv.ID, Title: "НБА", Goal: "Scores", Status: "waiting", ScheduleSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	in := domain.Incoming{ID: "pause-without-ack", Conversation: conv, Text: "/pause " + task.ID, Command: "pause"}
	if _, err := repo.AcceptIncoming(ctx, in, 1); err != nil {
		t.Fatal(err)
	}
	assertInbox := func(want int) {
		t.Helper()
		pending, err := repo.PendingIncoming(ctx, 10)
		if err != nil || len(pending) != want {
			t.Fatalf("inbox has %d items, want %d (error %v)", len(pending), want, err)
		}
	}
	shutdownCtx, interrupt := context.WithCancel(ctx)
	defer interrupt()
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- r.DispatchOnce(shutdownCtx) }()
	select {
	case <-sending:
	case err := <-done:
		t.Fatalf("dispatch exited before attempting the acknowledgement: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("acknowledgement never reached Telegram")
	}
	claim := "widget-command-send/" + conv.ID + "/" + in.ID + "/claim"
	raw, found, err := repo.LoadCheckpoint(ctx, claim)
	if err != nil || !found {
		t.Fatalf("sending marker was not persisted before HTTP: found=%v err=%v", found, err)
	}
	var deadline time.Time
	if err := json.Unmarshal(raw, &deadline); err != nil || deadline.Before(started.Add(time.Minute)) || deadline.After(time.Now().Add(time.Minute)) {
		t.Fatalf("sending marker did not record a one-minute deadline: %s (error %v)", raw, err)
	}
	if err := r.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertInbox(1) // A live overlapping attempt can still record a result.
	interrupt()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("dispatch did not observe shutdown: %v", err)
	}
	assertInbox(1)

	var logs bytes.Buffer
	restarted := &Runtime{
		Repo: repo, Telegram: r.Telegram, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Activities: &agent.Activities{Widgets: &widgets.Controller{Repo: repo, Transport: r.Telegram}},
	}
	now := time.Now()
	nextTyping := map[string]time.Time{}
	if err := restarted.refreshTyping(ctx, now, nextTyping); err != nil {
		t.Fatal(err)
	}
	if typing.Load() != 1 {
		t.Fatal("pending inbox did not trigger typing")
	}
	aged, err := json.Marshal(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(ctx, claim, aged); err != nil {
		t.Fatal(err)
	}
	if err := restarted.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertInbox(0)
	if !strings.Contains(logs.String(), "operation=telegram.widget_command") || !strings.Contains(logs.String(), "error_code=delivery_unknown") || !strings.Contains(logs.String(), "incoming_id="+in.ID) {
		t.Fatalf("missing unknown-delivery diagnostic: %s", &logs)
	}
	if err := restarted.refreshTyping(ctx, now.Add(5*time.Second), nextTyping); err != nil {
		t.Fatal(err)
	}
	if typing.Load() != 1 || len(nextTyping) != 0 || acknowledgements.Load() != 1 {
		t.Fatalf("typing or acknowledgement repeated: typing=%d, scheduled=%v, sends=%d", typing.Load(), nextTyping, acknowledgements.Load())
	}
	paused, found, err := repo.Task(ctx, conv.ID, task.ID)
	if err != nil || !found || paused.Status != "paused" || paused.Version != task.Version+1 {
		t.Fatalf("pause was lost or repeated: %+v err=%v", paused, err)
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
