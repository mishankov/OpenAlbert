package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopDeliveryNeverContactsTelegram(t *testing.T) {
	var requests atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) { requests.Add(1); t.Error("desktop contacted Telegram") })
	ctx := context.Background()
	conv.DeliveryChannel = "desktop"
	enqueue(t, repo, conv, "desktop", "**answer**", "")
	if err := r.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("desktop contacted Telegram")
	}
	history, err := repo.PublicTimeline(ctx, conv.ID, 0, 10)
	if err != nil || len(history) != 1 || history[0].Outgoing.State != "sent" {
		t.Fatal("desktop outbox not delivered", history, err)
	}
	telegramConv := conv
	telegramConv.DeliveryChannel = ""
	if err = repo.StartProgress(ctx, domain.Progress{TurnID: "desktop-progress", Conversation: telegramConv, StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = repo.StartProgress(ctx, domain.Progress{TurnID: "desktop-progress", Conversation: conv, StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = r.ProgressOnce(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("desktop progress contacted Telegram")
	}
}

func TestDesktopSteeringSuppressesTelegramTyping(t *testing.T) {
	var requests atomic.Int32
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		t.Error("desktop steering contacted Telegram typing")
	})
	ctx := context.Background()
	for _, in := range []domain.Incoming{{ID: "telegram-first", Conversation: conv}, {ID: "desktop-last", Source: "desktop", Conversation: conv}} {
		if _, err := repo.AcceptIncoming(ctx, in, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.refreshTyping(ctx, time.Now(), map[string]time.Time{}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("desktop typing contacted Telegram")
	}
}

func TestProgressRouteChangesCleanUpAndResume(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			var methods []string
			var ids []int64
			var sends int
			r, _, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
				var body struct {
					MessageID int64 `json:"message_id"`
				}
				_ = json.NewDecoder(req.Body).Decode(&body)
				methods = append(methods, method)
				ids = append(ids, body.MessageID)
				if method == "sendMessage" {
					sends++
				}
				_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, 41+sends)
			})
			var repo domain.Repository = storage.NewMemory()
			if backend == "postgres" {
				url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
				if url == "" {
					t.Skip("set OPENALBERT_TEST_DATABASE_URL")
				}
				s, err := storage.Open(ctx, url)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				repo = s
			}
			conv.ID = fmt.Sprintf("mixed-route-%d", now.UnixNano())
			r.Repo = singleTurnProgressRepo{Repository: repo, turnID: conv.ID}
			turn := conv.ID
			start := func(desktop bool) {
				t.Helper()
				c := conv
				if desktop {
					c.DeliveryChannel = "desktop"
				}
				if err := repo.StartProgress(ctx, domain.Progress{TurnID: turn, Conversation: c, StartedAt: now.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			tick := func(d time.Duration) {
				t.Helper()
				if err := r.ProgressOnce(ctx, now.Add(d)); err != nil {
					t.Fatal(err)
				}
			}
			start(false)
			tick(0)
			start(true)
			tick(time.Second)
			tick(2 * time.Second)
			if got := strings.Join(methods, ","); got != "sendMessage,deleteMessage" || ids[1] != 42 {
				t.Fatal("desktop switch failed to remove existing status", methods, ids)
			}
			start(false)
			tick(3 * time.Second)
			if err := repo.ReportProgress(ctx, turn, 1, "Следующий этап"); err != nil {
				t.Fatal(err)
			}
			tick(64 * time.Second)
			if got := strings.Join(methods, ","); got != "sendMessage,deleteMessage,sendMessage,editMessageText" || ids[3] != 43 {
				t.Fatal("Telegram switch did not resume status updates", methods, ids)
			}
		})
	}
}

func TestUnknownProgressCreationStaysDisabledAcrossRouteChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	calls := 0
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) { calls++; _, _ = w.Write([]byte(`{"ok":true,`)) })
	start := func(desktop bool) {
		t.Helper()
		c := conv
		if desktop {
			c.DeliveryChannel = "desktop"
		}
		if err := repo.StartProgress(ctx, domain.Progress{TurnID: "unknown-route", Conversation: c, StartedAt: now.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	start(false)
	if err := r.ProgressOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, desktop := range []bool{true, false, true, false} {
		start(desktop)
		if err := r.ProgressOnce(ctx, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("ambiguous first status send was recreated after route switch", calls)
	}
}

func TestProgressCleanupRetryFinishesBeforeResumingTelegram(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	var methods []string
	deletes := 0
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		methods = append(methods, method)
		if method == "deleteMessage" {
			deletes++
			if deletes == 1 {
				_, _ = w.Write([]byte(`{"ok":true,`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	})
	start := func(desktop bool) {
		t.Helper()
		c := conv
		if desktop {
			c.DeliveryChannel = "desktop"
		}
		if err := repo.StartProgress(ctx, domain.Progress{TurnID: "cleanup-route", Conversation: c, StartedAt: now.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	tick := func(d time.Duration) {
		t.Helper()
		if err := r.ProgressOnce(ctx, now.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	start(false)
	tick(0)
	start(true)
	tick(time.Second)
	start(false)
	tick(2 * time.Second)
	tick(16 * time.Second)
	tick(17 * time.Second)
	if got := strings.Join(methods, ","); got != "sendMessage,deleteMessage,deleteMessage,sendMessage" {
		t.Fatal("cleanup retry lost across route switch", methods)
	}
}

// The disposable integration database is shared with storage contract tests.
// A runtime test must not deliver those unrelated turns' progress messages.
type singleTurnProgressRepo struct {
	domain.Repository
	turnID string
}

func (r singleTurnProgressRepo) DueProgress(ctx context.Context, now time.Time, n int) ([]domain.Progress, error) {
	rows, err := r.Repository.DueProgress(ctx, now, 1000)
	if err != nil {
		return nil, err
	}
	out := []domain.Progress{}
	for _, p := range rows {
		if p.TurnID == r.turnID {
			out = append(out, p)
		}
	}
	return out, nil
}
