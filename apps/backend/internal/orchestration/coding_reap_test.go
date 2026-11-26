package orchestration

import (
	"context"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

func TestReapStaleCodingSessionsRespectsConfigAndQueuesAnalysis(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMemory()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	seedCoding := func(id string, createdAt time.Time) {
		t.Helper()
		if err := repo.SaveConversation(ctx, domain.Conversation{ID: "coding:" + id, Title: id, OwnerID: 7, Private: true}); err != nil {
			t.Fatal(err)
		}
		if err := repo.StartSession(ctx, domain.Session{
			ID:             id,
			ConversationID: "coding:" + id,
			Status:         "active",
			Trigger:        "coding",
			CreatedAt:      createdAt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Stale empty coding session (30h old) and stale coding session with old trace (26h old).
	seedCoding("orphan-empty", now.Add(-30*time.Hour))
	seedCoding("orphan-old-trace", now.Add(-48*time.Hour))
	oldEnd := now.Add(-26 * time.Hour)
	if err := repo.SaveTrace(ctx, domain.Trace{
		ID:        "tr-old",
		SessionID: "orphan-old-trace",
		TurnID:    "t1",
		Trigger:   "coding",
		Kind:      "model",
		Name:      "gpt",
		Status:    "completed",
		StartedAt: now.Add(-27 * time.Hour),
		EndedAt:   &oldEnd,
	}); err != nil {
		t.Fatal(err)
	}

	// Genuinely active coding sessions: one recently created (2h old), one old but with recent trace (30m old).
	seedCoding("fresh-coding", now.Add(-2*time.Hour))
	seedCoding("long-lived-active", now.Add(-72*time.Hour))
	if err := repo.SaveTrace(ctx, domain.Trace{
		ID:        "tr-recent",
		SessionID: "long-lived-active",
		TurnID:    "t2",
		Trigger:   "coding",
		Kind:      "model",
		Name:      "gpt",
		Status:    "running",
		StartedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	// Old active personal session must NEVER be closed by coding reaper.
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartSession(ctx, domain.Session{
		ID:             "personal-active",
		ConversationID: "personal:7",
		Status:         "active",
		CreatedAt:      now.Add(-72 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Disabled (0) must do nothing.
	disabled := &Runtime{Repo: repo, CodingSessionStaleTimeout: 0}
	if err := disabled.ReapStaleCodingSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := repo.Session(ctx, "orphan-empty"); s.Status != "active" {
		t.Fatalf("disabled reaper closed session: %+v", s)
	}

	// Enabled with conservative 24h threshold closes only the two orphaned coding sessions.
	rt := &Runtime{Repo: repo, CodingSessionStaleTimeout: 24 * time.Hour}
	if err := rt.ReapStaleCodingSessions(ctx, now); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"orphan-empty", "orphan-old-trace"} {
		s, ok, err := repo.Session(ctx, id)
		if err != nil || !ok || s.Status != "closed" {
			t.Fatalf("expected orphaned session %s to be closed, got %+v ok=%v err=%v", id, s, ok, err)
		}
	}
	for _, id := range []string{"fresh-coding", "long-lived-active", "personal-active"} {
		s, ok, err := repo.Session(ctx, id)
		if err != nil || !ok || s.Status != "active" {
			t.Fatalf("expected live/non-coding session %s to stay active, got %+v ok=%v err=%v", id, s, ok, err)
		}
	}

	// Closed coding sessions should flow into normal post-session analysis queue.
	pending, err := repo.PendingAnalysis(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 reaped coding sessions queued for analysis, got %+v", pending)
	}
}
