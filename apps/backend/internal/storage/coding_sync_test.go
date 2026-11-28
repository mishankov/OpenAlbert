package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func TestMemorySyncCodingSessionIsAtomicOnValidationFailure(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()

	// Oversized 4097-byte mutation key must fail without creating conversation or session.
	err := mem.SyncCodingSession(ctx, domain.CodingSyncBatch{
		SessionID:      "coding-atomic-1",
		ConversationID: "coding:coding-atomic-1",
		Title:          "Should Not Appear",
		OwnerID:        42,
		Messages: []domain.CodingSyncMessage{{
			Key:     strings.Repeat("k", 4097),
			Message: domain.Message{Role: "user", Content: "bad key"},
		}},
	})
	if err == nil {
		t.Fatal("expected error for 4097-byte mutation key")
	}
	if _, ok, _ := mem.Session(ctx, "coding-atomic-1"); ok {
		t.Fatal("failed batch created session")
	}
	if _, ok, _ := mem.Conversation(ctx, "coding:coding-atomic-1"); ok {
		t.Fatal("failed batch created conversation")
	}

	// Existing conversation title must not change when a subsequent batch fails validation.
	if err := mem.SaveConversation(ctx, domain.Conversation{
		ID: "coding:coding-atomic-2", Title: "Original Title", OwnerID: 42, Private: true,
	}); err != nil {
		t.Fatal(err)
	}
	err = mem.SyncCodingSession(ctx, domain.CodingSyncBatch{
		SessionID:      "coding-atomic-2",
		ConversationID: "coding:coding-atomic-2",
		Title:          "Mutated Title",
		OwnerID:        42,
		Messages: []domain.CodingSyncMessage{{
			Key:     strings.Repeat("k", 4097),
			Message: domain.Message{Role: "user", Content: "bad key"},
		}},
	})
	if err == nil {
		t.Fatal("expected error for 4097-byte mutation key")
	}
	conv, ok, _ := mem.Conversation(ctx, "coding:coding-atomic-2")
	if !ok || conv.Title != "Original Title" {
		t.Fatalf("failed batch mutated existing conversation title: %+v", conv)
	}
}

func TestSanitizeJSONBStripsNULButPreservesEscapedLiteral(t *testing.T) {
	in := []byte(`{"a":"hi\u0000there","b":"keep\\u0000literal"}`)
	out := string(sanitizeJSONB(in))
	if strings.Contains(out, `"hi\u0000there"`) || !strings.Contains(out, `"hi\ufffdthere"`) {
		t.Fatalf("expected \\u0000 sanitized to \\ufffd, got %s", out)
	}
	if !strings.Contains(out, `"keep\\u0000literal"`) {
		t.Fatalf("escaped \\\\u0000 literal was corrupted: %s", out)
	}
}

func TestSQLiteCodingSyncNULSanitizationAndLegacyCheckpointIsolation(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLite(t)

	// Seed a personal session with a legacy model checkpoint under turn key "shared-legacy-key".
	if err := store.SaveCheckpoint(ctx, "model/shared-legacy-key/0", json.RawMessage(`{"secret":"personal-only"}`)); err != nil {
		t.Fatal(err)
	}

	// Sync a coding session containing a NUL byte AND a turn-done checkpoint reusing "shared-legacy-key".
	sessID := "coding-pg-nul-" + time.Now().UTC().Format("20060102150405.000000000")
	err := store.SyncCodingSession(ctx, domain.CodingSyncBatch{
		SessionID:      sessID,
		ConversationID: "coding:" + sessID,
		Title:          "NUL\x00Test",
		OwnerID:        42,
		Messages: []domain.CodingSyncMessage{{
			Key:     "t1/user",
			Message: domain.Message{Role: "user", Content: "before\x00after"},
		}},
		Traces: []domain.Trace{{
			ID:        "coding-trace-" + sessID,
			SessionID: sessID,
			TurnID:    "t1",
			Trigger:   "coding",
			Kind:      "tool",
			Name:      "bash",
			Status:    "completed",
			StartedAt: time.Now().UTC(),
			Input:     json.RawMessage(`{"cmd":"printf '\\u0000'"}`),
		}},
		CheckpointKey:     "turn-done/coding/" + sessID + "/shared-legacy-key",
		CheckpointPayload: json.RawMessage(`{"Input":{"SessionID":"` + sessID + `","Key":"shared-legacy-key"}}`),
	})
	if err != nil {
		t.Fatalf("SyncCodingSession with NUL byte failed on Postgres: %v", err)
	}

	// InspectSession and InspectTrace must NOT expose the personal session's legacy model checkpoint to the coding session.
	detail, found, err := store.InspectSession(ctx, sessID, 50)
	if err != nil || !found {
		t.Fatalf("InspectSession failed: found=%v err=%v", found, err)
	}
	for _, tr := range detail.Traces {
		if tr.ID == "model/shared-legacy-key/0" {
			t.Fatalf("legacy personal model checkpoint leaked into coding session: %+v", tr)
		}
	}
	if _, leaked, err := store.InspectTrace(ctx, sessID, "model/shared-legacy-key/0"); err != nil || leaked {
		t.Fatalf("InspectTrace leaked personal legacy checkpoint into coding session: leaked=%v err=%v", leaked, err)
	}
}

