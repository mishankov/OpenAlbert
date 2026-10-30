package telemetry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SQLSlowThreshold = 100 * time.Millisecond
	maxSQLQueries    = 16
	maxSQLLength     = 2048
)

// SQLQuery contains only a parameterized template and operational metadata.
// Never pass parameter values, result data or error text to the collector.
type SQLQuery struct {
	ID         string  `json:"query_id"`
	SQL        string  `json:"sql"`
	Truncated  bool    `json:"sql_truncated,omitempty"`
	Count      int64   `json:"count"`
	DurationMS float64 `json:"duration_ms"`
	MaxMS      float64 `json:"max_duration_ms"`
	Rows       int64   `json:"rows"`
	Errors     int64   `json:"error_count"`
}

type sqlStats struct {
	count, errors, rows, slow, omitted int64
	duration                           time.Duration
	queries                            map[string]*SQLQuery
	slowest                            SQLQuery
}

// LogSQL retains an unscoped slow/error query as one completed operation with
// the actual query duration, rather than the time needed to build its event.
func LogSQL(ctx context.Context, logger *slog.Logger, sql string, elapsed time.Duration, rows int64, err error) {
	e := Start(ctx, logger, "db.query")
	e.start = time.Now().Add(-elapsed)
	RecordSQL(e.Context(), sql, elapsed, rows, err != nil)
	e.End(err)
}

// RecordSQL accumulates completed client-side Query/QueryRow/Exec calls in the
// active operation and its parents. It does not emit a log entry per query.
// False means the caller has no operation; background slow/error calls can then
// be emitted as a standalone bounded operation instead of disappearing.
func RecordSQL(ctx context.Context, sql string, elapsed time.Duration, rows int64, failed bool) bool {
	event, _ := ctx.Value(eventKey{}).(*Event)
	if event == nil {
		return false
	}
	normalized := strings.Join(strings.Fields(sql), " ")
	hash := sha256.Sum256([]byte(normalized))
	q := SQLQuery{ID: fmt.Sprintf("%x", hash[:]), SQL: normalized, Count: 1, DurationMS: float64(elapsed) / float64(time.Millisecond), Rows: rows}
	q.MaxMS = q.DurationMS
	if failed {
		q.Errors = 1
	}
	if len(q.SQL) > maxSQLLength {
		end := maxSQLLength
		for !utf8.RuneStart(q.SQL[end]) {
			end--
		}
		q.SQL, q.Truncated = q.SQL[:end], true
	}
	for e := event; e != nil; e = e.parent {
		e.mu.Lock()
		if !e.ended {
			e.sql.add(q, elapsed)
		}
		e.mu.Unlock()
	}
	return true
}

func (s *sqlStats) add(q SQLQuery, elapsed time.Duration) {
	s.count++
	s.duration += elapsed
	s.rows += q.Rows
	s.errors += q.Errors
	if elapsed >= SQLSlowThreshold {
		s.slow++
	}
	if s.count == 1 || q.MaxMS > s.slowest.MaxMS {
		s.slowest = q
	}
	if existing := s.queries[q.ID]; existing != nil {
		existing.Count++
		existing.DurationMS += q.DurationMS
		existing.Rows += q.Rows
		existing.Errors += q.Errors
		if q.MaxMS > existing.MaxMS {
			existing.MaxMS = q.MaxMS
		}
	} else if len(s.queries) < maxSQLQueries {
		if s.queries == nil {
			s.queries = make(map[string]*SQLQuery)
		}
		s.queries[q.ID] = &q
	} else {
		s.omitted++
	}
}

func (s *sqlStats) fields(fields map[string]any) {
	if s.count == 0 {
		return
	}
	queries := make([]SQLQuery, 0, len(s.queries))
	for _, q := range s.queries {
		queries = append(queries, *q)
	}
	sort.Slice(queries, func(i, j int) bool {
		if queries[i].DurationMS != queries[j].DurationMS {
			return queries[i].DurationMS > queries[j].DurationMS
		}
		return queries[i].ID < queries[j].ID
	})
	fields["db_query_count"] = s.count
	fields["db_duration_ms"] = float64(s.duration) / float64(time.Millisecond)
	fields["db_rows"] = s.rows
	fields["db_error_count"] = s.errors
	fields["db_slow_query_count"] = s.slow
	fields["db_max_duration_ms"] = s.slowest.MaxMS
	fields["db_slowest_query"] = s.slowest
	fields["db_queries"] = queries
	fields["db_queries_omitted"] = s.omitted
}
