package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSQLSummaryNestedConcurrentAndLateQueries(t *testing.T) {
	var out bytes.Buffer
	parent := Start(WithFields(context.Background(), map[string]any{"session_id": "s"}), New(&out), "session.analyze")
	child := Start(parent.Context(), New(&out), "llm.complete")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			RecordSQL(child.Context(), "SELECT id\n FROM items WHERE id=$1", 5*time.Millisecond, 2, false)
		})
	}
	wg.Wait()
	RecordSQL(child.Context(), "SELECT data FROM items", 150*time.Millisecond, 0, true)
	child.End(nil)
	RecordSQL(parent.Context(), "UPDATE items SET data=$1", time.Millisecond, 1, false)
	parent.End(nil)
	parent.End(nil)
	before := out.String()
	RecordSQL(child.Context(), "LATE QUERY", time.Second, 50, true)
	if out.String() != before {
		t.Fatal("late query emitted a second event")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("queries emitted individual events: %s", out.String())
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["db_query_count"] != float64(21+i) || row["db_duration_ms"] != float64(250+i) || row["db_rows"] != float64(40+i) || row["db_error_count"] != float64(1) || row["db_slow_query_count"] != float64(1) || row["session_id"] != "s" {
			t.Fatalf("bad SQL summary: %v", row)
		}
		queries := row["db_queries"].([]any)
		if queries[1].(map[string]any)["count"] != float64(20) {
			t.Fatalf("repeated queries were not grouped: %v", queries)
		}
	}
}

func TestSQLSummaryBoundedButTotalsAndSlowestRemainComplete(t *testing.T) {
	var out bytes.Buffer
	e := Start(context.Background(), New(&out), "test")
	for i := range maxSQLQueries + 4 {
		RecordSQL(e.Context(), fmt.Sprintf("SELECT column_%d FROM items", i), time.Millisecond, 1, false)
	}
	query := "SELECT " + strings.Repeat("я", maxSQLLength)
	RecordSQL(e.Context(), query, time.Second, 7, true)
	e.End(nil)
	var row struct {
		Count   int64      `json:"db_query_count"`
		Omitted int64      `json:"db_queries_omitted"`
		Queries []SQLQuery `json:"db_queries"`
		Slowest SQLQuery   `json:"db_slowest_query"`
	}
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.Count != maxSQLQueries+5 || row.Omitted != 5 || len(row.Queries) != maxSQLQueries || !row.Slowest.Truncated || !utf8.ValidString(row.Slowest.SQL) || len(row.Slowest.SQL) > maxSQLLength || row.Slowest.MaxMS != 1000 {
		t.Fatalf("unbounded/incomplete SQL summary: %+v", row)
	}
	if RecordSQL(context.Background(), "SELECT 1", 0, 0, false) {
		t.Fatal("query without an event claimed to have a collector")
	}
}

func TestSQLContextKeepsCancellationAndSeparatesOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	e := Start(ctx, New(&out), "first")
	cancel()
	if e.Context().Err() != context.Canceled {
		t.Fatal("event detached cancellation")
	}
	RecordSQL(e.Context(), "SELECT 1", time.Millisecond, 1, false)
	e.End(nil)
	out.Reset()
	Start(context.Background(), New(&out), "second").End(nil)
	if strings.Contains(out.String(), "db_query_count") {
		t.Fatal("SQL metrics leaked into another operation")
	}
}
