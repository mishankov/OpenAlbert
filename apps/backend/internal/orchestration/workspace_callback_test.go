package orchestration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/storage"
)

func TestRuntimePollRoutesCallbackQueriesToCallbackRouter(t *testing.T) {
	var mu sync.Mutex
	var answered []string
	polled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/botsecret/") {
		case "getUpdates":
			mu.Lock()
			alreadyPolled := polled
			polled = true
			mu.Unlock()
			if alreadyPolled {
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[
				{"update_id":100,"callback_query":{"id":"cb-t3","from":{"id":7},"message":{"message_id":10,"message_thread_id":5,"is_topic_message":true,"chat":{"id":-100555,"type":"supergroup"}},"data":"t3:approve:run-1"}},
				{"update_id":101,"callback_query":{"id":"cb-denied","from":{"id":9},"message":{"message_id":10,"message_thread_id":5,"is_topic_message":true,"chat":{"id":-100555,"type":"supergroup"}},"data":"t3:approve:run-1"}}
			]}`)
		case "answerCallbackQuery":
			var body struct {
				ID   string `json:"callback_query_id"`
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			answered = append(answered, body.ID+":"+body.Text)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	tg, err := integrations.NewTelegram(integrations.TelegramConfig{
		Token:            "secret",
		BaseURL:          server.URL,
		OwnerUserID:      7,
		AllowedUserIDs:   []int64{7, 9},
		WorkspaceGroupID: -100555,
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := storage.NewMemory()
	router := integrations.NewCallbackRouter(7, -100555, tg.AnswerCallbackQuery)
	handledCh := make(chan domain.CallbackQuery, 2)
	router.RegisterPrefixFunc("t3:", func(_ context.Context, q domain.CallbackQuery) (domain.CallbackResult, error) {
		handledCh <- q
		return domain.CallbackResult{Text: "Approved"}, nil
	})
	rt := &Runtime{
		Repo:       repo,
		Activities: &agent.Activities{OwnerUserID: 7, WorkspaceGroupID: -100555, Repo: repo},
		Telegram:   tg,
		Callbacks:  router,
	}

	ctx, cancel := context.WithCancel(context.Background())
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		rt.poll(ctx)
	}()

	select {
	case q := <-handledCh:
		if q.ID != "cb-t3" || q.ThreadID != 5 || q.UserID != 7 {
			t.Fatalf("unexpected callback dispatched: %+v", q)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for callback dispatch")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(answered)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	rt.Wait()

	if len(handledCh) != 0 {
		t.Fatal("non-owner callback must not invoke handler")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 2 {
		t.Fatalf("expected 2 answered callbacks, got %v", answered)
	}
	joined := strings.Join(answered, ",")
	if !strings.Contains(joined, "cb-t3:Approved") || !strings.Contains(joined, "cb-denied:Unauthorized") {
		t.Fatalf("unexpected callback answers: %v", answered)
	}
}