func closeStaleCodingSessionsContract(t *testing.T, repo domain.Repository) {
	t.Helper()
	ctx := context.Background()
	prefix := "stale-coding-" + time.Now().UTC().Format("20060102150405.000000000") + "-"
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)

	start := func(id, convID string, createdAt time.Time) {
		t.Helper()
		if err := repo.SaveConversation(ctx, domain.Conversation{ID: convID, Title: id, OwnerID: 42, Private: true}); err != nil {
			t.Fatal(err)
		}
		if err := repo.StartSession(ctx, domain.Session{
			ID: id, ConversationID: convID, Status: "active", Trigger: "coding", CreatedAt: createdAt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Orphaned empty coding session (created 30h ago) -> should close.
	idStaleEmpty := prefix + "empty"
	start(idStaleEmpty, "coding:"+idStaleEmpty, now.Add(-30*time.Hour))

	// 2. Old coding session with only an old trace (ended 25h ago) -> should close.
	idStaleTrace := prefix + "old-trace"
	start(idStaleTrace, "coding:"+idStaleTrace, now.Add(-48*time.Hour))
	oldEnd := now.Add(-25 * time.Hour)
	if err := repo.SaveTrace(ctx, domain.Trace{
		ID: prefix + "tr-old", SessionID: idStaleTrace, TurnID: "t1", Trigger: "coding",
		Kind: "model", Name: "gpt", Status: "completed", StartedAt: now.Add(-26 * time.Hour), EndedAt: &oldEnd,
	}); err != nil {
		t.Fatal(err)
	}

	// 3. Recently created empty coding session (2h ago) -> must stay active.
	idFresh := prefix + "fresh"
	start(idFresh, "coding:"+idFresh, now.Add(-2*time.Hour))

	// 4. Old coding session with recent synced message -> must stay active.
	idRecentMsg := prefix + "recent-msg"
	start(idRecentMsg, "coding:"+idRecentMsg, now.Add(-48*time.Hour))
	if err := repo.AppendMessage(ctx, idRecentMsg, "m1", domain.Message{Role: "user", Content: "still working"}); err != nil {
		t.Fatal(err)
	}

	// 5. Old coding session with recent trace ended 1h ago -> must stay active.
	idRecentTrace := prefix + "recent-trace"
	start(idRecentTrace, "coding:"+idRecentTrace, now.Add(-48*time.Hour))
	recentEnd := now.Add(-1 * time.Hour)
	if err := repo.SaveTrace(ctx, domain.Trace{
		ID: prefix + "tr-recent", SessionID: idRecentTrace, TurnID: "t1", Trigger: "coding",
		Kind: "model", Name: "gpt", Status: "completed", StartedAt: now.Add(-2 * time.Hour), EndedAt: &recentEnd,
	}); err != nil {
		t.Fatal(err)
	}

	// 6. Old active non-coding session -> must NEVER be touched.
	idPersonal := prefix + "personal"
	start(idPersonal, "personal:42", now.Add(-72*time.Hour))

	// First call with limit=1 closes exactly 1 stale session.
	first, err := repo.CloseStaleCodingSessions(ctx, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("limit=1 expected 1 closed session, got %d (%+v)", len(first), first)
	}
	// Second call closes the remaining stale session (and no others).
	second, err := repo.CloseStaleCodingSessions(ctx, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	closedIDs := map[string]bool{}
	for _, s := range append(first, second...) {
		closedIDs[s.ID] = true
	}
	if !closedIDs[idStaleEmpty] || !closedIDs[idStaleTrace] {
		t.Fatalf("expected both stale coding sessions closed, got %+v", closedIDs)
	}
	for _, liveID := range []string{idFresh, idRecentMsg, idRecentTrace, idPersonal} {
		if closedIDs[liveID] {
			t.Fatalf("session %s must not be closed", liveID)
		}
		s, ok, err := repo.Session(ctx, liveID)
		if err != nil || !ok || s.Status != "active" {
			t.Fatalf("expected %s to remain active, got %+v ok=%v err=%v", liveID, s, ok, err)
		}
	}
}

func TestMemoryCloseStaleCodingSessionsContract(t *testing.T) {
	closeStaleCodingSessionsContract(t, NewMemory())
}

func TestSQLiteCloseStaleCodingSessionsContract(t *testing.T) {
	closeStaleCodingSessionsContract(t, openTestSQLite(t))
}

func TestSQLiteCloseStaleCodingSessionsSerializesWithConcurrentTurnStart(t *testing.T) {
	ctx := context.Background()
	store := openTestSQLite(t)

	basePrefix := "stale-race-" + time.Now().UTC().Format("20060102150405.000000000") + "-"
	for i := 0; i < 12; i++ {
		now := time.Now().UTC()
		cutoff := now.Add(-24 * time.Hour)
		sessID := fmt.Sprintf("%s%d", basePrefix, i)
		convID := "coding:" + sessID
		traceID := "turn-start-" + sessID
		if err := store.SaveConversation(ctx, domain.Conversation{ID: convID, Title: sessID, OwnerID: 42, Private: true}); err != nil {
			t.Fatal(err)
		}
		if err := store.StartSession(ctx, domain.Session{
			ID: sessID, ConversationID: convID, Status: "active", Trigger: "coding", CreatedAt: now.Add(-30 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		startGate := make(chan struct{})
		var (
			wg      sync.WaitGroup
			syncErr error
			reapErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-startGate
			syncErr = store.SyncCodingSession(ctx, domain.CodingSyncBatch{
				SessionID:      sessID,
				ConversationID: convID,
				OwnerID:        42,
				Traces: []domain.Trace{{
					ID:        traceID,
					SessionID: sessID,
					TurnID:    "t1",
					Trigger:   "coding",
					Kind:      "model",
					Name:      "coding",
					Status:    "running",
					StartedAt: now,
				}},
			})
		}()
		go func() {
			defer wg.Done()
			<-startGate
			_, reapErr = store.CloseStaleCodingSessions(ctx, cutoff, 50)
		}()
		close(startGate)
		wg.Wait()
		if reapErr != nil {
			t.Fatalf("iter %d: CloseStaleCodingSessions failed: %v", i, reapErr)
		}

		sess, ok, err := store.Session(ctx, sessID)
		if err != nil || !ok {
			t.Fatalf("iter %d: Session lookup failed: ok=%v err=%v", i, ok, err)
		}
		_, traceFound, err := store.InspectTrace(ctx, sessID, traceID)
		if err != nil {
			t.Fatalf("iter %d: InspectTrace failed: %v", i, err)
		}
		switch {
		case syncErr == nil:
			if sess.Status != "active" || !traceFound {
				t.Fatalf("iter %d: turn admitted but session=%q traceFound=%v", i, sess.Status, traceFound)
			}
			// A subsequent reap pass must also respect the committed running trace.
			closed, err := store.CloseStaleCodingSessions(ctx, cutoff, 50)
			if err != nil {
				t.Fatalf("iter %d: follow-up CloseStaleCodingSessions failed: %v", i, err)
			}
			for _, c := range closed {
				if c.ID == sessID {
					t.Fatalf("iter %d: follow-up reap closed session with active running trace", i)
				}
			}
		case errors.Is(syncErr, domain.ErrSessionClosed):
			if sess.Status != "closed" || traceFound {
				t.Fatalf("iter %d: turn rejected with ErrSessionClosed but session=%q traceFound=%v", i, sess.Status, traceFound)
			}
		default:
			t.Fatalf("iter %d: unexpected SyncCodingSession error: %v", i, syncErr)
		}
	}
}
