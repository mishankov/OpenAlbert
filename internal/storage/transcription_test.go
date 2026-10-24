package storage

import (
	"context"
	"fmt"
	"openalbert/internal/domain"
	"os"
	"testing"
	"time"
)

func transcriptionContract(t *testing.T, repo domain.Repository) {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("speech-%d", time.Now().UnixNano())
	_, cached, claimed, e := repo.ClaimTranscription(ctx, key, "a")
	if e != nil || cached || !claimed {
		t.Fatalf("first claim %v", e)
	}
	_, _, claimed, e = repo.ClaimTranscription(ctx, key, "b")
	if e != nil || claimed {
		t.Fatal("competing owner acquired lease")
	}
	if ok, e := repo.CompleteTranscription(ctx, key, "b", domain.SpeechText{Text: "wrong"}); e != nil || ok {
		t.Fatal("stale owner committed")
	}
	if e = repo.ReleaseTranscription(ctx, key, "a"); e != nil {
		t.Fatal(e)
	}
	_, _, claimed, e = repo.ClaimTranscription(ctx, key, "b")
	if e != nil || !claimed {
		t.Fatal("released lease unavailable")
	}
	if ok, e := repo.CompleteTranscription(ctx, key, "a", domain.SpeechText{Text: "stale"}); e != nil || ok {
		t.Fatal("old owner committed after takeover")
	}
	if ok, e := repo.CompleteTranscription(ctx, key, "b", domain.SpeechText{Text: "Правильно"}); e != nil || !ok {
		t.Fatal("owner could not commit")
	}
	r, cached, claimed, e := repo.ClaimTranscription(ctx, key, "c")
	if e != nil || !cached || claimed || r.Text != "Правильно" {
		t.Fatalf("cache %+v %v", r, e)
	}
}
func TestTranscriptionLeaseMemory(t *testing.T) { transcriptionContract(t, NewMemory()) }
func TestTranscriptionLeasePostgres(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("disposable PostgreSQL only")
	}
	repo, e := Open(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	defer repo.Close()
	transcriptionContract(t, repo)
}
