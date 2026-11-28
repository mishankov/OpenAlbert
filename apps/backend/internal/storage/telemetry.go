package storage

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"openalbert/apps/backend/internal/telemetry"
)

type sqlTracer struct {
	logger *slog.Logger
}

func (t sqlTracer) record(ctx context.Context, query string, elapsed time.Duration, rows int64, err error) {
	// Args deliberately never enter telemetry. sql.ErrNoRows is a normal
	// empty lookup for QueryRow, not a database error.
	reportErr := err
	if errors.Is(reportErr, sql.ErrNoRows) {
		reportErr = nil
	}
	if telemetry.RecordSQL(ctx, query, elapsed, rows, reportErr != nil) {
		return
	}
	// Background work may have no enclosing event. Retain slow/error queries,
	// while keeping routine polling from producing one event per SQL call.
	if reportErr != nil || elapsed >= telemetry.SQLSlowThreshold {
		logger := t.logger
		if logger == nil {
			logger = slog.Default()
		}
		telemetry.LogSQL(ctx, logger, query, elapsed, rows, reportErr)
	}
}

type tracedRow struct {
	ctx    context.Context
	tracer sqlTracer
	query  string
	start  time.Time
	row    *sql.Row
	once   sync.Once
}

func (r *tracedRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.once.Do(func() {
		var rows int64
		if err == nil {
			rows = 1
		}
		r.tracer.record(r.ctx, r.query, time.Since(r.start), rows, err)
	})
	return err
}

type tracedRows struct {
	ctx      context.Context
	tracer   sqlTracer
	query    string
	start    time.Time
	rows     *sql.Rows
	count    int64
	err      error
	recorded bool
}

func (r *tracedRows) Next() bool {
	if r.rows.Next() {
		r.count++
		return true
	}
	r.finish(r.rows.Err())
	return false
}

func (r *tracedRows) Scan(dest ...any) error {
	err := r.rows.Scan(dest...)
	if err != nil {
		r.err = err
	}
	return err
}

func (r *tracedRows) Err() error {
	err := r.rows.Err()
	if err != nil {
		r.err = err
	}
	r.finish(r.err)
	return err
}

func (r *tracedRows) Close() error {
	err := r.rows.Close()
	if r.err == nil && err != nil {
		r.err = err
	}
	r.finish(r.err)
	return err
}

func (r *tracedRows) finish(err error) {
	if r.recorded {
		return
	}
	r.recorded = true
	r.tracer.record(r.ctx, r.query, time.Since(r.start), r.count, err)
}

func execTraced(ctx context.Context, tracer sqlTracer, fn func(context.Context, string, ...any) (sql.Result, error), query string, args ...any) (sql.Result, error) {
	start := time.Now()
	res, err := fn(ctx, query, args...)
	var rows int64
	if err == nil && res != nil {
		if n, rerr := res.RowsAffected(); rerr == nil {
			rows = n
		}
	}
	tracer.record(ctx, query, time.Since(start), rows, err)
	return res, err
}

func queryTraced(ctx context.Context, tracer sqlTracer, fn func(context.Context, string, ...any) (*sql.Rows, error), query string, args ...any) (*tracedRows, error) {
	start := time.Now()
	rows, err := fn(ctx, query, args...)
	if err != nil {
		tracer.record(ctx, query, time.Since(start), 0, err)
		return nil, err
	}
	return &tracedRows{ctx: ctx, tracer: tracer, query: query, start: start, rows: rows}, nil
}

func queryRowTraced(ctx context.Context, tracer sqlTracer, fn func(context.Context, string, ...any) *sql.Row, query string, args ...any) *tracedRow {
	start := time.Now()
	return &tracedRow{ctx: ctx, tracer: tracer, query: query, start: start, row: fn(ctx, query, args...)}
}
