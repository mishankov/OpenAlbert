package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jarvis/internal/agent"
	"jarvis/internal/domain"
	"jarvis/internal/integrations"
	"jarvis/internal/storage"
)

type deliveryRepository struct {
	domain.Repository
	states []string
}

func (r *deliveryRepository) SetOutgoingState(ctx context.Context, id, state string) error {
	if err := r.Repository.SetOutgoingState(ctx, id, state); err != nil {
		return err
	}
	r.states = append(r.states, id+":"+state)
	return nil
}
func deliveryRuntime(t *testing.T, handler http.HandlerFunc) (*Runtime, *deliveryRepository, domain.Conversation) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	if err != nil {
		t.Fatal(err)
	}
	repo := &deliveryRepository{Repository: storage.NewMemory()}
	conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
	return &Runtime{Repo: repo, Telegram: tg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, repo, conv
}
func enqueue(t *testing.T, repo domain.Repository, conv domain.Conversation, id, text, artifact string) {
	t.Helper()
	if err := repo.EnqueueOutgoing(context.Background(), domain.Outgoing{ID: id, Conversation: conv, Text: text, ArtifactID: artifact}); err != nil {
		t.Fatal(err)
	}
}
func TestDeliveryUnknownAcknowledgementIsNeverAutomaticallyRepeated(t *testing.T) {
	var count atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) { count.Add(1); _, _ = io.WriteString(w, `{"ok":true,`) })
	enqueue(t, repo, conv, "reply", "answer", "")
	for i := 0; i < 2; i++ {
		if err := r.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(repo.states, ","); got != "reply:sending,reply:unknown" {
		t.Fatalf("unexpected delivery states%s", got)
	}
	if count.Load() != 1 {
		t.Fatal("unknown delivery automatically repeated")
	}
}
func TestDeliveryExplicitRejectionIsFailed(t *testing.T) {
	var count atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":403}`)
	})
	enqueue(t, repo, conv, "reply", "answer", "")
	for i := 0; i < 2; i++ {
		if err := r.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(repo.states, ","); got != "reply:sending,reply:failed" {
		t.Fatalf("unexpected delivery states%s", got)
	}
	if count.Load() != 1 {
		t.Fatal("rejected delivery repeated")
	}
}
func TestDeliveryRateLimitRetainsPendingAndRetriesSafely(t *testing.T) {
	var count atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	enqueue(t, repo, conv, "reply", "answer", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.DeliverOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rate-limit wait did not observe shutdown: %v", err)
	}
	pending, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("rate-limited message lost %+v %v", pending, err)
	}
	if err := r.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(repo.states, ","); got != "reply:sending,reply:pending,reply:sending,reply:sent" {
		t.Fatalf("unexpected rate-limit states%s", got)
	}
	if count.Load() != 2 {
		t.Fatalf("wrong retry count%d", count.Load())
	}
}
func TestFileFailureDoesNotRepeatAlreadyDeliveredText(t *testing.T) {
	var texts, files atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "sendRichMessage") {
			texts.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		} else {
			files.Add(1)
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400}`)
		}
	})
	if err := repo.SaveArtifact(context.Background(), domain.Artifact{ID: "document", ConversationID: conv.ID, Name: "result.txt", MIME: "text/plain", Data: []byte("result")}); err != nil {
		t.Fatal(err)
	}
	enqueue(t, repo, conv, "text", "Here is the result", "")
	enqueue(t, repo, conv, "file", "", "document")
	for i := 0; i < 2; i++ {
		if err := r.DeliverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if texts.Load() != 1 || files.Load() != 1 {
		t.Fatalf("partial delivery duplicated: texts%d files%d", texts.Load(), files.Load())
	}
	if got := strings.Join(repo.states, ","); got != "text:sending,text:sent,file:sending,file:failed" {
		t.Fatalf("bad file/text states%s", got)
	}
}

func TestTypingRenewsOnlyWhileUserMessagesArePending(t *testing.T) {
	var calls atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/sendChatAction") {
			t.Errorf("unexpected method %s", req.URL.Path)
		}
		var payload struct {
			ChatID int64  `json:"chat_id"`
			Action string `json:"action"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.ChatID != 7 || payload.Action != "typing" {
			t.Errorf("bad typing payload %+v", payload)
		}
		calls.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	})
	ctx := context.Background()
	now := time.Now()
	next := map[string]time.Time{}
	for _, id := range []string{"one", "two"} {
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv, Text: "hello"}, 1); err != nil {
			t.Fatal(err)
		}
	}
	for _, delta := range []time.Duration{0, time.Second, 4 * time.Second} {
		if err := r.refreshTyping(ctx, now.Add(delta), next); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("typing should renew once per conversation, got %d", calls.Load())
	}
	for _, id := range []string{"one", "two"} {
		if err := repo.MarkIncomingHandled(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.refreshTyping(ctx, now.Add(8*time.Second), next); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(next) != 0 {
		t.Fatal("typing continued after reply completed")
	}
}

func TestTypingFailureDoesNotFailProcessingAndRespectsRateLimit(t *testing.T) {
	var calls atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":12}}`)
	})
	ctx := context.Background()
	_, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "request", Conversation: conv, Text: "hello"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	next := map[string]time.Time{}
	for _, delta := range []time.Duration{0, 4 * time.Second, 11 * time.Second, 12 * time.Second} {
		if err := r.refreshTyping(ctx, now.Add(delta), next); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("typing rate limit ignored: %d", calls.Load())
	}
	pending, err := repo.PendingIncoming(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatal("typing failure changed incoming processing state")
	}
}

