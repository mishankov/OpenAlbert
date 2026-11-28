package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

func TestSQLiteLogStoreSearchAndFilters(t *testing.T) {
	ctx := context.Background()
	s := openTestSQLite(t)

	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	// Write events for target conversation and a foreign conversation, plus non-whitelisted secrets.
	s.WriteLog(base.Add(1*time.Minute), "INFO", "oldest event", map[string]any{
		"conversation_id": "personal:7",
		"session_id":      "sess-1",
		"turn_id":         "turn-1",
		"operation":       "turn.run",
		"result":          "ok",
		"duration_ms":     42.5,
		"http_status":     200,
		"retryable":       false,
		"secret_token":    "SHOULD_NEVER_LEAK",
	})
	s.WriteLog(base.Add(2*time.Minute), "ERROR", "Ошибка тула: таймаут", map[string]any{
		"conversation_id": "personal:7",
		"session_id":      "sess-1",
		"turn_id":         "turn-2",
		"task_id":         "task-9",
		"operation":       "tool.exec",
		"tool":            "web_search",
		"result":          "error",
		"error_code":      "timeout",
		"retryable":       true,
		"attempt":         2,
	})
	s.WriteLog(base.Add(3*time.Minute), "ERROR", "Второй таймаут", map[string]any{
		"conversation_id": "personal:7",
		"session_id":      "sess-2",
		"turn_id":         "turn-3",
		"operation":       "tool.exec",
		"tool":            "web_search",
		"result":          "error",
		"error_code":      "timeout",
	})
	s.WriteLog(base.Add(4*time.Minute), "INFO", "foreign private message", map[string]any{
		"conversation_id": "personal:999",
		"session_id":      "sess-foreign",
		"operation":       "turn.run",
		"result":          "ok",
	})
	// Out-of-window event (before Since).
	s.WriteLog(base.Add(-2*time.Hour), "INFO", "too old", map[string]any{
		"conversation_id": "personal:7",
		"operation":       "turn.run",
		"result":          "ok",
	})

	// 1. Scoped search over [base, base+10m]: newest first, foreign & out-of-window excluded, secrets stripped.
	res, err := s.Search(ctx, domain.LogFilter{
		ConversationID: "personal:7",
		Since:          base,
		Until:          base.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.ConversationID != "personal:7" || res.Truncated || res.SummaryTruncated {
		t.Fatalf("unexpected search header: %+v", res)
	}
	if len(res.Events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(res.Events), res.Events)
	}
	if res.Events[0]["_msg"] != "Второй таймаут" || res.Events[1]["_msg"] != "Ошибка тула: таймаут" || res.Events[2]["_msg"] != "oldest event" {
		t.Fatalf("events not newest-first or Unicode lost: %+v", res.Events)
	}
	if res.Events[2]["duration_ms"] != "42.5" || res.Events[2]["http_status"] != "200" || res.Events[2]["retryable"] != "false" {
		t.Fatalf("scalar fields formatted incorrectly: %+v", res.Events[2])
	}
	if _, leaked := res.Events[2]["secret_token"]; leaked {
		t.Fatalf("non-whitelisted field leaked: %+v", res.Events[2])
	}
	if _, err := time.Parse(time.RFC3339Nano, res.Events[0]["_time"]); err != nil {
		t.Fatalf("invalid _time %q: %v", res.Events[0]["_time"], err)
	}
	// Summary sorted by count DESC: tool.exec/error/timeout (2) before turn.run/ok (1).
	if len(res.Summary) != 2 || res.Summary[0].Operation != "tool.exec" || res.Summary[0].Count != 2 || res.Summary[0].ErrorCode != "timeout" || res.Summary[1].Operation != "turn.run" || res.Summary[1].Count != 1 {
		t.Fatalf("unexpected summary: %+v", res.Summary)
	}

	// 2. Exact filter combinations: SessionID, TurnID, Operation, Tool, Result, ErrorCode.
	filtered, err := s.Search(ctx, domain.LogFilter{
		ConversationID: "personal:7",
		SessionID:      "sess-1",
		TurnID:         "turn-2",
		Operation:      "tool.exec",
		Tool:           "web_search",
		Result:         "error",
		ErrorCode:      "timeout",
		Since:          base,
		Until:          base.Add(10 * time.Minute),
	})
	if err != nil || len(filtered.Events) != 1 || filtered.Events[0]["task_id"] != "task-9" || filtered.Events[0]["attempt"] != "2" {
		t.Fatalf("exact filter mismatch: %+v err=%v", filtered, err)
	}

	// 3. Limit truncation.
	limited, err := s.Search(ctx, domain.LogFilter{
		ConversationID: "personal:7",
		Since:          base,
		Until:          base.Add(10 * time.Minute),
		Limit:          2,
	})
	if err != nil || len(limited.Events) != 2 || !limited.Truncated {
		t.Fatalf("expected truncated limit=2, got %+v err=%v", limited, err)
	}
}

func TestSQLiteLogStoreValidationClippingAndCaps(t *testing.T) {
	ctx := context.Background()
	s := openTestSQLite(t)
	now := time.Now().UTC()

	// Validation errors.
	badFilters := []domain.LogFilter{
		{}, // missing conversation_id
		{ConversationID: "personal:1", Limit: -1},
		{ConversationID: "personal:1", Limit: 51},
		{ConversationID: "personal:1", Since: now, Until: now},
		{ConversationID: "personal:1", Since: now.Add(-25 * time.Hour), Until: now},
		{ConversationID: "personal:1", Operation: strings.Repeat("a", 257)},
		{ConversationID: "personal:1", Operation: "bad\nnewline"},
		{ConversationID: "personal:1", Tool: "bad\x00nul"},
	}
	for i, f := range badFilters {
		if _, err := s.Search(ctx, f); err == nil {
			t.Fatalf("case %d: expected validation error for %+v", i, f)
		}
	}

	// Clipping: _msg at 600 runes, other fields at 256 runes.
	longMsg := strings.Repeat("ж", 650)
	longModel := strings.Repeat("m", 300)
	s.WriteLog(now.Add(-time.Minute), "INFO", longMsg, map[string]any{
		"conversation_id": "clip:1",
		"operation":       "model.call",
		"result":          "ok",
		"model":           longModel,
	})
	res, err := s.Search(ctx, domain.LogFilter{
		ConversationID: "clip:1",
		Since:          now.Add(-time.Hour),
		Until:          now,
	})
	if err != nil || len(res.Events) != 1 {
		t.Fatalf("clip search failed: %+v err=%v", res, err)
	}
	if got := []rune(res.Events[0]["_msg"]); len(got) != 601 || got[600] != '…' {
		t.Fatalf("expected _msg clipped to 600 runes + ellipsis, got len=%d", len(got))
	}
	if got := []rune(res.Events[0]["model"]); len(got) != 257 || got[256] != '…' {
		t.Fatalf("expected model clipped to 256 runes + ellipsis, got len=%d", len(got))
	}

	// Summary 100-group cap & 10000-byte cap.
	for i := range 105 {
		s.WriteLog(now.Add(-time.Duration(i+1)*time.Second), "INFO", "summary-cap", map[string]any{
			"conversation_id": "cap-summary:1",
			"operation":       fmt.Sprintf("op-%03d-%s", i, strings.Repeat("x", 80)),
			"result":          "ok",
		})
	}
	sumRes, err := s.Search(ctx, domain.LogFilter{
		ConversationID: "cap-summary:1",
		Since:          now.Add(-time.Hour),
		Until:          now,
		Limit:          1,
	})
	if err != nil {
		t.Fatalf("summary cap search: %v", err)
	}
	if !sumRes.SummaryTruncated || len(sumRes.Summary) > 100 {
		t.Fatalf("expected summary truncated <=100, got len=%d truncated=%v", len(sumRes.Summary), sumRes.SummaryTruncated)
	}

	// Events 24000-byte cap.
	for i := range 50 {
		s.WriteLog(now.Add(-time.Duration(i+1)*time.Second), "INFO", strings.Repeat("a", 590), map[string]any{
			"conversation_id": "cap-events:1",
			"operation":       "turn.run",
			"result":          "ok",
			"model":           strings.Repeat("b", 250),
			"error_reason":    strings.Repeat("c", 250),
		})
	}
	evRes, err := s.Search(ctx, domain.LogFilter{
		ConversationID: "cap-events:1",
		Since:          now.Add(-time.Hour),
		Until:          now,
		Limit:          50,
	})
	if err != nil {
		t.Fatalf("events cap search: %v", err)
	}
	encoded, _ := json.Marshal(evRes)
	if !evRes.Truncated || len(encoded) > 24000 || len(evRes.Events) >= 50 {
		t.Fatalf("expected events capped under 24000 bytes, got bytes=%d len=%d truncated=%v", len(encoded), len(evRes.Events), evRes.Truncated)
	}
}

func TestSQLiteLogStorePruningAndStartupRetention(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "logs-retention.db")
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.WriteLog(now.Add(-31*24*time.Hour), "INFO", "expired-31d", map[string]any{"conversation_id": "ret:1", "operation": "old"})
	s.WriteLog(now.Add(-29*24*time.Hour), "INFO", "kept-29d", map[string]any{"conversation_id": "ret:1", "operation": "recent"})
	s.WriteLog(now.Add(-5*time.Minute), "INFO", "kept-5m", map[string]any{"conversation_id": "ret:1", "operation": "fresh"})
	s.Close()

	// Reopening prunes logs older than 30 days automatically.
	s, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var remaining int
	if err := s.queryRow(ctx, "SELECT COUNT(*) FROM openalbert_logs WHERE conversation_id = 'ret:1'").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("expected 2 logs remaining after 30d startup prune, got %d", remaining)
	}
	if err := s.PruneLogs(ctx, now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, "SELECT COUNT(*) FROM openalbert_logs WHERE conversation_id = 'ret:1'").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("expected 1 log remaining after explicit PruneLogs, got %d", remaining)
	}
}

func TestSQLiteWriteLogDoesNotRecurseIntoTelemetryAndIsRaceSafeOnClose(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "logs-race.db")
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// WriteLog must not invoke sqlTracer or slog even when an event is active.
	var out bytes.Buffer
	ev := telemetry.Start(ctx, telemetry.New(&out), "test.noloop")
	s.WriteLog(time.Now().UTC(), "INFO", "direct log write", map[string]any{"conversation_id": "race:1", "operation": "op"})
	ev.End(nil)
	var row map[string]any
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if _, hasDB := row["db_query_count"]; hasDB {
		t.Fatalf("WriteLog recorded SQL telemetry (risk of recursive logging): %v", row)
	}

	// Concurrent WriteLog, Search, and Close must not race or panic.
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 25 {
				s.WriteLog(time.Now().UTC(), "INFO", fmt.Sprintf("msg-%d-%d", id, i), map[string]any{
					"conversation_id": "race:1",
					"operation":       "concurrent",
					"result":          "ok",
				})
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		s.Close()
	}()
	wg.Wait()

	// Calling WriteLog after Close is a safe no-op.
	s.WriteLog(time.Now().UTC(), "INFO", "after-close", map[string]any{"conversation_id": "race:1"})
}
