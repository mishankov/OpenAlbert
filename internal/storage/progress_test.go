package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"openalbert/internal/domain"
)

func TestProgressRepositoryContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) { progressContract(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL for PostgreSQL")
		}
		s, err := Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		progressContract(t, s)
	})
}

func progressContract(t *testing.T, repo domain.Repository) {
	ctx := context.Background()
	now := time.Now().UTC()
	id := fmt.Sprintf("progress-%d", now.UnixNano())
	conv := domain.Conversation{ID: id + "/chat", ChatID: 7, OwnerID: 7, Private: true}
	p := domain.Progress{TurnID: id, Conversation: conv, StartedAt: now.Add(-time.Minute)}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(repo.StartProgress(ctx, p))
	check(repo.ReportProgress(ctx, id, 3, "current"))
	check(repo.StartProgress(ctx, p)) // Retried Prepare must not erase public text or delivery metadata.
	check(repo.ReportProgress(ctx, id, 1, "stale"))
	find := func(want string) *domain.Progress {
		t.Helper()
		rows, err := repo.DueProgress(ctx, now, 1000)
		check(err)
		for _, row := range rows {
			if row.TurnID == want {
				return &row
			}
		}
		return nil
	}
	row := find(id)
	if row == nil || row.Text != "current" {
		t.Fatal("Prepare/retry erased newer progress")
	}
	claimed, ok, err := repo.SetProgressDelivery(ctx, id, row.Delivery.Version, domain.ProgressDelivery{State: "sending"})
	check(err)
	if !ok {
		t.Fatal("could not claim status")
	}
	_, ok, err = repo.SetProgressDelivery(ctx, id, row.Delivery.Version, domain.ProgressDelivery{State: "sending"})
	check(err)
	if ok {
		t.Fatal("two senders claimed one creation")
	}
	check(repo.ReportProgress(ctx, id, 4, "new while sending"))
	check(repo.EndProgress(ctx, id, "cancelled"))
	check(repo.EndProgress(ctx, id, "failed"))
	check(repo.ReportProgress(ctx, id, 5, "late update"))
	saved, ok, err := repo.SetProgressDelivery(ctx, id, claimed.Delivery.Version, domain.ProgressDelivery{State: "ready", MessageID: "42"})
	check(err)
	if !ok || saved.Text != "new while sending" || saved.Outcome != "cancelled" || saved.Delivery.MessageID != "42" {
		t.Fatalf("concurrent source/delivery updates lost: %+v", saved)
	}
	row = find(id)
	if row == nil || row.Outcome != "cancelled" {
		t.Fatal("stop overwritten by stale updates")
	}
	_, _, err = repo.SetProgressDelivery(ctx, id, row.Delivery.Version, domain.ProgressDelivery{State: "done", MessageID: "42"})
	check(err)
	if find(id) != nil {
		t.Fatal("terminal status is still pending")
	}

	id += "/reply"
	p.TurnID = id
	check(repo.StartProgress(ctx, p))
	other := conv
	other.ID += "/other"
	check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/group", TurnID: id, Conversation: other, Text: "public"}))
	check(repo.SetOutgoingState(ctx, id+"/group", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/group", "sent"))
	if find(id) == nil {
		t.Fatal("cross-chat post suppressed private progress")
	}
	for _, part := range []string{"text", "file"} {
		check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/" + part, TurnID: id, Conversation: conv, Text: "reply"}))
	}
	if find(id) != nil {
		t.Fatal("created a status after final reply was queued")
	}
	check(repo.EndProgress(ctx, id, "waiting_delivery"))
	check(repo.SetOutgoingState(ctx, id+"/text", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/text", "sent"))
	if find(id) != nil {
		t.Fatal("partial reply removed progress")
	}
	check(repo.SetOutgoingState(ctx, id+"/file", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/file", "sent"))
	row = find(id)
	if row == nil || row.Outcome != "delivered" {
		t.Fatal("delivered answer not ready for cleanup")
	}
	check(repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id + "/unknown", TurnID: id, Conversation: conv, Text: "extra part"}))
	check(repo.SetOutgoingState(ctx, id+"/unknown", "sending"))
	check(repo.SetOutgoingState(ctx, id+"/unknown", "unknown"))
	row = find(id)
	if row == nil || row.Outcome != "delivery_failed" {
		t.Fatal("unknown delivery treated as success")
	}

}

func TestPostgresProgressRestartRecovery(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL for PostgreSQL")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	prefix := fmt.Sprintf("restart-%d", now.UnixNano())
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"creating", "editing"} {
		p := domain.Progress{TurnID: prefix + id, Conversation: domain.Conversation{ID: prefix}, StartedAt: now.Add(-time.Minute)}
		if err = s.StartProgress(ctx, p); err != nil {
			t.Fatal(err)
		}
		delivery := domain.ProgressDelivery{State: "sending"}
		if id == "editing" {
			delivery.MessageID = "42"
		}
		if _, _, err = s.SetProgressDelivery(ctx, p.TurnID, 0, delivery); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"creating", "editing"} {
		var state, messageID string
		if err = s.pool.QueryRow(ctx, `SELECT data->'delivery'->>'state',data->'delivery'->>'message_id' FROM openalbert_progress WHERE id=$1`, prefix+id).Scan(&state, &messageID); err != nil {
			t.Fatal(err)
		}
		if id == "creating" && state != "disabled" {
			t.Fatal("ambiguous creation retried after restart")
		}
		if id == "editing" && (state != "ready" || messageID != "42") {
			t.Fatal("saved status ID lost after restart")
		}
	}
}
