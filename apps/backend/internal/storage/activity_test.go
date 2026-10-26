package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestConversationActivityRepositoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { activityContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL to a disposable database")
		}
		repo, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer repo.Close()
		activityContract(t, repo)
	})
}
func activityContract(t *testing.T, repo domain.Repository) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprintf("activity-%d", time.Now().UnixNano())
	scope := nonce + "-owner"
	other := nonce + "-other"
	owner := domain.Conversation{ID: scope, Private: true, OwnerID: 7}
	check := func(state, text string) {
		t.Helper()
		activity, err := repo.ConversationActivity(ctx, scope)
		if err != nil || activity.State != state || activity.Text != text {
			t.Fatalf("activity %+v, want %s %q: %v", activity, state, text, err)
		}
	}
	check("idle", "")
	_, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: nonce + "foreign-input", Conversation: domain.Conversation{ID: other}, Text: "foreign private message"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.StartProgress(ctx, domain.Progress{TurnID: nonce + "foreign-turn", Conversation: domain.Conversation{ID: other}, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = repo.ReportProgress(ctx, nonce+"foreign-turn", 1, "FOREIGN_PROGRESS"); err != nil {
		t.Fatal(err)
	}
	check("idle", "")
	for index, terminal := range []string{"waiting_delivery", "failed", "cancelled"} {
		id := fmt.Sprintf("%s-input-%d", nonce, index)
		turn := fmt.Sprintf("%s-turn-%d", nonce, index)
		if _, err = repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: owner, Text: "hello"}, 0); err != nil {
			t.Fatal(err)
		}
		check("queued", queuedActivityText)
		if err = repo.StartProgress(ctx, domain.Progress{TurnID: turn, Conversation: owner, StartedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		// This is immediate, before the Telegram 30-second progress delivery delay.
		check("processing", "Работаю над запросом…")
		if err = repo.ReportProgress(ctx, turn, 2, "Проверяю результат…"); err != nil {
			t.Fatal(err)
		}
		check("processing", "Проверяю результат…")
		// Suppressed or uncertain Telegram status delivery cannot hide real work.
		if _, ok, err := repo.SetProgressDelivery(ctx, turn, 0, domain.ProgressDelivery{State: "disabled"}); err != nil || !ok {
			t.Fatal("disable progress delivery", ok, err)
		}
		check("processing", "Проверяю результат…")
		if err = repo.MarkIncomingHandled(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err = repo.EndProgress(ctx, turn, terminal); err != nil {
			t.Fatal(err)
		}
		check("idle", "")
	}
	// Pick the latest public progress even at fractional-second boundaries, not
	// whichever map entry, mutation order or lexicographic timestamp comes first.
	start := time.Date(2026, time.October, 26, 12, 0, 0, 0, time.UTC)
	for _, item := range []struct {
		id, text string
		at       time.Time
	}{{"z-old", "old stage", start}, {"a-new", "current stage", start.Add(100 * time.Millisecond)}} {
		turn := nonce + item.id
		if err = repo.StartProgress(ctx, domain.Progress{TurnID: turn, Conversation: owner, StartedAt: item.at}); err != nil {
			t.Fatal(err)
		}
		if err = repo.ReportProgress(ctx, turn, 1, item.text); err != nil {
			t.Fatal(err)
		}
	}
	check("processing", "current stage")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = repo.ConversationActivity(cancelled, scope); err == nil {
		t.Fatal("cancelled activity query succeeded")
	}
}
