package storage

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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

func TestPostgresCodingSyncNULSanitizationAndLegacyCheckpointIsolation(t *testing.T) {
	dsn := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OPENALBERT_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed a personal session with a legacy model checkpoint under turn key "shared-legacy-key".
	if err := store.SaveCheckpoint(ctx, "model/shared-legacy-key/0", json.RawMessage(`{"secret":"personal-only"}`)); err != nil {
		t.Fatal(err)
	}

	// Sync a coding session containing a NUL byte AND a turn-done checkpoint reusing "shared-legacy-key".
	sessID := "coding-pg-nul-" + time.Now().UTC().Format("20060102150405.000000000")
	err = store.SyncCodingSession(ctx, domain.CodingSyncBatch{
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
