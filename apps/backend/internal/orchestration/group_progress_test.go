package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/storage"
)

func TestGroupsNeverPostProgressAndCleanUpLegacyStatuses(t *testing.T) {
	for _, topic := range []int{0, 3} {
		for _, mode := range []string{"new", "legacy pending", "legacy visible", "legacy delete retry"} {
			t.Run(fmt.Sprintf("topic=%d/%s", topic, mode), func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				deletes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if !strings.HasSuffix(req.URL.Path, "/deleteMessage") {
						t.Error("group progress tried to send or edit a message", req.URL.Path)
					}
					var payload struct {
						ChatID    int64 `json:"chat_id"`
						MessageID int64 `json:"message_id"`
					}
					_ = json.NewDecoder(req.Body).Decode(&payload)
					if payload.ChatID != -20 || payload.MessageID != 42 {
						t.Error("cleanup targeted another chat or message", payload)
					}
					deletes++
					if mode == "legacy delete retry" && deletes == 1 {
						w.WriteHeader(500)
						_, _ = io.WriteString(w, `{"ok":false,"error_code":500}`)
						return
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
				}))
				defer server.Close()
				tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
				if err != nil {
					t.Fatal(err)
				}
				repo := storage.NewMemory()
				conv := domain.Conversation{ID: "group", ChatID: -20, OwnerID: 7, ThreadID: topic}
				if err := repo.StartProgress(ctx, domain.Progress{TurnID: "turn", Conversation: conv, StartedAt: now.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
				if mode != "new" {
					// Simulate a row persisted by the previous version.
					next := domain.ProgressDelivery{State: "ready"}
					if mode != "legacy pending" {
						next.MessageID = "42"
					}
					if _, ok, err := repo.SetProgressDelivery(ctx, "turn", 0, next); err != nil || !ok {
						t.Fatal("legacy setup", err)
					}
				}
				for _, delta := range []time.Duration{0, 20 * time.Second, 2 * time.Minute} {
					// A restart must not re-enable group statuses or lose cleanup retries.
					r := &Runtime{Repo: repo, Telegram: tg}
					if err := repo.ReportProgress(ctx, "turn", 1, "Checking details"); err != nil {
						t.Fatal(err)
					}
					if err := r.ProgressOnce(ctx, now.Add(delta)); err != nil {
						t.Fatal(err)
					}
				}
				if err := repo.EndProgress(ctx, "turn", "failed"); err != nil {
					t.Fatal(err)
				}
				if err := (&Runtime{Repo: repo, Telegram: tg}).ProgressOnce(ctx, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				want := 0
				if mode == "legacy visible" {
					want = 1
				} else if mode == "legacy delete retry" {
					want = 2
				}
				if deletes != want {
					t.Fatalf("cleanup calls: got %d, want %d", deletes, want)
				}
				if rows, err := repo.DueProgress(ctx, now.Add(2*time.Hour), 10); err != nil || len(rows) != 0 {
					t.Fatal("group status remained pending", rows, err)
				}
			})
		}
	}
}
