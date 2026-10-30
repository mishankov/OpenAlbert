package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"openalbert/apps/backend/internal/telemetry"
)

type queryTracer struct{ logger *slog.Logger }
type queryTraceKey struct{}
type queryTrace struct {
	start time.Time
	sql   string
}

var _ pgx.QueryTracer = queryTracer{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// Args deliberately never enter the trace context or event.
	return context.WithValue(ctx, queryTraceKey{}, queryTrace{time.Now(), data.SQL})
}

func (t queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	query, ok := ctx.Value(queryTraceKey{}).(queryTrace)
	if !ok {
		return
	}
	elapsed := time.Since(query.start)
	if telemetry.RecordSQL(ctx, query.sql, elapsed, data.CommandTag.RowsAffected(), data.Err != nil) {
		return
	}
	// Background work may have no enclosing event. Retain slow/error queries,
	// while keeping routine polling from producing one event per SQL call.
	if data.Err != nil || elapsed >= telemetry.SQLSlowThreshold {
		telemetry.LogSQL(ctx, t.logger, query.sql, elapsed, data.CommandTag.RowsAffected(), data.Err)
	}
}
