package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/inspector"
)

func (s *Store) SaveTrace(ctx context.Context, tr domain.Trace) error {
	return upsertTraceSQL(s.exec, ctx, tr)
}

const sessionTriggerSQL = `COALESCE(
 (SELECT tr.trigger_kind FROM openalbert_traces tr WHERE tr.session_id = s.id AND tr.trigger_kind IS NOT NULL ORDER BY tr.started_at ASC LIMIT 1),
 CASE
  WHEN s.conversation_id LIKE 'coding:%' THEN 'coding'
  ELSE COALESCE(
   (SELECT CASE WHEN cp.turn_task_id IS NOT NULL THEN 'task' WHEN cp.turn_initiative = 1 THEN 'initiative' ELSE 'message' END
    FROM openalbert_checkpoints cp
    WHERE cp.turn_session_id IS NOT NULL AND cp.turn_session_id = s.id
    LIMIT 1),
   'message'
  )
 END
)`

const sessionColumns = `s.seq, s.data, COALESCE(c.data, '{}'), s.analyzed,
 (SELECT count(*) FROM openalbert_messages m WHERE m.session_id = s.id),
 (SELECT COALESCE(sum(CASE WHEN json_type(m.data, '$.tool_calls') = 'array' THEN json_array_length(m.data, '$.tool_calls') ELSE 0 END), 0) FROM openalbert_messages m WHERE m.session_id = s.id),
 (SELECT count(*) FROM openalbert_traces tr WHERE tr.session_id = s.id),
 ` + sessionTriggerSQL + `,
 COALESCE(
  (SELECT substr(json_extract(m.data, '$.content'), 1, 180) FROM openalbert_messages m WHERE m.session_id = s.id AND json_extract(m.data, '$.role') = 'user' ORDER BY m.seq ASC LIMIT 1),
  substr(json_extract(s.data, '$.summary'), 1, 180),
  ''
 )`

type scanner interface{ Scan(...any) error }

func scanInspectionSession(row scanner) (out inspector.Session, err error) {
	var se, co string
	var analyzed int
	err = row.Scan(&out.Seq, &se, &co, &analyzed, &out.MessageCount, &out.ToolCount, &out.TraceCount, &out.Trigger, &out.Preview)
	if err != nil {
		return
	}
	out.Analyzed = analyzed != 0
	err = json.Unmarshal([]byte(se), &out.Session)
	if err != nil {
		return
	}
	err = json.Unmarshal([]byte(co), &out.Conversation)
	return
}

