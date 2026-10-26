package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func newProgress(p domain.Progress) domain.Progress {
	p.StartedAt = nowIfZero(p.StartedAt)
	p.Status, p.Text, p.Step = "active", "Работаю над запросом…", -1
	p.Delivery = domain.ProgressDelivery{State: "ready"}
	p.Outcome = ""
	return p
}

func validProgressStatus(status string) bool {
	return status == "waiting_delivery" || status == "failed" || status == "cancelled"
}

// Only replies in this conversation count. A requested cross-chat post does not
// complete the owner's reply and cannot cause a private status to disappear.
const progressOutcomeSQL = `CASE
 WHEN p.data->>'status' IN ('failed','cancelled') THEN p.data->>'status'
 WHEN p.data->>'status'='active' THEN CASE WHEN EXISTS(SELECT 1 FROM openalbert_outgoing o WHERE o.data->>'turn_id'=p.id AND o.data->'conversation'->>'id'=p.data->'conversation'->>'id') THEN 'waiting' ELSE 'active' END
 WHEN EXISTS(SELECT 1 FROM openalbert_outgoing o WHERE o.data->>'turn_id'=p.id AND o.data->'conversation'->>'id'=p.data->'conversation'->>'id' AND o.state IN ('failed','unknown')) THEN 'delivery_failed'
 WHEN EXISTS(SELECT 1 FROM openalbert_outgoing o WHERE o.data->>'turn_id'=p.id AND o.data->'conversation'->>'id'=p.data->'conversation'->>'id')
 AND NOT EXISTS(SELECT 1 FROM openalbert_outgoing o WHERE o.data->>'turn_id'=p.id AND o.data->'conversation'->>'id'=p.data->'conversation'->>'id' AND o.state<>'sent') THEN 'delivered'
 ELSE 'waiting' END`

func (s *Store) StartProgress(ctx context.Context, p domain.Progress) error {
	if err := validID(p.TurnID); err != nil {
		return err
	}
	if err := validID(p.Conversation.ID); err != nil {
		return err
	}
	raw, err := encode(newProgress(p))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO openalbert_progress(id,data) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET data=jsonb_set(openalbert_progress.data,'{conversation}',excluded.data->'conversation')`, p.TurnID, raw)
	return err
}

func (s *Store) ReportProgress(ctx context.Context, id string, step int, text string) error {
	_, err := s.pool.Exec(ctx, `UPDATE openalbert_progress SET data=data||jsonb_build_object('step',$2::int,'text',$3::text) WHERE id=$1 AND data->>'status'='active' AND (data->>'step')::int<=$2`, id, step, text)
	return err
}

func (s *Store) EndProgress(ctx context.Context, id, status string) error {
	if !validProgressStatus(status) {
		return errors.New("invalid progress status")
	}
	_, err := s.pool.Exec(ctx, `UPDATE openalbert_progress SET data=jsonb_set(data,'{status}',to_jsonb($2::text)) WHERE id=$1 AND data->>'status'='active'`, id, status)
	return err
}

func (s *Store) DueProgress(ctx context.Context, now time.Time, n int) ([]domain.Progress, error) {
	return selectJSON[domain.Progress](ctx, s.pool, `WITH candidates AS (
 SELECT p.data||jsonb_build_object('outcome',`+progressOutcomeSQL+`) AS data
 FROM openalbert_progress p WHERE p.data->'delivery'->>'state'='ready'
 OR (p.data->'delivery'->>'state'='suspended' AND COALESCE(p.data->'conversation'->>'delivery_channel','')<>'desktop'))
 SELECT data FROM candidates WHERE (data->'delivery'->>'retry_at')::timestamptz<=$1
 AND (data->'conversation'->>'delivery_channel'='desktop' OR data->'delivery'->>'removing'='true'
 OR (data->>'outcome'<>'waiting' AND (data->>'outcome'<>'active'
 OR ((data->>'started_at')::timestamptz<=$2 AND (data->'delivery'->>'updated_at')::timestamptz<=$3))))
 ORDER BY data->>'started_at',data->>'turn_id' LIMIT $4`, now, now.Add(-domain.ProgressDelay), now.Add(-domain.ProgressInterval), limitValue(n))
}

// Only delivery fields are replaced. Concurrent model updates/Finish are retained.
// The compare-and-swap also claims initial creation before contacting Telegram.
func (s *Store) SetProgressDelivery(ctx context.Context, id string, version int64, next domain.ProgressDelivery) (domain.Progress, bool, error) {
	next.Version = version + 1
	raw, err := json.Marshal(next)
	if err != nil {
		return domain.Progress{}, false, err
	}
	return oneJSON[domain.Progress](ctx, s.pool, `UPDATE openalbert_progress p SET data=jsonb_set(data,'{delivery}',$3::jsonb) WHERE id=$1 AND (data->'delivery'->>'version')::bigint=$2 RETURNING p.data||jsonb_build_object('outcome',`+progressOutcomeSQL+`)`, id, version, raw)
}

func (m *Memory) StartProgress(ctx context.Context, p domain.Progress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(p.TurnID); err != nil {
		return err
	}
	if err := validID(p.Conversation.ID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.progress[p.TurnID]; !ok {
		m.progress[p.TurnID] = newProgress(p)
	} else {
		current := m.progress[p.TurnID]
		current.Conversation = p.Conversation
		m.progress[p.TurnID] = current
	}
	return nil
}

func (m *Memory) ReportProgress(ctx context.Context, id string, step int, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.progress[id]
	if ok && p.Status == "active" && step >= p.Step {
		p.Step, p.Text = step, text
		m.progress[id] = p
	}
	return nil
}

func (m *Memory) EndProgress(ctx context.Context, id, status string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validProgressStatus(status) {
		return errors.New("invalid progress status")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.progress[id]
	if ok && p.Status == "active" {
		p.Status = status
		m.progress[id] = p
	}
	return nil
}

func (m *Memory) progressOutcome(p domain.Progress) domain.Progress {
	p.Outcome = p.Status
	if p.Status == "failed" || p.Status == "cancelled" {
		return p
	}
	count, sent, failed := 0, 0, false
	for _, out := range m.outgoing {
		if out.TurnID != p.TurnID || out.Conversation.ID != p.Conversation.ID {
			continue
		}
		count++
		if out.State == "sent" {
			sent++
		}
		failed = failed || out.State == "failed" || out.State == "unknown"
	}
	if p.Status == "active" {
		if count > 0 {
			p.Outcome = "waiting"
		}
		return p
	}
	p.Outcome = "waiting"
	if failed {
		p.Outcome = "delivery_failed"
	} else if count > 0 && count == sent {
		p.Outcome = "delivered"
	}
	return p
}

func (m *Memory) DueProgress(ctx context.Context, now time.Time, n int) ([]domain.Progress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Progress{}
	for _, p := range m.progress {
		p = m.progressOutcome(p)
		if p.Due(now) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].TurnID < out[j].TurnID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	if len(out) > limitValue(n) {
		out = out[:limitValue(n)]
	}
	return out, ctx.Err()
}

func (m *Memory) SetProgressDelivery(ctx context.Context, id string, version int64, next domain.ProgressDelivery) (domain.Progress, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Progress{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.progress[id]
	if !ok || p.Delivery.Version != version {
		return domain.Progress{}, false, nil
	}
	next.Version = version + 1
	p.Delivery = next
	m.progress[id] = p
	return m.progressOutcome(p), true, nil
}
