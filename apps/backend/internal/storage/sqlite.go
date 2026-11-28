package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/scheduling"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("record not found")
var ErrInvalidTransition = errors.New("invalid state transition")
var ErrConflict = errors.New("record changed concurrently; reload and retry")

const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

type Store struct {
	writer  *sql.DB
	reader  *sql.DB
	tracer  sqlTracer
	closeMu sync.RWMutex
	closed  bool
}

var _ domain.Repository = (*Store)(nil)

func isMemoryDBPath(dbPath string) bool {
	return dbPath == ":memory:" ||
		strings.HasPrefix(dbPath, "file::memory:") ||
		strings.Contains(dbPath, "mode=memory")
}

func sqliteFileDSN(dbPath string) string {
	const pragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)"
	if strings.HasPrefix(dbPath, "file:") {
		if strings.Contains(dbPath, "?") {
			return dbPath + "&" + pragmas
		}
		return dbPath + "?" + pragmas
	}
	escaped := filepath.ToSlash(dbPath)
	escaped = strings.ReplaceAll(escaped, "%", "%25")
	escaped = strings.ReplaceAll(escaped, "?", "%3F")
	escaped = strings.ReplaceAll(escaped, "#", "%23")
	return "file:" + escaped + "?" + pragmas
}

func Open(ctx context.Context, dbPath string) (*Store, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, errors.New("sqlite database path is required")
	}
	var writer, reader *sql.DB
	if isMemoryDBPath(dbPath) {
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			return nil, fmt.Errorf("sqlite open: %w", err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		db.SetConnMaxIdleTime(0)
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite connect: %w", err)
		}
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA temp_store=MEMORY;`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite pragmas: %w", err)
		}
		writer, reader = db, db
	} else {
		if !strings.HasPrefix(dbPath, "file:") {
			if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil, fmt.Errorf("sqlite directory: %w", err)
				}
			}
		}
		dsn := sqliteFileDSN(dbPath)
		var err error
		writer, err = sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("sqlite writer open: %w", err)
		}
		writer.SetMaxOpenConns(1)
		writer.SetMaxIdleConns(1)
		writer.SetConnMaxLifetime(0)
		writer.SetConnMaxIdleTime(0)
		if err := writer.PingContext(ctx); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("sqlite writer connect: %w", err)
		}
		if _, err := writer.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA temp_store=MEMORY;`); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("sqlite writer pragmas: %w", err)
		}
		reader, err = sql.Open("sqlite", dsn)
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("sqlite reader open: %w", err)
		}
		reader.SetMaxOpenConns(4)
		reader.SetMaxIdleConns(4)
		reader.SetConnMaxLifetime(0)
		reader.SetConnMaxIdleTime(0)
		if err := reader.PingContext(ctx); err != nil {
			_ = reader.Close()
			_ = writer.Close()
			return nil, fmt.Errorf("sqlite reader connect: %w", err)
		}
	}

	s := &Store{
		writer: writer,
		reader: reader,
		tracer: sqlTracer{logger: slog.Default()},
	}

	tx, err := s.beginImmediate(ctx)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.conn.ExecContext(ctx, schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	// Telegram has no idempotency keys. Interrupted voice uploads switch to
	// text; any other interrupted send has an unknown outcome.
	if _, err := tx.conn.ExecContext(ctx, `UPDATE openalbert_outgoing SET
 state = CASE
  WHEN json_type(data, '$.voice_reply_to') IS NOT NULL AND json_type(data, '$.voice_fallback') IS NULL THEN 'pending'
  ELSE 'unknown'
 END,
 data = CASE
  WHEN json_type(data, '$.voice_reply_to') IS NOT NULL AND json_type(data, '$.voice_fallback') IS NULL
   THEN json_set(data, '$.state', 'pending', '$.voice_fallback', ?1)
  ELSE json_set(data, '$.state', 'unknown')
 END
 WHERE state = 'sending'`, string(domain.VoiceUnconfirmed)); err != nil {
		s.Close()
		return nil, err
	}
	// Creation may have succeeded without returning its message ID. Never create
	// another status in that case. Editing/deleting a known ID is safe to repeat.
	if _, err := tx.conn.ExecContext(ctx, `UPDATE openalbert_progress SET
 delivery_state = CASE WHEN COALESCE(json_extract(data, '$.delivery.message_id'), '') = '' THEN 'disabled' ELSE 'ready' END,
 delivery_version = delivery_version + 1,
 data = json_set(
  data,
  '$.delivery.state', CASE WHEN COALESCE(json_extract(data, '$.delivery.message_id'), '') = '' THEN 'disabled' ELSE 'ready' END,
  '$.delivery.version', delivery_version + 1
 )
 WHERE delivery_state = 'sending'`); err != nil {
		s.Close()
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("commit migration: %w", err)
	}

	if err := s.PruneLogs(ctx, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		s.Close()
		return nil, fmt.Errorf("prune logs: %w", err)
	}
	return s, nil
}