func (s *Store) InspectSessions(ctx context.Context, f inspector.Filter) ([]inspector.Session, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	baseSQL := `SELECT ` + sessionColumns + ` FROM openalbert_sessions s LEFT JOIN openalbert_conversations c ON c.id = s.conversation_id
 WHERE (?1 = 0 OR s.seq < ?1)
   AND (?2 = '' OR json_extract(s.data, '$.status') = ?2)
   AND (?3 = '' OR s.conversation_id = ?3)
   AND (?4 = '' OR ` + sessionTriggerSQL + ` = ?4)
 ORDER BY s.seq DESC`
	if f.Query == "" {
		rows, err := s.query(ctx, baseSQL+` LIMIT ?5`, f.Before, f.Status, f.Conversation, f.Trigger, f.Limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []inspector.Session{}
		for rows.Next() {
			v, e := scanInspectionSession(rows)
			if e != nil {
				return nil, e
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}

	rows, err := s.query(ctx, baseSQL, f.Before, f.Status, f.Conversation, f.Trigger)
	if err != nil {
		return nil, err
	}
	candidates := []inspector.Session{}
	for rows.Next() {
		v, e := scanInspectionSession(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		candidates = append(candidates, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	lowerQ := strings.ToLower(f.Query)
	out := []inspector.Session{}
	for _, cand := range candidates {
		header := cand.ID + " " + cand.ConversationID + " " + cand.Conversation.Title + " " + cand.Summary
		matched := strings.Contains(strings.ToLower(header), lowerQ)
		if !matched {
			msgRows, err := s.query(ctx, `SELECT data FROM openalbert_messages WHERE session_id = ?1`, cand.ID)
			if err != nil {
				return nil, err
			}
			for msgRows.Next() {
				var raw string
				if err := msgRows.Scan(&raw); err != nil {
					msgRows.Close()
					return nil, err
				}
				if strings.Contains(strings.ToLower(raw), lowerQ) {
					matched = true
					break
				}
			}
			if err := msgRows.Err(); err != nil {
				msgRows.Close()
				return nil, err
			}
			msgRows.Close()
		}
		if matched {
			out = append(out, cand)
			if len(out) >= f.Limit {
				break
			}
		}
	}
	return out, nil
}

func (s *Store) InspectSession(ctx context.Context, id string, after int64) (out inspector.Detail, found bool, err error) {
	var exists int
	if err = s.queryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openalbert_sessions WHERE id = ?1)`, id).Scan(&exists); err != nil || exists == 0 {
		return
	}
	found = true
	out.Session, err = scanInspectionSession(s.queryRow(ctx, `SELECT `+sessionColumns+` FROM openalbert_sessions s LEFT JOIN openalbert_conversations c ON c.id = s.conversation_id WHERE s.id = ?1`, id))
	if err != nil {
		return
	}
	out.Messages = []inspector.SavedMessage{}
	rows, e := s.query(ctx, `SELECT seq, mutation_key, recorded_at, data FROM openalbert_messages WHERE session_id = ?1 AND seq > ?2 ORDER BY seq ASC LIMIT 201`, id, after)
	if e != nil {
		err = e
		return
	}
	for rows.Next() {
		var v inspector.SavedMessage
		var recordedAt sql.NullString
		var raw string
		if err = rows.Scan(&v.Seq, &v.Key, &recordedAt, &raw); err != nil {
			break
		}
		if recordedAt.Valid {
			if t, pe := parseTime(recordedAt.String); pe == nil {
				v.RecordedAt = &t
			}
		}
		if err = json.Unmarshal([]byte(raw), &v.Message); err != nil {
			break
		}
		out.Messages = append(out.Messages, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return
	}
	if len(out.Messages) > 200 {
		out.Messages = out.Messages[:200]
		out.NextAfter = out.Messages[199].Seq
	}
	out.Traces, err = selectJSON[domain.Trace](ctx, s, `SELECT json_remove(data, '$.input', '$.output') FROM openalbert_traces WHERE session_id = ?1 ORDER BY started_at ASC, id ASC`, id)
	if err != nil {
		return
	}
	if out.Traces == nil {
		out.Traces = []domain.Trace{}
	}
	out.Turns, err = selectJSON[json.RawMessage](ctx, s, `SELECT json_remove(data, '$.Messages') FROM openalbert_checkpoints WHERE turn_session_id = ?1 AND id LIKE 'turn-done/%' ORDER BY COALESCE(turn_started_at, ''), id ASC`, id)
	if err != nil {
		return
	}
	if out.Turns == nil {
		out.Turns = []json.RawMessage{}
	}
	out.Analysis, _, err = s.LoadCheckpoint(ctx, "analysis/"+id)
	if err != nil {
		return
	}

	// Legacy completions still exist in checkpoints. They have no request snapshot,
	// duration or reliable per-message timestamp; label that limitation explicitly.
	legacy, e := s.query(ctx, `WITH turn_prefixes AS (
 SELECT DISTINCT 'model/' || json_extract(data, '$.Input.Key') || '/' AS prefix
 FROM openalbert_checkpoints
 WHERE turn_session_id = ?1
   AND id LIKE 'turn-done/%'
   AND id NOT LIKE 'turn-done/coding/%'
   AND COALESCE(json_extract(data, '$.Input.Key'), '') <> ''
)
SELECT m.id
FROM openalbert_checkpoints m
WHERE m.id LIKE 'model/%'
  AND EXISTS(SELECT 1 FROM turn_prefixes t WHERE substr(m.id, 1, length(t.prefix)) = t.prefix)`, id)
	if e != nil {
		err = e
		return
	}
	var legacyKeys []string
	for legacy.Next() {
		var key string
		if err = legacy.Scan(&key); err != nil {
			break
		}
		legacyKeys = append(legacyKeys, key)
	}
	if err == nil {
		err = legacy.Err()
	}
	legacy.Close()
	if err != nil {
		return
	}
	if len(legacyKeys) > 0 {
		msgSeqs := map[string]int64{}
		seqRows, se := s.query(ctx, `SELECT mutation_key, MIN(seq) FROM openalbert_messages WHERE session_id = ?1 GROUP BY mutation_key`, id)
		if se != nil {
			err = se
			return
		}
		for seqRows.Next() {
			var mk string
			var seq int64
			if err = seqRows.Scan(&mk, &seq); err != nil {
				break
			}
			msgSeqs[mk] = seq
		}
		if err == nil {
			err = seqRows.Err()
		}
		seqRows.Close()
		if err != nil {
			return
		}
		keyOrder := func(key string) (int64, bool) {
			parts := strings.Split(key, "/")
			if len(parts) != 3 {
				return 0, false
			}
			seq, ok := msgSeqs[parts[1]+"/assistant/"+parts[2]]
			return seq, ok
		}
		sort.Slice(legacyKeys, func(i, j int) bool {
			si, oki := keyOrder(legacyKeys[i])
			sj, okj := keyOrder(legacyKeys[j])
			if oki != okj {
				return oki
			}
			if oki && si != sj {
				return si < sj
			}
			return legacyKeys[i] < legacyKeys[j]
		})
	}
	historical := []domain.Trace{}
	for _, key := range legacyKeys {
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			continue
		}
		var step int
		_, _ = fmt.Sscan(parts[2], &step)
		exists := false
		for _, tr := range out.Traces {
			if tr.Kind == "model" && tr.TurnID == parts[1] && tr.Step == step && tr.Status == "completed" {
				exists = true
				break
			}
		}
		if !exists {
			historical = append(historical, domain.Trace{ID: key, SessionID: id, TurnID: parts[1], Kind: "model", Name: "Модель не сохранена", Step: step, Status: "completed", Historical: true})
		}
	}
	out.Traces = append(historical, out.Traces...)

	out.Outgoing, err = selectJSON[domain.Outgoing](ctx, s, `SELECT json_set(data, '$.state', state, '$.attempts', attempts) FROM openalbert_outgoing WHERE session_id = ?1 ORDER BY seq ASC`, id)
	if err != nil {
		return
	}
	if out.Outgoing == nil {
		out.Outgoing = []domain.Outgoing{}
	}
	out.Audio, err = selectJSON[inspector.AudioResult](ctx, s, `SELECT CASE
 WHEN i.id IS NOT NULL THEN json_set(c.data, '$.original_text', json_extract(i.data, '$.text'), '$.received_at', json_extract(i.data, '$.created_at'))
 ELSE c.data
END
FROM openalbert_checkpoints c
LEFT JOIN openalbert_incoming i
 ON i.id = json_extract(c.data, '$.incoming_id')
 AND i.conversation_id = ?2
WHERE c.id LIKE 'audio/%' AND c.audio_session_id = ?1
ORDER BY COALESCE(c.audio_queued_at, ''), c.id ASC`, id, out.Session.ConversationID)
	if err != nil {
		return
	}
	if out.Audio == nil {
		out.Audio = []inspector.AudioResult{}
	}
	// Session artifacts are referenced in incoming/model/tool messages. Scope the
	// lookup to that conversation and exact IDs mentioned in this session.
	out.Artifacts, err = selectJSON[domain.Artifact](ctx, s, `WITH message_texts AS (
 SELECT data AS content FROM openalbert_messages WHERE session_id = ?1
)
SELECT a.data FROM openalbert_artifacts a
WHERE a.conversation_id = ?2
  AND (
   a.session_id = ?1
   OR EXISTS(SELECT 1 FROM message_texts m WHERE instr(m.content, a.id) > 0)
   OR EXISTS(SELECT 1 FROM openalbert_checkpoints c WHERE c.id LIKE 'audio/%' AND c.audio_session_id = ?1 AND json_extract(c.data, '$.artifact_id') = a.id)
  )
ORDER BY a.created_at ASC, a.id ASC`, id, out.Session.ConversationID)
	if out.Artifacts == nil {
		out.Artifacts = []domain.Artifact{}
	}
	return
}

func (s *Store) InspectTrace(ctx context.Context, session, id string) (tr domain.Trace, found bool, err error) {
	if !strings.HasPrefix(id, "model/") {
		return oneJSON[domain.Trace](ctx, s, `SELECT data FROM openalbert_traces WHERE session_id = ?1 AND id = ?2`, session, id)
	}
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return
	}
	var raw json.RawMessage
	raw, found, err = oneJSON[json.RawMessage](ctx, s, `SELECT m.data
FROM openalbert_checkpoints m
WHERE m.id = ?2
  AND EXISTS(
   SELECT 1 FROM openalbert_checkpoints t
   WHERE t.turn_session_id = ?1
     AND t.id LIKE 'turn-done/%'
     AND t.id NOT LIKE 'turn-done/coding/%'
     AND COALESCE(json_extract(t.data, '$.Input.Key'), '') <> ''
     AND substr(m.id, 1, length('model/' || json_extract(t.data, '$.Input.Key') || '/')) = 'model/' || json_extract(t.data, '$.Input.Key') || '/'
  )`, session, id)
	if !found || err != nil {
		return
	}
	var step int
	_, _ = fmt.Sscan(parts[2], &step)
	tr = domain.Trace{ID: id, SessionID: session, TurnID: parts[1], Kind: "model", Step: step, Status: "completed", Historical: true, Output: domain.SanitizeModelCheckpointJSON(raw)}
	return
}

var _ inspector.MemoryMetadataStore = (*Store)(nil)
var _ inspector.SearchUsageStore = (*Store)(nil)

const inspectSearchUsageSQL = `SELECT id, session_id, started_at,
 search_outcome_at,
 COALESCE(trace_status, ''), COALESCE(trigger_kind, ''), COALESCE(task_id, ''),
 search_provider, COALESCE(search_mode, ''), COALESCE(search_resolved_mode, ''),
 search_cost_usd, COALESCE(search_http_status, 0), COALESCE(search_results, 0),
 COALESCE(search_rate_limited, 0)
FROM openalbert_traces
WHERE search_provider IS NOT NULL
 AND search_outcome_at >= ?1
 AND search_outcome_at < ?2
ORDER BY search_outcome_at DESC, id DESC`

// InspectSearchUsage reads scalar web_search trace metrics over [from, to) using
// the partial covering index without reading full trace JSON payloads.
func (s *Store) InspectSearchUsage(ctx context.Context, from, to time.Time) ([]inspector.SearchUsageRecord, error) {
	rows, err := s.query(ctx, inspectSearchUsageSQL, formatTime(from.UTC()), formatTime(to.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []inspector.SearchUsageRecord{}
	for rows.Next() {
		var r inspector.SearchUsageRecord
		var startedAtStr, completedAtStr string
		var costUSD sql.NullFloat64
		var rateLimited int
		if err := rows.Scan(
			&r.TraceID,
			&r.SessionID,
			&startedAtStr,
			&completedAtStr,
			&r.Status,
			&r.Trigger,
			&r.TaskID,
			&r.Provider,
			&r.Mode,
			&r.ResolvedMode,
			&costUSD,
			&r.HTTPStatus,
			&r.ResultCount,
			&rateLimited,
		); err != nil {
			return nil, err
		}
		if t, err := parseTime(startedAtStr); err == nil {
			r.StartedAt = t
		}
		if t, err := parseTime(completedAtStr); err == nil {
			r.CompletedAt = t
		}
		if costUSD.Valid {
			v := costUSD.Float64
			r.CostUSD = &v
		}
		r.RateLimited = rateLimited != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

const inspectSessionOriginsSQL = `WITH scoped AS (
 SELECT s.id AS session_id, s.conversation_id,
  json_extract(s.data, '$.created_at') AS created_at
 FROM openalbert_sessions s
 LEFT JOIN openalbert_conversations c ON c.id = s.conversation_id
 WHERE s.id IN (SELECT value FROM json_each(?2))
  AND (
   s.conversation_id = ?1
   OR (?1 LIKE 'personal:%' AND s.conversation_id LIKE 'coding:%' AND 'personal:' || COALESCE(CAST(json_extract(c.data, '$.owner_id') AS TEXT), '') = ?1)
  )
),
resolved AS (
 SELECT sc.session_id, sc.conversation_id, sc.created_at,
  COALESCE(
   (SELECT tr.trigger_kind FROM openalbert_traces tr WHERE tr.session_id = sc.session_id AND tr.trigger_kind IS NOT NULL ORDER BY tr.started_at ASC LIMIT 1),
   CASE
    WHEN sc.conversation_id LIKE 'coding:%' THEN 'coding'
    ELSE COALESCE(
     (SELECT CASE WHEN cp.turn_task_id IS NOT NULL THEN 'task' WHEN cp.turn_initiative = 1 THEN 'initiative' ELSE 'message' END
      FROM openalbert_checkpoints cp
      WHERE cp.turn_session_id IS NOT NULL AND cp.turn_session_id = sc.session_id
      LIMIT 1),
     'message'
    )
   END
  ) AS trigger,
  COALESCE(
   (SELECT tr.task_id FROM openalbert_traces tr WHERE tr.session_id = sc.session_id AND tr.task_id IS NOT NULL ORDER BY tr.started_at ASC LIMIT 1),
   (SELECT cp.turn_task_id FROM openalbert_checkpoints cp WHERE cp.turn_session_id IS NOT NULL AND cp.turn_session_id = sc.session_id AND cp.turn_task_id IS NOT NULL LIMIT 1),
   ''
  ) AS task_id
 FROM scoped sc
)
SELECT r.session_id, r.trigger, r.task_id, COALESCE(json_extract(t.data, '$.title'), '') AS task_title, r.created_at
FROM resolved r
LEFT JOIN openalbert_tasks t ON t.conversation_id = r.conversation_id AND t.id = r.task_id AND r.task_id <> ''`

const inspectLatestConsolidationSQL = `SELECT id, data FROM openalbert_checkpoints
 WHERE consolidation_scope IS NOT NULL
  AND consolidation_scope = ?1
 ORDER BY consolidation_completed_at DESC, id DESC
 LIMIT 1`

// InspectSessionOrigins resolves session triggers, creation timestamps, and
// originating task metadata for a scope's memory entries in a single batched
// query, including owner-matched coding sessions for personal scopes while
// enforcing scope isolation across conversations and owners.
func (s *Store) InspectSessionOrigins(ctx context.Context, scope string, sessionIDs []string) (map[string]inspector.SessionOrigin, error) {
	out := make(map[string]inspector.SessionOrigin, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	idsJSON, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, inspectSessionOriginsSQL, scope, string(idsJSON))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o inspector.SessionOrigin
		var createdAt sql.NullString
		if err := rows.Scan(&o.SessionID, &o.Trigger, &o.TaskID, &o.TaskTitle, &createdAt); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			if t, ok := parseFlexibleTime(createdAt.String); ok {
				o.CreatedAt = t
			}
		}
		out[o.SessionID] = o
	}
	return out, rows.Err()
}

// InspectLatestConsolidation returns the latest memory consolidation checkpoint
// recorded for the given conversation scope, if one exists.
func (s *Store) InspectLatestConsolidation(ctx context.Context, scope string) (string, json.RawMessage, bool, error) {
	var id, raw string
	err := s.queryRow(ctx, inspectLatestConsolidationSQL, scope).Scan(&id, &raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, false, nil
		}
		return "", nil, false, err
	}
	return id, json.RawMessage([]byte(raw)), true, nil
}