func TestDeliveryPreservesAlreadyRenderedCodeEntities(t *testing.T) {
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		var payload struct {
			Text        string              `json:"text"`
			Entities    []domain.TextEntity `json:"entities"`
			ParseMode   string              `json:"parse_mode"`
			RichMessage map[string]string   `json:"rich_message"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.RichMessage["html"] != "<pre>**literal**</pre>" || payload.Text != "" || len(payload.Entities) != 0 {
			t.Errorf("formatted code parsed twice: %+v", payload)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	ctx := context.Background()
	if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "formatted", Conversation: conv, Text: "**literal**", Entities: []domain.TextEntity{{Type: "pre", Length: 11}}, Formatted: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(repo.states, ","); got != "formatted:sending,formatted:sent" {
		t.Fatalf("bad delivery states %s", got)
	}
}

func TestOwnerGroupPostUsesRichTelegramTopicAndIsNotDeliveredTwice(t *testing.T) {
	ctx := context.Background()
	a, repo, _, owner := newFixture(t)
	a.OwnerUserID = owner.OwnerID
	a.AllowedGroupIDs = []int64{-20}
	group := domain.Conversation{ID: "group-rich-topic", ChatID: -20, ThreadID: 3, OwnerID: 8}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "topic-seed", Conversation: group, Text: "hello"}, 2); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if !strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Errorf("wrong endpoint %s", req.URL.Path)
		}
		var body struct {
			ChatID   int64 `json:"chat_id"`
			ThreadID int   `json:"message_thread_id"`
			Rich     struct {
				Markdown string `json:"markdown"`
			} `json:"rich_message"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.ChatID != -20 || body.ThreadID != 3 || body.Rich.Markdown != "**Публичный текст**" {
			t.Errorf("wrong public message: %+v", body)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":777}}`)
	}))
	defer server.Close()
	tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"conversation_id":"group-rich-topic","text":"**Публичный текст**"}`)
	for i := 0; i < 2; i++ {
		result, err := a.ToolCall(ctx, agent.ToolInput{RunKey: "group-post", SessionID: "private-source", Conversation: owner, Call: domain.ToolCall{ID: "post", Name: "send_group_message", Arguments: args}})
		if err != nil || strings.Contains(result, `"error"`) {
			t.Fatalf("post failed: %s %v", result, err)
		}
	}
	r := &Runtime{Repo: repo, Telegram: tg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i := 0; i < 2; i++ {
		if err := r.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("delivered %d times", requests.Load())
	}
	pending, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("delivery did not leave the pending queue")
	}
}
