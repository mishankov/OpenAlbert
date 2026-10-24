// Package storage persists operational state independently of Temporal history.
package storage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"openalbert/internal/domain"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("record not found")
var ErrInvalidTransition = errors.New("invalid state transition")
var ErrConflict = errors.New("record changed concurrently; reload and retry")

type Store struct{ pool *pgxpool.Pool }

var _ domain.Repository = (*Store)(nil)

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres configuration: %w", err)
	}
	s := &Store{pool: pool}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	// A transaction-scoped advisory lock serializes migrations across startup workers.
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(785986713)"); err == nil {
		_, err = tx.Exec(ctx, schema)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	// Telegram has no idempotency keys. An interrupted send has unknown delivery;
	// never automatically send it a second time after restarting the process.
	if _, err = pool.Exec(ctx, "UPDATE openalbert_outgoing SET state='unknown',data=jsonb_set(data,'{state}','\"unknown\"'::jsonb) WHERE state='sending'"); err != nil {
		pool.Close()
		return nil, err
	}
	// Creation may have succeeded without returning its message ID. Never create
	// another status in that case. Editing/deleting a known ID is safe to repeat.
	if _, err = pool.Exec(ctx, `UPDATE openalbert_progress SET data=jsonb_set(jsonb_set(data,'{delivery,state}',CASE WHEN data->'delivery'->>'message_id'='' THEN '"disabled"'::jsonb ELSE '"ready"'::jsonb END),'{delivery,version}',to_jsonb((data->'delivery'->>'version')::bigint+1)) WHERE data->'delivery'->>'state'='sending'`); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func encode(v any) ([]byte, error)              { return json.Marshal(v) }
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
	c, err := encode(in.Conversation)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, "INSERT INTO openalbert_incoming(id,data) VALUES($1,$2) ON CONFLICT DO NOTHING", in.ID, b)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO openalbert_conversations(id,data) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET data=excluded.data", in.Conversation.ID, c); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, "UPDATE openalbert_poll SET offset_value=GREATEST(offset_value,$1) WHERE id", offset); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}
func (s *Store) PollOffset(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, "SELECT offset_value FROM openalbert_poll WHERE id").Scan(&n)
	return n, err
}
func (s *Store) SetPollOffset(ctx context.Context, offset int64) error {
	_, err := s.pool.Exec(ctx, "UPDATE openalbert_poll SET offset_value=GREATEST(offset_value,$1) WHERE id", offset)
	return err
}
func (s *Store) PendingIncoming(ctx context.Context, n int) ([]domain.Incoming, error) {
	return selectJSON[domain.Incoming](ctx, s.pool, "SELECT data FROM openalbert_incoming WHERE NOT handled ORDER BY seq LIMIT $1", limitValue(n))
}
func (s *Store) Incoming(ctx context.Context, id string) (domain.Incoming, bool, error) {
	return oneJSON[domain.Incoming](ctx, s.pool, "SELECT data FROM openalbert_incoming WHERE id=$1", id)
}
func (s *Store) MarkIncomingHandled(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, "UPDATE openalbert_incoming SET handled=true WHERE id=$1", id)
	return err
}
func (s *Store) Conversation(ctx context.Context, id string) (domain.Conversation, bool, error) {
	return oneJSON[domain.Conversation](ctx, s.pool, "SELECT data FROM openalbert_conversations WHERE id=$1", id)
}
func (s *Store) ListPrivateConversations(ctx context.Context) ([]domain.Conversation, error) {
	return selectJSON[domain.Conversation](ctx, s.pool, "SELECT data FROM openalbert_conversations WHERE (data->>'private')::boolean ORDER BY id")
}
func (s *Store) ListConversations(ctx context.Context) ([]domain.Conversation, error) {
	return selectJSON[domain.Conversation](ctx, s.pool, "SELECT data FROM openalbert_conversations ORDER BY id")
}