func (s *Store) Close() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	writer, reader := s.writer, s.reader
	s.closeMu.Unlock()
	if reader != nil && reader != writer {
		_ = reader.Close()
	}
	if writer != nil {
		_ = writer.Close()
	}
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.writer.PingContext(ctx); err != nil {
		return err
	}
	if s.reader != s.writer {
		return s.reader.PingContext(ctx)
	}
	return nil
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return execTraced(ctx, s.tracer, s.writer.ExecContext, query, args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*tracedRows, error) {
	return queryTraced(ctx, s.tracer, s.reader.QueryContext, query, args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *tracedRow {
	return queryRowTraced(ctx, s.tracer, s.reader.QueryRowContext, query, args...)
}

func (s *Store) writeQueryRow(ctx context.Context, query string, args ...any) *tracedRow {
	return queryRowTraced(ctx, s.tracer, s.writer.QueryRowContext, query, args...)
}

type writeTx struct {
	store *Store
	conn  *sql.Conn
	done  bool
}

func (s *Store) beginImmediate(ctx context.Context) (*writeTx, error) {
	conn, err := s.writer.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &writeTx{store: s, conn: conn}, nil
}

func (tx *writeTx) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return execTraced(ctx, tx.store.tracer, tx.conn.ExecContext, query, args...)
}

func (tx *writeTx) query(ctx context.Context, query string, args ...any) (*tracedRows, error) {
	return queryTraced(ctx, tx.store.tracer, tx.conn.QueryContext, query, args...)
}

func (tx *writeTx) queryRow(ctx context.Context, query string, args ...any) *tracedRow {
	return queryRowTraced(ctx, tx.store.tracer, tx.conn.QueryRowContext, query, args...)
}

func (tx *writeTx) Commit(ctx context.Context) error {
	if tx.done {
		return nil
	}
	tx.done = true
	_, err := tx.conn.ExecContext(ctx, "COMMIT")
	if err != nil {
		_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK")
	}
	_ = tx.conn.Close()
	return err
}

func (tx *writeTx) Rollback() {
	if tx.done {
		return
	}
	tx.done = true
	_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK")
	_ = tx.conn.Close()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

func formatTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func parseTime(raw string) (time.Time, error) {
	if t, err := time.Parse(sqliteTimeLayout, raw); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func parseFlexibleTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encode(v any) ([]byte, error) { return json.Marshal(v) }

func decode[T any](b []byte) (T, error) {
	var v T
	err := json.Unmarshal(b, &v)
	if in, ok := any(&v).(*domain.Incoming); ok && err == nil {
		var content struct {
			AttachmentData [][]byte `json:"attachment_data,omitempty"`
		}
		err = json.Unmarshal(b, &content)
		for i, data := range content.AttachmentData {
			if i < len(in.Attachments) {
				in.Attachments[i].Data = data
			}
		}
	}
	return v, err
}

func validID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is required")
	}
	if strings.ContainsRune(id, 0) || len(id) > 4096 {
		return errors.New("invalid id")
	}
	return nil
}

func limitValue(n int) int {
	if n <= 0 || n > 1000 {
		return 100
	}
	return n
}

func nowIfZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

func taskStatus(s string) bool {
	switch s {
	case "queued", "running", "waiting", "paused", "completed", "cancelled", "failed", "blocked":
		return true
	}
	return false
}

func runnable(s string) bool          { return s == "queued" || s == "running" || s == "waiting" }
func taskKey(scope, id string) string { return scope + "\x00" + id }

type checkpointMeta struct {
	turnSessionID            any
	turnTaskID               any
	turnInitiative           any
	turnStartedAt            any
	consolidationScope       any
	consolidationCompletedAt any
	audioSessionID           any
	audioQueuedAt            any
}

func extractCheckpointMeta(key string, raw []byte) checkpointMeta {
	var meta checkpointMeta
	switch {
	case strings.HasPrefix(key, "turn-done/"):
		var payload struct {
			Input struct {
				SessionID  string          `json:"SessionID"`
				TaskID     string          `json:"TaskID"`
				Initiative json.RawMessage `json:"Initiative"`
				StartedAt  string          `json:"StartedAt"`
			} `json:"Input"`
		}
		_ = json.Unmarshal(raw, &payload)
		if payload.Input.SessionID != "" {
			meta.turnSessionID = payload.Input.SessionID
		}
		if payload.Input.TaskID != "" {
			meta.turnTaskID = payload.Input.TaskID
		}
		initVal := strings.TrimSpace(string(payload.Input.Initiative))
		if initVal == "true" || initVal == `"true"` {
			meta.turnInitiative = 1
		} else {
			meta.turnInitiative = 0
		}
		if t, ok := parseFlexibleTime(payload.Input.StartedAt); ok {
			meta.turnStartedAt = formatTime(t)
		} else if payload.Input.StartedAt != "" {
			meta.turnStartedAt = payload.Input.StartedAt
		}
	case strings.HasPrefix(key, "memory-consolidation/"):
		var payload struct {
			Scope       string `json:"scope"`
			CompletedAt string `json:"completed_at"`
		}
		_ = json.Unmarshal(raw, &payload)
		if payload.Scope != "" {
			meta.consolidationScope = payload.Scope
		}
		if t, ok := parseFlexibleTime(payload.CompletedAt); ok {
			meta.consolidationCompletedAt = formatTime(t)
		}
	case strings.HasPrefix(key, "audio/"):
		var payload struct {
			SessionID string `json:"session_id"`
			QueuedAt  string `json:"queued_at"`
		}
		_ = json.Unmarshal(raw, &payload)
		if payload.SessionID != "" {
			meta.audioSessionID = payload.SessionID
		}
		if t, ok := parseFlexibleTime(payload.QueuedAt); ok {
			meta.audioQueuedAt = formatTime(t)
		} else if payload.QueuedAt != "" {
			meta.audioQueuedAt = payload.QueuedAt
		}
	}
	return meta
}

func upsertCheckpointSQL(execFn func(context.Context, string, ...any) (sql.Result, error), ctx context.Context, key string, raw []byte, onConflictUpdate bool) (sql.Result, error) {
	meta := extractCheckpointMeta(key, raw)
	clause := "ON CONFLICT(id) DO NOTHING"
	if onConflictUpdate {
		clause = `ON CONFLICT(id) DO UPDATE SET
 data = excluded.data,
 turn_session_id = excluded.turn_session_id,
 turn_task_id = excluded.turn_task_id,
 turn_initiative = excluded.turn_initiative,
 turn_started_at = excluded.turn_started_at,
 consolidation_scope = excluded.consolidation_scope,
 consolidation_completed_at = excluded.consolidation_completed_at,
 audio_session_id = excluded.audio_session_id,
 audio_queued_at = excluded.audio_queued_at`
	}
	return execFn(ctx, `INSERT INTO openalbert_checkpoints(
 id, data, turn_session_id, turn_task_id, turn_initiative, turn_started_at,
 consolidation_scope, consolidation_completed_at, audio_session_id, audio_queued_at
) VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10) `+clause,
		key, string(raw),
		meta.turnSessionID, meta.turnTaskID, meta.turnInitiative, meta.turnStartedAt,
		meta.consolidationScope, meta.consolidationCompletedAt,
		meta.audioSessionID, meta.audioQueuedAt,
	)
}

func upsertTraceSQL(execFn func(context.Context, string, ...any) (sql.Result, error), ctx context.Context, tr domain.Trace) error {
	raw, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	raw = sanitizeJSONB(raw)
	startedAt := formatTime(tr.StartedAt)
	endedAt := formatTimePtr(tr.EndedAt)
	var triggerKind any
	switch tr.Trigger {
	case "message", "task", "initiative", "coding":
		triggerKind = tr.Trigger
	}
	var taskID any
	if tr.TaskID != "" {
		taskID = tr.TaskID
	}
	var traceStatus any
	if tr.Status != "" {
		traceStatus = tr.Status
	}
	var searchProvider, searchMode, searchResolvedMode, searchCostUSD, searchHTTPStatus, searchResults, searchCompletedAt any
	searchRateLimited := 0
	searchOutcomeAt := startedAt
	if tr.Search != nil {
		if tr.Search.Provider != "" {
			searchProvider = tr.Search.Provider
		}
		if tr.Search.Mode != "" {
			searchMode = tr.Search.Mode
		}
		if tr.Search.ResolvedMode != "" {
			searchResolvedMode = tr.Search.ResolvedMode
		}
		if tr.Search.CostUSD != nil {
			searchCostUSD = *tr.Search.CostUSD
		}
		if tr.Search.HTTPStatus >= 0 {
			searchHTTPStatus = tr.Search.HTTPStatus
		}
		if tr.Search.ResultCount >= 0 {
			searchResults = tr.Search.ResultCount
		}
		if tr.Search.RateLimited {
			searchRateLimited = 1
		}
		if tr.Search.CompletedAt != nil && !tr.Search.CompletedAt.IsZero() {
			c := formatTime(*tr.Search.CompletedAt)
			searchCompletedAt = c
			searchOutcomeAt = c
		}
	}
	res, err := execFn(ctx, `INSERT INTO openalbert_traces(
 id, session_id, turn_id, started_at, ended_at, data,
 trigger_kind, task_id,
 search_provider, search_mode, search_resolved_mode,
 search_cost_usd, search_http_status, search_results, search_rate_limited,
 search_completed_at, search_outcome_at, trace_status
) VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18)
ON CONFLICT(id) DO UPDATE SET
 turn_id = excluded.turn_id,
 started_at = excluded.started_at,
 ended_at = excluded.ended_at,
 data = excluded.data,
 trigger_kind = excluded.trigger_kind,
 task_id = excluded.task_id,
 search_provider = excluded.search_provider,
 search_mode = excluded.search_mode,
 search_resolved_mode = excluded.search_resolved_mode,
 search_cost_usd = excluded.search_cost_usd,
 search_http_status = excluded.search_http_status,
 search_results = excluded.search_results,
 search_rate_limited = excluded.search_rate_limited,
 search_completed_at = excluded.search_completed_at,
 search_outcome_at = excluded.search_outcome_at,
 trace_status = excluded.trace_status
WHERE openalbert_traces.session_id = excluded.session_id`,
		tr.ID, tr.SessionID, tr.TurnID, startedAt, endedAt, string(raw),
		triggerKind, taskID,
		searchProvider, searchMode, searchResolvedMode,
		searchCostUSD, searchHTTPStatus, searchResults, searchRateLimited,
		searchCompletedAt, searchOutcomeAt, traceStatus,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrSessionConflict
	}
	return nil
}

// AcceptIncoming commits the Telegram offset together with the durable inbox.
// The private attachment_data field preserves bytes omitted by the public DTO.
func (s *Store) AcceptIncoming(ctx context.Context, in domain.Incoming, offset int64) (bool, error) {
	if err := validID(in.ID); err != nil {
		return false, err
	}
	if err := validID(in.Conversation.ID); err != nil {
		return false, err
	}
	in.CreatedAt = nowIfZero(in.CreatedAt)
	attachmentData := make([][]byte, len(in.Attachments))
	for i := range in.Attachments {
		attachmentData[i] = in.Attachments[i].Data
	}
	b, err := encode(struct {
		domain.Incoming
		AttachmentData [][]byte `json:"attachment_data,omitempty"`
	}{in, attachmentData})
	if err != nil {
		return false, err
	}
	canonical := in.Conversation
	canonical.DeliveryChannel = ""

	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.exec(ctx, "INSERT INTO openalbert_incoming(id, conversation_id, data) VALUES(?1, ?2, ?3) ON CONFLICT(id) DO NOTHING", in.ID, in.Conversation.ID, string(b))
	if err != nil {
		return false, err
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	var existingConvStr string
	convErr := tx.queryRow(ctx, "SELECT data FROM openalbert_conversations WHERE id = ?1", in.Conversation.ID).Scan(&existingConvStr)
	if convErr == nil {
		var existing domain.Conversation
		if json.Unmarshal([]byte(existingConvStr), &existing) == nil &&
			existing.ThreadID > 0 && existing.Title != "" && !existing.TitleFromChat &&
			!(in.Conversation.TitleFromChat && existing.Title == in.Conversation.Title) {
			canonical.Title = existing.Title
			canonical.TitleFromChat = false
		}
	} else if !errors.Is(convErr, sql.ErrNoRows) {
		return false, convErr
	}
	c, err := encode(canonical)
	if err != nil {
		return false, err
	}
	if _, err := tx.exec(ctx, "INSERT INTO openalbert_conversations(id, data) VALUES(?1, ?2) ON CONFLICT(id) DO UPDATE SET data = excluded.data", in.Conversation.ID, string(c)); err != nil {
		return false, err
	}
	if _, err := tx.exec(ctx, "UPDATE openalbert_poll SET offset_value = MAX(offset_value, ?1) WHERE id = 1", offset); err != nil {
		return false, err
	}
	if rowsAffected > 0 {
		if _, err := tx.exec(ctx, "INSERT INTO openalbert_public_timeline(kind, source_id, conversation_id) VALUES('incoming', ?1, ?2) ON CONFLICT(kind, source_id) DO NOTHING", in.ID, in.Conversation.ID); err != nil {
			return false, err
		}
	}
	return rowsAffected > 0, tx.Commit(ctx)
}

func (s *Store) PollOffset(ctx context.Context) (int64, error) {
	var n int64
	err := s.queryRow(ctx, "SELECT offset_value FROM openalbert_poll WHERE id = 1").Scan(&n)
	return n, err
}

func (s *Store) SetPollOffset(ctx context.Context, offset int64) error {
	_, err := s.exec(ctx, "UPDATE openalbert_poll SET offset_value = MAX(offset_value, ?1) WHERE id = 1", offset)
	return err
}

func (s *Store) PendingIncoming(ctx context.Context, n int) ([]domain.Incoming, error) {
	return selectJSON[domain.Incoming](ctx, s, "SELECT data FROM openalbert_incoming WHERE handled = 0 ORDER BY seq ASC LIMIT ?1", limitValue(n))
}

func (s *Store) Incoming(ctx context.Context, id string) (domain.Incoming, bool, error) {
	return oneJSON[domain.Incoming](ctx, s, "SELECT data FROM openalbert_incoming WHERE id = ?1", id)
}

func (s *Store) MarkIncomingHandled(ctx context.Context, id string) error {
	_, err := s.exec(ctx, "UPDATE openalbert_incoming SET handled = 1 WHERE id = ?1", id)
	return err
}

func (s *Store) Conversation(ctx context.Context, id string) (domain.Conversation, bool, error) {
	return oneJSON[domain.Conversation](ctx, s, "SELECT data FROM openalbert_conversations WHERE id = ?1", id)
}

func (s *Store) SaveConversation(ctx context.Context, conv domain.Conversation) error {
	if err := validID(conv.ID); err != nil {
		return err
	}
	conv.DeliveryChannel = ""
	b, err := encode(conv)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, "INSERT INTO openalbert_conversations(id, data) VALUES(?1, ?2) ON CONFLICT(id) DO UPDATE SET data = excluded.data", conv.ID, string(b))
	return err
}

func (s *Store) ListPrivateConversations(ctx context.Context) ([]domain.Conversation, error) {
	return selectJSON[domain.Conversation](ctx, s, "SELECT data FROM openalbert_conversations WHERE json_extract(data, '$.private') = 1 AND id NOT LIKE 'coding:%' ORDER BY id ASC")
}

func (s *Store) ListConversations(ctx context.Context) ([]domain.Conversation, error) {
	return selectJSON[domain.Conversation](ctx, s, "SELECT data FROM openalbert_conversations ORDER BY id ASC")
}

// Tool-based replies are stored as assistant reply tool calls, not plain content.
// Their presence records a reply request, not proof of Telegram delivery.
func historyContent(m domain.Message) string {
	parts := []string{}
	if m.Content != "" {
		parts = append(parts, m.Content)
	}
	if m.Role == "assistant" {
		for _, call := range m.ToolCalls {
			if !domain.IsReplyTool(call.Name) {
				continue
			}
			var args struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(call.Arguments, &args) == nil && args.Text != "" {
				parts = append(parts, args.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func historyToolCalls(m domain.Message) []domain.HistoryToolCall {
	var calls []domain.HistoryToolCall
	if m.Role == "assistant" {
		for _, call := range m.ToolCalls {
			if domain.IsReplyTool(call.Name) {
				calls = append(calls, domain.HistoryToolCall{ID: call.ID, Name: call.Name})
			}
		}
	}
	return calls
}

func historyLimit(n int) int {
	if n <= 0 || n > 50 {
		return 20
	}
	return n
}

// Results are newest first; before is an exclusive cursor into older messages.
func (s *Store) ConversationHistory(ctx context.Context, scope, query string, before int64, n int) ([]domain.HistoryMessage, error) {
	limit := historyLimit(n)
	lowerQuery := strings.ToLower(query)
	rows, err := s.query(ctx, `SELECT m.seq, s.id, s.data, m.data
 FROM openalbert_messages m
 JOIN openalbert_sessions s ON s.id = m.session_id
 WHERE s.conversation_id = ?1
   AND (?2 = 0 OR m.seq < ?2)
   AND json_extract(m.data, '$.role') IN ('user', 'assistant')
 ORDER BY m.seq DESC`, scope, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.HistoryMessage{}
	for rows.Next() {
		var item domain.HistoryMessage
		var sessionData, messageData string
		if err := rows.Scan(&item.Seq, &item.SessionID, &sessionData, &messageData); err != nil {
			return nil, err
		}
		session, err := decode[domain.Session]([]byte(sessionData))
		if err != nil {
			return nil, err
		}
		message, err := decode[domain.Message]([]byte(messageData))
		if err != nil {
			return nil, err
		}
		content := historyContent(message)
		calls := historyToolCalls(message)
		if content == "" && len(calls) == 0 {
			continue
		}
		if lowerQuery != "" && !strings.Contains(strings.ToLower(content), lowerQuery) {
			continue
		}
		item.ConversationID, item.SessionStartedAt = scope, session.CreatedAt
		item.AuthorID = message.AuthorID
		item.Role, item.Content = message.Role, content
		item.ToolCalls = calls
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func (s *Store) Session(ctx context.Context, id string) (domain.Session, bool, error) {
	return oneJSON[domain.Session](ctx, s, "SELECT data FROM openalbert_sessions WHERE id = ?1", id)
}

func (s *Store) StartSession(ctx context.Context, se domain.Session) error {
	if err := validID(se.ID); err != nil {
		return err
	}
	if err := validID(se.ConversationID); err != nil {
		return err
	}
	se.CreatedAt = nowIfZero(se.CreatedAt)
	se.Status = "active"
	b, err := encode(se)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, "INSERT INTO openalbert_sessions(id, conversation_id, data) VALUES(?1, ?2, ?3) ON CONFLICT(id) DO NOTHING", se.ID, se.ConversationID, string(b))
	return err
}

// sanitizeJSONB removes NUL escape sequences (\u0000) and literal NUL bytes
// before persisting JSON payloads.
func sanitizeJSONB(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var out []byte
	for i := 0; i < len(raw); {
		if raw[i] == 0 {
			if out == nil {
				out = append(make([]byte, 0, len(raw)), raw[:i]...)
			}
			out = append(out, '\xef', '\xbf', '\xbd')
			i++
			continue
		}
		if raw[i] == '\\' && i+5 < len(raw) && raw[i+1] == 'u' && raw[i+2] == '0' && raw[i+3] == '0' && raw[i+4] == '0' && raw[i+5] == '0' {
			// Count preceding backslashes to distinguish \u0000 from \\u0000.
			slashes := 0
			for j := i - 1; j >= 0 && raw[j] == '\\'; j-- {
				slashes++
			}
			if slashes%2 == 0 {
				if out == nil {
					out = append(make([]byte, 0, len(raw)), raw[:i]...)
				}
				out = append(out, `\ufffd`...)
				i += 6
				continue
			}
		}
		if out != nil {
			out = append(out, raw[i])
		}
		i++
	}
	if out == nil {
		return raw
	}
	return out
}

func (s *Store) SyncCodingSession(ctx context.Context, batch domain.CodingSyncBatch) error {
	if err := validID(batch.SessionID); err != nil {
		return err
	}
	if err := validID(batch.ConversationID); err != nil {
		return err
	}
	if !strings.HasPrefix(batch.ConversationID, "coding:") {
		return domain.ErrSessionForbidden
	}
	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Ensure conversation exists / update title if provided.
	var existingConvRaw string
	convErr := tx.queryRow(ctx, "SELECT data FROM openalbert_conversations WHERE id = ?1", batch.ConversationID).Scan(&existingConvRaw)
	if errors.Is(convErr, sql.ErrNoRows) {
		title := strings.TrimSpace(batch.Title)
		if title == "" {
			title = "Coding · " + batch.SessionID
		}
		convBytes, err := encode(domain.Conversation{
			ID: batch.ConversationID, Title: title, OwnerID: batch.OwnerID, Private: true,
		})
		if err != nil {
			return err
		}
		if _, err := tx.exec(ctx, "INSERT INTO openalbert_conversations(id, data) VALUES(?1, ?2) ON CONFLICT(id) DO NOTHING", batch.ConversationID, string(sanitizeJSONB(convBytes))); err != nil {
			return err
		}
	} else if convErr != nil {
		return convErr
	} else if title := strings.TrimSpace(batch.Title); title != "" {
		var conv domain.Conversation
		if json.Unmarshal([]byte(existingConvRaw), &conv) == nil && conv.Title != title {
			conv.Title = title
			if convBytes, err := encode(conv); err == nil {
				if _, err := tx.exec(ctx, "UPDATE openalbert_conversations SET data = ?2 WHERE id = ?1", batch.ConversationID, string(sanitizeJSONB(convBytes))); err != nil {
					return err
				}
			}
		}
	}

	// Atomically ensure session row exists under the BEGIN IMMEDIATE write lock.
	sessBytes, err := encode(domain.Session{
		ID: batch.SessionID, ConversationID: batch.ConversationID, Status: "active", Trigger: "coding", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if _, err := tx.exec(ctx, "INSERT INTO openalbert_sessions(id, conversation_id, data) VALUES(?1, ?2, ?3) ON CONFLICT(id) DO NOTHING", batch.SessionID, batch.ConversationID, string(sessBytes)); err != nil {
		return err
	}
	var lockedRaw string
	if err := tx.queryRow(ctx, "SELECT data FROM openalbert_sessions WHERE id = ?1", batch.SessionID).Scan(&lockedRaw); err != nil {
		return err
	}
	var existing domain.Session
	if err := json.Unmarshal([]byte(lockedRaw), &existing); err != nil {
		return err
	}
	if !strings.HasPrefix(existing.ConversationID, "coding:") {
		return domain.ErrSessionForbidden
	}
	if existing.ConversationID != batch.ConversationID {
		return domain.ErrSessionConflict
	}
	if existing.Status != "active" {
		return domain.ErrSessionClosed
	}

	recordedAt := formatTime(time.Now().UTC())
	for _, item := range batch.Messages {
		if err := validID(item.Key); err != nil {
			return err
		}
		mb, err := encode(item.Message)
		if err != nil {
			return err
		}
		mb = sanitizeJSONB(mb)
		if _, err := tx.exec(ctx, "INSERT INTO openalbert_messages(session_id, mutation_key, data, recorded_at) VALUES(?1, ?2, ?3, ?4) ON CONFLICT(session_id, mutation_key) DO NOTHING", batch.SessionID, item.Key, string(mb), recordedAt); err != nil {
			return err
		}
	}
	for _, tr := range batch.Traces {
		if err := upsertTraceSQL(tx.exec, ctx, tr); err != nil {
			return err
		}
	}
	if batch.CheckpointKey != "" && len(batch.CheckpointPayload) > 0 {
		if err := validID(batch.CheckpointKey); err != nil {
			return err
		}
		cp := sanitizeJSONB(batch.CheckpointPayload)
		if _, err := upsertCheckpointSQL(tx.exec, ctx, batch.CheckpointKey, cp, true); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// CloseStaleCodingSessions closes active coding:* sessions whose latest known
// activity (session creation, recorded message, or trace start/end) is strictly
// older than cutoff. Runs in BEGIN IMMEDIATE so it serializes cleanly with
// concurrent SyncCodingSession calls and never touches non-coding conversations.
func (s *Store) CloseStaleCodingSessions(ctx context.Context, cutoff time.Time, limit int) ([]domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cutoff = cutoff.UTC()
	maxClose := limitValue(limit)

	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.query(ctx, `SELECT id, data FROM openalbert_sessions WHERE conversation_id LIKE 'coding:%' AND json_extract(data, '$.status') = 'active' ORDER BY seq ASC`)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id   string
		sess domain.Session
	}
	var candidates []candidate
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		sess, err := decode[domain.Session]([]byte(raw))
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate{id: id, sess: sess})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	closed := []domain.Session{}
	for _, cand := range candidates {
		if len(closed) >= maxClose {
			break
		}
		if cand.sess.Status != "active" || !strings.HasPrefix(cand.sess.ConversationID, "coding:") {
			continue
		}
		lastActivity := cand.sess.CreatedAt.UTC()
		if !lastActivity.IsZero() && !lastActivity.Before(cutoff) {
			continue
		}
		var lastMsgAt sql.NullString
		if err := tx.queryRow(ctx, `SELECT MAX(recorded_at) FROM openalbert_messages WHERE session_id = ?1`, cand.id).Scan(&lastMsgAt); err != nil {
			return nil, err
		}
		var lastTraceAt sql.NullString
		if err := tx.queryRow(ctx, `SELECT MAX(CASE WHEN ended_at IS NOT NULL AND ended_at > started_at THEN ended_at ELSE started_at END) FROM openalbert_traces WHERE session_id = ?1`, cand.id).Scan(&lastTraceAt); err != nil {
			return nil, err
		}
		if lastMsgAt.Valid {
			if t, err := parseTime(lastMsgAt.String); err == nil && t.After(lastActivity) {
				lastActivity = t
			}
		}
		if lastTraceAt.Valid {
			if t, err := parseTime(lastTraceAt.String); err == nil && t.After(lastActivity) {
				lastActivity = t
			}
		}
		if lastActivity.IsZero() || !lastActivity.Before(cutoff) {
			continue
		}
		res, err := tx.exec(ctx, `UPDATE openalbert_sessions SET data = json_set(data, '$.status', 'closed') WHERE id = ?1 AND json_extract(data, '$.status') = 'active'`, cand.id)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			cand.sess.Status = "closed"
			closed = append(closed, cand.sess)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return closed, nil
}

func (s *Store) EndSession(ctx context.Context, id, summary string) error {
	res, err := s.exec(ctx, `UPDATE openalbert_sessions
 SET data = json_set(data, '$.status', 'closed', '$.summary', ?2)
 WHERE id = ?1
   AND (json_extract(data, '$.status') = 'active'
    OR (COALESCE(json_extract(data, '$.summary'), '') = '' AND ?2 <> ''))`, id, summary)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, ok, err := oneJSON[domain.Session](ctx, s, "SELECT data FROM openalbert_sessions WHERE id = ?1", id)
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}

func (s *Store) PendingAnalysis(ctx context.Context, n int) ([]domain.Session, error) {
	return selectJSON[domain.Session](ctx, s, `SELECT data FROM openalbert_sessions
 WHERE analyzed = 0
   AND json_extract(data, '$.status') = 'closed'
   AND COALESCE(json_extract(data, '$.analysis_error'), '') = ''
 ORDER BY seq ASC LIMIT ?1`, limitValue(n))
}

func (s *Store) MarkAnalyzed(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE openalbert_sessions
 SET analyzed = 1
 WHERE id = ?1
   AND json_extract(data, '$.status') = 'closed'
   AND COALESCE(json_extract(data, '$.analysis_error'), '') = ''`, id)
	return err
}

func (s *Store) MarkAnalysisFailed(ctx context.Context, id, code string) error {
	_, err := s.exec(ctx, `UPDATE openalbert_sessions
 SET data = json_set(data, '$.analysis_error', ?2)
 WHERE id = ?1
   AND analyzed = 0
   AND json_extract(data, '$.status') = 'closed'
   AND COALESCE(json_extract(data, '$.analysis_error'), '') = ''`, id, code)
	return err
}

func (s *Store) AppendMessage(ctx context.Context, session, key string, m domain.Message) error {
	if err := validID(key); err != nil {
		return err
	}
	b, err := encode(m)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, "INSERT INTO openalbert_messages(session_id, mutation_key, data, recorded_at) VALUES(?1, ?2, ?3, ?4) ON CONFLICT(session_id, mutation_key) DO NOTHING", session, key, string(b), formatTime(time.Now().UTC()))
	return err
}

func (s *Store) Messages(ctx context.Context, session string) ([]domain.Message, error) {
	return selectJSON[domain.Message](ctx, s, "SELECT data FROM openalbert_messages WHERE session_id = ?1 ORDER BY seq ASC", session)
}

func (s *Store) CreateTask(ctx context.Context, t domain.Task) (domain.Task, error) {
	if err := validateTask(t); err != nil {
		return t, err
	}
	if t.Status == "" {
		t.Status = "queued"
	}
	t.CreatedAt = nowIfZero(t.CreatedAt)
	t.UpdatedAt = t.CreatedAt
	t.Version = 1
	b, err := encode(t)
	if err != nil {
		return t, err
	}
	_, err = s.exec(ctx, "INSERT INTO openalbert_tasks(conversation_id, id, data) VALUES(?1, ?2, ?3) ON CONFLICT(conversation_id, id) DO NOTHING", t.ConversationID, t.ID, string(b))
	if err != nil {
		return t, err
	}
	out, _, err := s.Task(ctx, t.ConversationID, t.ID)
	return out, err
}

func validateTask(t domain.Task) error {
	if err := validID(t.ID); err != nil {
		return err
	}
	if err := validID(t.ConversationID); err != nil {
		return err
	}
	if t.Status != "" && !taskStatus(t.Status) {
		return fmt.Errorf("invalid task status %q", t.Status)
	}
	return scheduling.Validate(t)
}

func (s *Store) UpdateTask(ctx context.Context, t domain.Task) (domain.Task, error) {
	if err := validateTask(t); err != nil {
		return t, err
	}
	if t.Status == "" {
		return t, errors.New("task status is required")
	}
	expectedVersion := t.Version
	t.Version++
	t.UpdatedAt = time.Now().UTC()
	b, err := encode(t)
	if err != nil {
		return t, err
	}
	var raw string
	err = s.writeQueryRow(ctx, `UPDATE openalbert_tasks
 SET data = json_set(?3, '$.created_at', json_extract(data, '$.created_at'))
 WHERE conversation_id = ?1
   AND id = ?2
   AND COALESCE(CAST(json_extract(data, '$.version') AS INTEGER), 1) = ?4
 RETURNING data`, t.ConversationID, t.ID, string(b), expectedVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		_, exists, lookupErr := s.Task(ctx, t.ConversationID, t.ID)
		if lookupErr != nil {
			return t, lookupErr
		}
		if exists {
			return t, ErrConflict
		}
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	return decode[domain.Task]([]byte(raw))
}

func (s *Store) FindTasks(ctx context.Context, scope, q string) ([]domain.Task, error) {
	all, err := selectJSON[domain.Task](ctx, s, "SELECT data FROM openalbert_tasks WHERE conversation_id = ?1 ORDER BY seq ASC", scope)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(q)
	out := []domain.Task{}
	for _, t := range all {
		if strings.Contains(strings.ToLower(t.Title), query) || strings.Contains(strings.ToLower(t.Goal), query) || t.ID == q {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Store) Task(ctx context.Context, scope, id string) (domain.Task, bool, error) {
	return oneJSON[domain.Task](ctx, s, "SELECT data FROM openalbert_tasks WHERE conversation_id = ?1 AND id = ?2", scope, id)
}

func (s *Store) RunnableTasks(ctx context.Context) ([]domain.Task, error) {
	return selectJSON[domain.Task](ctx, s, "SELECT data FROM openalbert_tasks WHERE json_extract(data, '$.status') IN ('queued', 'running', 'waiting') ORDER BY seq ASC")
}

func (s *Store) EnqueueOutgoing(ctx context.Context, o domain.Outgoing) error {
	if err := validID(o.ID); err != nil {
		return err
	}
	if err := validID(o.Conversation.ID); err != nil {
		return err
	}
	o.EnqueuedAt = nowIfZero(o.EnqueuedAt)
	o.State = "pending"
	o.Attempts = 0
	b, err := encode(o)
	if err != nil {
		return err
	}
	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.exec(ctx, `INSERT INTO openalbert_outgoing(id, turn_id, conversation_id, session_id, data)
 VALUES(?1, ?2, ?3, ?4, ?5) ON CONFLICT(id) DO NOTHING`, o.ID, o.TurnID, o.Conversation.ID, o.SessionID, string(b))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if _, err := tx.exec(ctx, "INSERT INTO openalbert_public_timeline(kind, source_id, conversation_id) VALUES('outgoing', ?1, ?2) ON CONFLICT(kind, source_id) DO NOTHING", o.ID, o.Conversation.ID); err != nil {
			return err
		}
		if o.RecordInHistory {
			session, message := outgoingHistory(o)
			se, err := encode(session)
			if err != nil {
				return err
			}
			msg, err := encode(message)
			if err != nil {
				return err
			}
			if _, err := tx.exec(ctx, "INSERT INTO openalbert_sessions(id, conversation_id, data, analyzed) VALUES(?1, ?2, ?3, 1)", session.ID, session.ConversationID, string(se)); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, "INSERT INTO openalbert_messages(session_id, mutation_key, data, recorded_at) VALUES(?1, ?2, ?3, ?4)", session.ID, o.ID, string(msg), formatTime(time.Now().UTC())); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) PendingOutgoing(ctx context.Context, n int) ([]domain.Outgoing, error) {
	return s.listOutgoing(ctx, "SELECT data, state, attempts FROM openalbert_outgoing WHERE state = 'pending' ORDER BY seq ASC LIMIT ?1", limitValue(n))
}

func (s *Store) PendingDeliverableOutgoing(ctx context.Context, n int, readyVoiceID string, startVoice bool) ([]domain.Outgoing, error) {
	// Block only the part of each conversation's queue at or behind its first
	// pending voice reply. Filter before LIMIT so busy chats cannot fill the batch.
	return s.listOutgoing(ctx, `WITH ranked_voice AS (
 SELECT
  conversation_id AS scope, id, seq,
  ROW_NUMBER() OVER (PARTITION BY conversation_id ORDER BY seq ASC) AS rn
 FROM openalbert_outgoing
 WHERE state = 'pending'
   AND json_type(data, '$.voice_reply_to') IS NOT NULL
   AND json_type(data, '$.voice_fallback') IS NULL
),
first_voice AS (
 SELECT scope, id, seq FROM ranked_voice WHERE rn = 1
)
SELECT o.data, o.state, o.attempts
FROM openalbert_outgoing o
LEFT JOIN first_voice v ON v.scope = o.conversation_id
WHERE o.state = 'pending' AND (
 v.seq IS NULL OR o.seq < v.seq OR
 (o.seq = v.seq AND (o.id = ?2 OR (?3 = 1 AND o.id = (SELECT id FROM first_voice ORDER BY seq ASC LIMIT 1))))
)
ORDER BY o.seq ASC LIMIT ?1`, limitValue(n), readyVoiceID, boolInt(startVoice))
}

func (s *Store) listOutgoing(ctx context.Context, query string, args ...any) ([]domain.Outgoing, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Outgoing{}
	for rows.Next() {
		var raw, state string
		var attempts int
		if err = rows.Scan(&raw, &state, &attempts); err != nil {
			return nil, err
		}
		v, e := decode[domain.Outgoing]([]byte(raw))
		if e != nil {
			return nil, e
		}
		v.State = state
		v.Attempts = attempts
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SetOutgoingState(ctx context.Context, id, state string) error {
	if state != "sending" && state != "sent" && state != "unknown" && state != "pending" && state != "failed" {
		return ErrInvalidTransition
	}
	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var current string
	err = tx.queryRow(ctx, "SELECT state FROM openalbert_outgoing WHERE id = ?1", id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current == state {
		return tx.Commit(ctx)
	}
	if !outgoingTransition(current, state) {
		return ErrInvalidTransition
	}
	if _, err := tx.exec(ctx, `UPDATE openalbert_outgoing
 SET state = ?2,
     attempts = attempts + CASE WHEN ?2 = 'sending' THEN 1 ELSE 0 END,
     data = json_set(data, '$.state', ?2, '$.attempts', attempts + CASE WHEN ?2 = 'sending' THEN 1 ELSE 0 END)
 WHERE id = ?1`, id, state); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FallbackVoice atomically converts the same durable reply to text. An
// interrupted update can never leave it eligible for another voice upload.
func (s *Store) FallbackVoice(ctx context.Context, id string, reason domain.VoiceFallback) error {
	res, err := s.exec(ctx, `UPDATE openalbert_outgoing
 SET state = 'pending',
     data = json_set(data, '$.state', 'pending', '$.voice_fallback', ?2)
 WHERE id = ?1
   AND state IN ('pending', 'sending')
   AND json_type(data, '$.voice_reply_to') IS NOT NULL
   AND json_type(data, '$.voice_fallback') IS NULL`, id, string(reason))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}

func outgoingTransition(from, to string) bool {
	return from == "pending" && to == "sending" || from == "sending" && (to == "sent" || to == "unknown" || to == "pending" || to == "failed")
}

func (s *Store) SaveArtifact(ctx context.Context, a domain.Artifact) error {
	if err := validID(a.ID); err != nil {
		return err
	}
	if err := validID(a.ConversationID); err != nil {
		return err
	}
	a.CreatedAt = nowIfZero(a.CreatedAt)
	if a.SizeBytes == 0 {
		a.SizeBytes = int64(len(a.Data))
	}
	b, err := encode(a)
	if err != nil {
		return err
	}
	data := a.Data
	if data == nil {
		data = []byte{}
	}
	_, err = s.exec(ctx, `INSERT INTO openalbert_artifacts(conversation_id, id, session_id, created_at, data, content)
 VALUES(?1, ?2, ?3, ?4, ?5, ?6) ON CONFLICT(conversation_id, id) DO NOTHING`,
		a.ConversationID, a.ID, a.SessionID, formatTime(a.CreatedAt), string(b), data)
	return err
}

func (s *Store) Artifact(ctx context.Context, scope, id string) (domain.Artifact, bool, error) {
	var raw string
	var content []byte
	err := s.queryRow(ctx, "SELECT data, content FROM openalbert_artifacts WHERE conversation_id = ?1 AND id = ?2", scope, id).Scan(&raw, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Artifact{}, false, nil
	}
	if err != nil {
		return domain.Artifact{}, false, err
	}
	a, err := decode[domain.Artifact]([]byte(raw))
	a.Data = append([]byte(nil), content...)
	return a, err == nil, err
}

type queryer interface {
	query(context.Context, string, ...any) (*tracedRows, error)
	queryRow(context.Context, string, ...any) *tracedRow
}

func oneJSON[T any](ctx context.Context, q queryer, query string, args ...any) (T, bool, error) {
	var raw string
	var zero T
	err := q.queryRow(ctx, query, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	v, err := decode[T]([]byte(raw))
	return v, err == nil, err
}

func selectJSON[T any](ctx context.Context, q queryer, query string, args ...any) ([]T, error) {
	rows, err := q.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		v, e := decode[T]([]byte(raw))
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) LoadCheckpoint(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var raw string
	err := s.queryRow(ctx, "SELECT data FROM openalbert_checkpoints WHERE id = ?1", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage([]byte(raw)), true, nil
}

func (s *Store) SaveCheckpoint(ctx context.Context, key string, value json.RawMessage) error {
	if err := validID(key); err != nil {
		return err
	}
	if !json.Valid(value) {
		return errors.New("invalid checkpoint JSON")
	}
	_, err := upsertCheckpointSQL(s.exec, ctx, key, value, true)
	return err
}

// ClaimCheckpoint creates a durable, single-writer claim without overwriting it.
func (s *Store) ClaimCheckpoint(ctx context.Context, key string, value json.RawMessage) (bool, error) {
	if err := validID(key); err != nil {
		return false, err
	}
	if !json.Valid(value) {
		return false, errors.New("invalid checkpoint JSON")
	}
	res, err := upsertCheckpointSQL(s.exec, ctx, key, value, false)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) ActiveSessions(ctx context.Context) ([]domain.Session, error) {
	return selectJSON[domain.Session](ctx, s, "SELECT data FROM openalbert_sessions WHERE json_extract(data, '$.status') = 'active'")
}

// MaintainTask locks via BEGIN IMMEDIATE before checking privileges.
// The audit receipt and mutation commit together, including across activity retries.
func (s *Store) MaintainTask(ctx context.Context, actorID, runID, key string, t domain.Task) (domain.Task, error) {
	if err := validID(key); err != nil {
		return t, err
	}
	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()

	rows, err := tx.query(ctx, "SELECT data FROM openalbert_tasks WHERE conversation_id = ?1 AND id IN (?2, ?3) ORDER BY id ASC", t.ConversationID, actorID, t.ID)
	if err != nil {
		return t, err
	}
	var actor, before domain.Task
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return t, err
		}
		task, e := decode[domain.Task]([]byte(raw))
		if e != nil {
			rows.Close()
			return t, e
		}
		if task.ID == actorID {
			actor = task
		}
		if task.ID == t.ID {
			before = task
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return t, err
	}
	if actor.ID == "" || before.ID == "" {
		return t, ErrNotFound
	}
	var receipt string
	err = tx.queryRow(ctx, "SELECT data FROM openalbert_checkpoints WHERE id = ?1", "task-maintenance/"+key).Scan(&receipt)
	if err == nil {
		audit, e := decode[TaskMaintenanceAudit]([]byte(receipt))
		if e != nil {
			return t, e
		}
		if audit.ActorTaskID != actorID || audit.TargetTaskID != t.ID || audit.RunID != runID || audit.ConversationID != t.ConversationID {
			return t, ErrConflict
		}
		return before, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return t, err
	}
	if err = validateMaintenance(actor, before, t, time.Now()); err != nil {
		return t, err
	}
	t.Version++
	t.UpdatedAt = time.Now().UTC()
	raw, err := encode(t)
	if err != nil {
		return t, err
	}
	if _, err = tx.exec(ctx, "UPDATE openalbert_tasks SET data = ?3 WHERE conversation_id = ?1 AND id = ?2", t.ConversationID, t.ID, string(raw)); err != nil {
		return t, err
	}
	audit, err := encode(maintenanceAudit(actorID, runID, key, before, t))
	if err != nil {
		return t, err
	}
	if _, err = upsertCheckpointSQL(tx.exec, ctx, "task-maintenance/"+key, audit, true); err != nil {
		return t, err
	}
	return t, tx.Commit(ctx)
}
