package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"openalbert/internal/domain"
	"openalbert/internal/inspector"
)

func (s *Store) SaveTrace(ctx context.Context, tr domain.Trace) error {
	raw, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO openalbert_traces(id,session_id,turn_id,started_at,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET data=EXCLUDED.data`, tr.ID, tr.SessionID, tr.TurnID, tr.StartedAt, raw)
	return err
}

const sessionColumns = `s.seq,s.data,COALESCE(c.data,'{}'),s.analyzed,
 (SELECT count(*) FROM openalbert_messages m WHERE m.session_id=s.id),
 (SELECT COALESCE(sum(jsonb_array_length(COALESCE(m.data->'tool_calls','[]'))),0) FROM openalbert_messages m WHERE m.session_id=s.id),
 (SELECT count(*) FROM openalbert_traces tr WHERE tr.session_id=s.id),
 COALESCE((SELECT tr.data->>'trigger' FROM openalbert_traces tr WHERE tr.session_id=s.id AND tr.data->>'trigger' IN ('message','task','initiative') ORDER BY tr.started_at LIMIT 1),
 (SELECT CASE WHEN COALESCE(cp.data->'Input'->>'TaskID','')<>'' THEN 'task' WHEN cp.data->'Input'->>'Initiative'='true' THEN 'initiative' ELSE 'message' END FROM openalbert_checkpoints cp WHERE cp.id LIKE 'turn-done/%' AND cp.data->'Input'->>'SessionID'=s.id LIMIT 1),'message'),
 COALESCE((SELECT left(m.data->>'content',180) FROM openalbert_messages m WHERE m.session_id=s.id AND m.data->>'role'='user' ORDER BY m.seq LIMIT 1),left(s.data->>'summary',180),'')`

type scanner interface{ Scan(...any) error }

func scanInspectionSession(row scanner) (out inspector.Session, err error) {
	var se, co []byte
	err = row.Scan(&out.Seq, &se, &co, &out.Analyzed, &out.MessageCount, &out.ToolCount, &out.TraceCount, &out.Trigger, &out.Preview)
	if err != nil {
		return
	}
	err = json.Unmarshal(se, &out.Session)
	if err != nil {
		return
	}
	err = json.Unmarshal(co, &out.Conversation)
	return
}

func (s *Store) InspectSessions(ctx context.Context, f inspector.Filter) ([]inspector.Session, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sessionColumns+` FROM openalbert_sessions s LEFT JOIN openalbert_conversations c ON c.id=s.conversation_id
 WHERE ($1::bigint=0 OR s.seq<$1) AND ($2='' OR s.data->>'status'=$2) AND ($3='' OR s.conversation_id=$3)
 AND ($4='' OR strpos(lower(s.id||' '||s.conversation_id||' '||COALESCE(c.data->>'title','')||' '||COALESCE(s.data->>'summary','')),lower($4))>0
 OR EXISTS(SELECT 1 FROM openalbert_messages m WHERE m.session_id=s.id AND strpos(lower(m.data::text),lower($4))>0))
 AND ($5='' OR COALESCE((SELECT tr.data->>'trigger' FROM openalbert_traces tr WHERE tr.session_id=s.id AND tr.data->>'trigger' IN ('message','task','initiative') ORDER BY tr.started_at LIMIT 1),
 (SELECT CASE WHEN COALESCE(cp.data->'Input'->>'TaskID','')<>'' THEN 'task' WHEN cp.data->'Input'->>'Initiative'='true' THEN 'initiative' ELSE 'message' END FROM openalbert_checkpoints cp WHERE cp.id LIKE 'turn-done/%' AND cp.data->'Input'->>'SessionID'=s.id LIMIT 1),'message')=$5)
 ORDER BY s.seq DESC LIMIT $6`, f.Before, f.Status, f.Conversation, f.Query, f.Trigger, f.Limit)
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

func (s *Store) InspectSession(ctx context.Context, id string, after int64) (out inspector.Detail, found bool, err error) {
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM openalbert_sessions WHERE id=$1)`, id).Scan(&found); err != nil || !found {
		return
	}
	out.Session, err = scanInspectionSession(s.pool.QueryRow(ctx, `SELECT `+sessionColumns+` FROM openalbert_sessions s LEFT JOIN openalbert_conversations c ON c.id=s.conversation_id WHERE s.id=$1`, id))
	if err != nil {
		return
	}
	out.Messages = []inspector.SavedMessage{}
	rows, e := s.pool.Query(ctx, `SELECT seq,mutation_key,recorded_at,data FROM openalbert_messages WHERE session_id=$1 AND seq>$2 ORDER BY seq LIMIT 201`, id, after)
	if e != nil {
		err = e
		return
	}
	for rows.Next() {
		var v inspector.SavedMessage
		var raw []byte
		if err = rows.Scan(&v.Seq, &v.Key, &v.RecordedAt, &raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &v.Message); err != nil {
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
	out.Traces, err = selectJSON[domain.Trace](ctx, s.pool, `SELECT data-'input'-'output' FROM openalbert_traces WHERE session_id=$1 ORDER BY started_at,id`, id)
	if err != nil {
		return
	}
	if out.Traces == nil {
		out.Traces = []domain.Trace{}
	}
	out.Turns, err = selectJSON[json.RawMessage](ctx, s.pool, `SELECT data-'Messages' FROM openalbert_checkpoints WHERE id LIKE 'turn-done/%' AND data->'Input'->>'SessionID'=$1 ORDER BY data->'Input'->>'StartedAt',id`, id)
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
	legacy, e := s.pool.Query(ctx, `SELECT m.id,m.data FROM openalbert_checkpoints m WHERE m.id LIKE 'model/%' AND EXISTS(SELECT 1 FROM openalbert_checkpoints t WHERE t.id LIKE 'turn-done/%' AND t.data->'Input'->>'SessionID'=$1 AND starts_with(m.id,'model/'||(t.data->'Input'->>'Key')||'/')) ORDER BY (SELECT min(msg.seq) FROM openalbert_messages msg WHERE msg.session_id=$1 AND msg.mutation_key=split_part(m.id,'/',2)||'/assistant/'||split_part(m.id,'/',3)) NULLS LAST,m.id`, id)
	if e != nil {
		err = e
		return
	}
	historical := []domain.Trace{}
	for legacy.Next() {
		var key string
		var raw json.RawMessage
		if err = legacy.Scan(&key, &raw); err != nil {
			break
		}
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
	if err == nil {
		err = legacy.Err()
	}
	legacy.Close()
	out.Traces = append(historical, out.Traces...)
	if err != nil {
		return
	}
	out.Outgoing, err = selectJSON[domain.Outgoing](ctx, s.pool, `SELECT data||jsonb_build_object('state',state,'attempts',attempts) FROM openalbert_outgoing WHERE data->>'session_id'=$1 ORDER BY seq`, id)
	if err != nil {
		return
	}
	if out.Outgoing == nil {
		out.Outgoing = []domain.Outgoing{}
	}
	out.Audio, err = selectJSON[domain.AudioResult](ctx, s.pool, `SELECT data FROM openalbert_checkpoints WHERE id LIKE 'audio/%' AND data->>'session_id'=$1 ORDER BY data->>'queued_at',id`, id)
	if err != nil {
		return
	}
	if out.Audio == nil {
		out.Audio = []domain.AudioResult{}
	}
	// Session artifacts are referenced in incoming/model/tool messages. Scope the
	// lookup to that conversation and exact IDs mentioned in this session.
	out.Artifacts, err = selectJSON[domain.Artifact](ctx, s.pool, `SELECT a.data FROM openalbert_artifacts a WHERE a.conversation_id=$2 AND (a.data->>'session_id'=$1 OR EXISTS(SELECT 1 FROM openalbert_messages m WHERE m.session_id=$1 AND strpos(m.data::text,a.id)>0) OR EXISTS(SELECT 1 FROM openalbert_checkpoints c WHERE c.id LIKE 'audio/%' AND c.data->>'session_id'=$1 AND c.data->>'artifact_id'=a.id)) ORDER BY a.data->>'created_at'`, id, out.Session.ConversationID)
	if out.Artifacts == nil {
		out.Artifacts = []domain.Artifact{}
	}
	return
}

func (s *Store) InspectTrace(ctx context.Context, session, id string) (tr domain.Trace, found bool, err error) {
	if !strings.HasPrefix(id, "model/") {
		return oneJSON[domain.Trace](ctx, s.pool, `SELECT data FROM openalbert_traces WHERE session_id=$1 AND id=$2`, session, id)
	}
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return
	}
	var raw json.RawMessage
	raw, found, err = oneJSON[json.RawMessage](ctx, s.pool, `SELECT m.data FROM openalbert_checkpoints m WHERE m.id=$2 AND EXISTS(SELECT 1 FROM openalbert_checkpoints t WHERE t.id LIKE 'turn-done/%' AND t.data->'Input'->>'SessionID'=$1 AND starts_with(m.id,'model/'||(t.data->'Input'->>'Key')||'/'))`, session, id)
	if !found || err != nil {
		return
	}
	var step int
	_, _ = fmt.Sscan(parts[2], &step)
	tr = domain.Trace{ID: id, SessionID: session, TurnID: parts[1], Kind: "model", Step: step, Status: "completed", Historical: true, Output: raw}
	return
}