// Tool-based replies are stored as assistant send_message calls, not plain content.
// Their presence records a reply request, not proof of Telegram delivery.
func historyContent(m domain.Message) string {
	parts := []string{}
	if m.Content != "" {
		parts = append(parts, m.Content)
	}
	if m.Role == "assistant" {
		for _, call := range m.ToolCalls {
			if call.Name != "send_message" {
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

func historyLimit(n int) int {
	if n <= 0 || n > 50 {
		return 20
	}
	return n
}

// Results are newest first; before is an exclusive cursor into older messages.
func (s *Store) ConversationHistory(ctx context.Context, scope, query string, before int64, n int) ([]domain.HistoryMessage, error) {
	rows, err := s.pool.Query(ctx, `WITH history AS (
 SELECT m.seq, s.id, s.data AS session_data, m.data AS message_data,
 concat_ws(E'\n', NULLIF(m.data->>'content',''),
  CASE WHEN m.data->>'role'='assistant' THEN (
   SELECT string_agg(call->'arguments'->>'text', E'\n')
   FROM jsonb_array_elements(COALESCE(m.data->'tool_calls','[]')) call
   WHERE call->>'name'='send_message'
  ) END) AS content
 FROM openalbert_messages m JOIN openalbert_sessions s ON s.id=m.session_id
 WHERE s.conversation_id=$1 AND ($3::bigint=0 OR m.seq<$3)
 AND m.data->>'role' IN ('user','assistant')
)
SELECT seq,id,session_data,message_data FROM history WHERE content<>''
AND ($2='' OR strpos(lower(content),lower($2))>0)
ORDER BY seq DESC LIMIT $4`, scope, query, before, historyLimit(n))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.HistoryMessage{}
	for rows.Next() {
		var item domain.HistoryMessage
		var sessionData, messageData []byte
		if err := rows.Scan(&item.Seq, &item.SessionID, &sessionData, &messageData); err != nil {
			return nil, err
		}
		session, err := decode[domain.Session](sessionData)
		if err != nil {
			return nil, err
		}
		message, err := decode[domain.Message](messageData)
		if err != nil {
			return nil, err
		}
		item.ConversationID, item.SessionStartedAt = scope, session.CreatedAt
		item.AuthorID = message.AuthorID
		item.Role, item.Content = message.Role, historyContent(message)
		item.ReplyRequested = len(message.ToolCalls) > 0 && message.Role == "assistant"
		out = append(out, item)
	}
	return out, rows.Err()
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
	_, err = s.pool.Exec(ctx, "INSERT INTO openalbert_sessions(id,conversation_id,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", se.ID, se.ConversationID, b)
	return err
}
func (s *Store) EndSession(ctx context.Context, id, summary string) error {
	tag, err := s.pool.Exec(ctx, "UPDATE openalbert_sessions SET data=jsonb_set(jsonb_set(data,'{status}','\"closed\"'::jsonb),'{summary}',to_jsonb($2::text)) WHERE id=$1 AND (data->>'status'='active' OR (COALESCE(data->>'summary','')='' AND $2<>''))", id, summary)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	_, ok, err := oneJSON[domain.Session](ctx, s.pool, "SELECT data FROM openalbert_sessions WHERE id=$1", id)
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}
func (s *Store) PendingAnalysis(ctx context.Context, n int) ([]domain.Session, error) {
	return selectJSON[domain.Session](ctx, s.pool, "SELECT data FROM openalbert_sessions WHERE NOT analyzed AND data->>'status'='closed' ORDER BY seq LIMIT $1", limitValue(n))
}
func (s *Store) MarkAnalyzed(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, "UPDATE openalbert_sessions SET analyzed=true WHERE id=$1 AND data->>'status'='closed'", id)
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
	_, err = s.pool.Exec(ctx, "INSERT INTO openalbert_messages(session_id,mutation_key,data,recorded_at) VALUES($1,$2,$3,now()) ON CONFLICT DO NOTHING", session, key, b)
	return err
}
func (s *Store) Messages(ctx context.Context, session string) ([]domain.Message, error) {
	return selectJSON[domain.Message](ctx, s.pool, "SELECT data FROM openalbert_messages WHERE session_id=$1 ORDER BY seq", session)
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
	_, err = s.pool.Exec(ctx, "INSERT INTO openalbert_tasks(conversation_id,id,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", t.ConversationID, t.ID, b)
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
	if t.ScheduleSeconds < 0 {
		return errors.New("negative task interval")
	}
	return nil
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
	var raw []byte
	err = s.pool.QueryRow(ctx, "UPDATE openalbert_tasks SET data=jsonb_set($3::jsonb,'{created_at}',data->'created_at') WHERE conversation_id=$1 AND id=$2 AND COALESCE((data->>'version')::bigint,1)=$4 RETURNING data", t.ConversationID, t.ID, b, expectedVersion).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
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
	return decode[domain.Task](raw)
}
func (s *Store) FindTasks(ctx context.Context, scope, q string) ([]domain.Task, error) {
	return selectJSON[domain.Task](ctx, s.pool, "SELECT data FROM openalbert_tasks WHERE conversation_id=$1 AND (strpos(lower(data->>'title'),lower($2))>0 OR strpos(lower(data->>'goal'),lower($2))>0 OR id=$2) ORDER BY seq", scope, q)
}
func (s *Store) Task(ctx context.Context, scope, id string) (domain.Task, bool, error) {
	return oneJSON[domain.Task](ctx, s.pool, "SELECT data FROM openalbert_tasks WHERE conversation_id=$1 AND id=$2", scope, id)
}
func (s *Store) RunnableTasks(ctx context.Context) ([]domain.Task, error) {
	return selectJSON[domain.Task](ctx, s.pool, "SELECT data FROM openalbert_tasks WHERE data->>'status' IN ('queued','running','waiting') ORDER BY seq")
}
func (s *Store) EnqueueOutgoing(ctx context.Context, o domain.Outgoing) error {
	if err := validID(o.ID); err != nil {
		return err
	}
	if err := validID(o.Conversation.ID); err != nil {
		return err
	}
	o.State = "pending"
	o.Attempts = 0
	b, err := encode(o)
	if err != nil {
		return err
	}
	if o.RecordInHistory {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		tag, err := tx.Exec(ctx, "INSERT INTO openalbert_outgoing(id,data) VALUES($1,$2) ON CONFLICT DO NOTHING", o.ID, b)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			session, message := outgoingHistory(o)
			se, err := encode(session)
			if err != nil {
				return err
			}
			msg, err := encode(message)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "INSERT INTO openalbert_sessions(id,conversation_id,data,analyzed) VALUES($1,$2,$3,true)", session.ID, session.ConversationID, se); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "INSERT INTO openalbert_messages(session_id,mutation_key,data,recorded_at) VALUES($1,$2,$3,now())", session.ID, o.ID, msg); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	_, err = s.pool.Exec(ctx, "INSERT INTO openalbert_outgoing(id,data) VALUES($1,$2) ON CONFLICT DO NOTHING", o.ID, b)
	return err
}
func (s *Store) PendingOutgoing(ctx context.Context, n int) ([]domain.Outgoing, error) {
	rows, err := s.pool.Query(ctx, "SELECT data,state,attempts FROM openalbert_outgoing WHERE state='pending' ORDER BY seq LIMIT $1", limitValue(n))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Outgoing{}
	for rows.Next() {
		var b []byte
		var state string
		var attempts int
		if err = rows.Scan(&b, &state, &attempts); err != nil {
			return nil, err
		}
		v, e := decode[domain.Outgoing](b)
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current string
	err = tx.QueryRow(ctx, "SELECT state FROM openalbert_outgoing WHERE id=$1 FOR UPDATE", id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
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
	_, err = tx.Exec(ctx, "UPDATE openalbert_outgoing SET state=$2,attempts=attempts+CASE WHEN $2='sending' THEN 1 ELSE 0 END,data=jsonb_set(jsonb_set(data,'{state}',to_jsonb($2::text)),'{attempts}',to_jsonb(attempts+CASE WHEN $2='sending' THEN 1 ELSE 0 END)) WHERE id=$1", id, state)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	b, err := encode(a)
	if err != nil {
		return err
	}
	data := a.Data
	if data == nil {
		data = []byte{}
	}
	_, err = s.pool.Exec(ctx, "INSERT INTO openalbert_artifacts(conversation_id,id,data,content) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING", a.ConversationID, a.ID, b, data)
	return err
}
func (s *Store) Artifact(ctx context.Context, scope, id string) (domain.Artifact, bool, error) {
	var b, content []byte
	err := s.pool.QueryRow(ctx, "SELECT data,content FROM openalbert_artifacts WHERE conversation_id=$1 AND id=$2", scope, id).Scan(&b, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Artifact{}, false, nil
	}
	if err != nil {
		return domain.Artifact{}, false, err
	}
	a, err := decode[domain.Artifact](b)
	a.Data = content
	return a, err == nil, err
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func oneJSON[T any](ctx context.Context, q queryer, sql string, args ...any) (T, bool, error) {
	var b []byte
	var zero T
	err := q.QueryRow(ctx, sql, args...).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	v, err := decode[T](b)
	return v, err == nil, err
}
func selectJSON[T any](ctx context.Context, q queryer, sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		v, e := decode[T](b)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) LoadCheckpoint(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT data FROM openalbert_checkpoints WHERE id=$1", key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return json.RawMessage(raw), err == nil, err
}
func (s *Store) SaveCheckpoint(ctx context.Context, key string, value json.RawMessage) error {
	if err := validID(key); err != nil {
		return err
	}
	if !json.Valid(value) {
		return errors.New("invalid checkpoint JSON")
	}
	_, err := s.pool.Exec(ctx, "INSERT INTO openalbert_checkpoints(id,data) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET data=excluded.data", key, []byte(value))
	return err
}

func (s *Store) ActiveSessions(ctx context.Context) ([]domain.Session, error) {
	rows, err := s.pool.Query(ctx, "SELECT data FROM openalbert_sessions WHERE data->>'status'='active'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Session{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		session, err := decode[domain.Session](b)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}
