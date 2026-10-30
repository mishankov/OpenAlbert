package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"openalbert/apps/backend/internal/telemetry"
)

func TestSQLTelemetryPostgres(t *testing.T) {
	url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set OPENALBERT_TEST_DATABASE_URL to a disposable database")
	}
	repo, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var out bytes.Buffer
	e := telemetry.Start(telemetry.WithFields(context.Background(), map[string]any{"turn_id": "turn"}), telemetry.New(&out), "test.sql")
	ctx := e.Context()
	var value string
	if err = repo.pool.QueryRow(ctx, "SELECT $1::text", "PRIVATE_PARAMETER").Scan(&value); err != nil || value != "PRIVATE_PARAMETER" {
		t.Fatalf("query row: value=%q err=%v", value, err)
	}
	rows, err := repo.pool.Query(ctx, "SELECT generate_series(1,3)")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		if err = rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.pool.Exec(ctx, "SELECT $1::int", "PRIVATE_BAD_PARAMETER"); err == nil {
		t.Fatal("expected a PostgreSQL error")
	}
	e.End(nil)
	var row map[string]any
	if err = json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["db_query_count"] != float64(3) || row["db_rows"] != float64(4) || row["db_error_count"] != float64(1) || row["turn_id"] != "turn" || row["db_duration_ms"].(float64) <= 0 {
		t.Fatalf("query lifecycle was not collected: %v", row)
	}
	if strings.Contains(out.String(), "PRIVATE") || strings.Count(strings.TrimSpace(out.String()), "\n") != 0 {
		t.Fatalf("parameters leaked or SQL produced separate events: %s", out.String())
	}
}

func TestSQLTracerFallbackIsQuietForFastQueriesAndRedactsErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		err     error
		logged  bool
	}{
		{"fast", 0, nil, false},
		{"slow", 150 * time.Millisecond, nil, true},
		{"error", 0, errors.New("PRIVATE_ERROR_TEXT"), true},
		{"cancelled", 0, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			tracer := queryTracer{logger: telemetry.New(&out)}
			ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT $1", Args: []any{"PRIVATE_PARAMETER"}})
			state := ctx.Value(queryTraceKey{}).(queryTrace)
			state.start = time.Now().Add(-tc.elapsed)
			ctx = context.WithValue(ctx, queryTraceKey{}, state)
			tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: tc.err})
			if (out.Len() > 0) != tc.logged || strings.Contains(out.String(), "PRIVATE") {
				t.Fatalf("bad fallback logging: %s", out.String())
			}
			if tc.elapsed > 0 {
				var row map[string]any
				if err := json.Unmarshal(out.Bytes(), &row); err != nil {
					t.Fatal(err)
				}
				if row["duration_ms"].(float64) < 150 || row["db_duration_ms"].(float64) < 150 {
					t.Fatalf("fallback logged event construction time: %v", row)
				}
			}
		})
	}
}
