package storage

import (
	"context"
	"database/sql"
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
	if p.Conversation.ChatID < 0 {
		p.Delivery.State = "disabled" // Keep internal progress without posting group statuses.
	}
	p.Outcome = ""
	return p
}

func validProgressStatus(status string) bool {
	return status == "waiting_delivery" || status == "failed" || status == "cancelled"
}

func (s *Store) StartProgress(ctx context.Context, p domain.Progress) error {
	if err := validID(p.TurnID); err != nil {
		return err
	}
	if err := validID(p.Conversation.ID); err != nil {
		return err
	}
	np := newProgress(p)
	raw, err := encode(np)
	if err != nil {
		return err
	}
	convRaw, err := encode(np.Conversation)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO openalbert_progress(
 id, conversation_id, delivery_channel, started_at, status, direct_reply,
 delivery_state, delivery_version, delivery_removing, delivery_updated_at, delivery_retry_at, data
) VALUES(?1, ?2, ?3, ?4, ?5, 0, ?6, ?7, ?8, ?9, ?10, ?11)
ON CONFLICT(id) DO UPDATE SET
 conversation_id = excluded.conversation_id,
 delivery_channel = excluded.delivery_channel,
 data = json_set(openalbert_progress.data, '$.conversation', json(?12))`,
		np.TurnID, np.Conversation.ID, np.Conversation.DeliveryChannel, formatTime(np.StartedAt), np.Status,
		np.Delivery.State, np.Delivery.Version, boolInt(np.Delivery.Removing),
		formatTime(np.Delivery.UpdatedAt), formatTime(np.Delivery.RetryAt), string(raw), string(convRaw),
	)
	return err
}

func (s *Store) ReportProgress(ctx context.Context, id string, step int, text string) error {
	_, err := s.exec(ctx, `UPDATE openalbert_progress
 SET data = json_set(data, '$.step', ?2, '$.text', ?3)
 WHERE id = ?1 AND status = 'active' AND CAST(json_extract(data, '$.step') AS INTEGER) <= ?2`, id, step, text)
	return err
}

func (s *Store) EndProgress(ctx context.Context, id, status string) error {
	if !validProgressStatus(status) {
		return errors.New("invalid progress status")
	}
	_, err := s.exec(ctx, `UPDATE openalbert_progress
 SET status = ?2, data = json_set(data, '$.status', ?2)
 WHERE id = ?1 AND status = 'active'`, id, status)
	return err
}

// ConfirmProgressReply records an acknowledged direct send. Ordinary queued
// parts must still finish before progress is removed.
func (s *Store) ConfirmProgressReply(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE openalbert_progress
 SET direct_reply = 1, data = json_set(data, '$.direct_reply', json('true'))
 WHERE id = ?1`, id)
	return err
}

func (m *Memory) ConfirmProgressReply(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.progress[id]; ok {
		p.DirectReply = true
		m.progress[id] = p
	}
	return nil
}

// Only replies in this conversation count. A requested cross-chat post does not
// complete the owner's reply and cannot cause a private status to disappear.
// Filter candidate rows with the openalbert_progress_due partial index and
// aggregate outgoing state once per candidate in a grouped CTE.
const dueProgressSQL = `WITH candidates AS (
 SELECT p.id, p.conversation_id, p.delivery_channel, p.started_at, p.status,
  p.direct_reply, p.delivery_removing, p.delivery_updated_at, p.data
 FROM openalbert_progress p
 WHERE (p.delivery_state = 'ready'
  OR (p.delivery_state = 'suspended' AND p.delivery_channel <> 'desktop'))
 AND p.delivery_retry_at <= ?1
),
agg AS (
 SELECT o.turn_id, o.conversation_id,
  COUNT(*) AS outgoing_count,
  MAX(CASE WHEN o.state IN ('failed', 'unknown') THEN 1 ELSE 0 END) AS has_failed,
  MAX(CASE WHEN o.state <> 'sent' THEN 1 ELSE 0 END) AS has_unsent
 FROM openalbert_outgoing o
 JOIN candidates c
  ON c.status NOT IN ('failed', 'cancelled')
  AND o.turn_id = c.id
  AND o.conversation_id = c.conversation_id
 GROUP BY o.turn_id, o.conversation_id
),
matched AS (
 SELECT c.id, c.started_at, c.delivery_channel, c.delivery_removing, c.delivery_updated_at, c.data,
  CASE
   WHEN c.status IN ('failed', 'cancelled') THEN c.status
   WHEN c.status = 'active' THEN CASE WHEN c.direct_reply = 1 OR COALESCE(agg.outgoing_count, 0) > 0 THEN 'waiting' ELSE 'active' END
   WHEN COALESCE(agg.has_failed, 0) = 1 THEN 'delivery_failed'
   WHEN (c.direct_reply = 1 OR COALESCE(agg.outgoing_count, 0) > 0) AND COALESCE(agg.has_unsent, 0) = 0 THEN 'delivered'
   ELSE 'waiting'
  END AS outcome
 FROM candidates c
 LEFT JOIN agg ON agg.turn_id = c.id AND agg.conversation_id = c.conversation_id
)
SELECT json_set(data, '$.outcome', outcome)
FROM matched
WHERE delivery_channel = 'desktop'
 OR delivery_removing = 1
 OR (outcome <> 'waiting' AND (outcome <> 'active'
  OR (started_at <= ?2 AND delivery_updated_at <= ?3)))
ORDER BY started_at ASC, id ASC
LIMIT ?4`

func (s *Store) DueProgress(ctx context.Context, now time.Time, n int) ([]domain.Progress, error) {
	return selectJSON[domain.Progress](ctx, s, dueProgressSQL,
		formatTime(now),
		formatTime(now.Add(-domain.ProgressDelay)),
		formatTime(now.Add(-domain.ProgressInterval)),
		limitValue(n),
	)
}

// Only delivery fields are replaced. Concurrent model updates/Finish are retained.
// The compare-and-swap also claims initial creation before contacting Telegram.
func (s *Store) SetProgressDelivery(ctx context.Context, id string, version int64, next domain.ProgressDelivery) (domain.Progress, bool, error) {
	next.Version = version + 1
	raw, err := json.Marshal(next)
	if err != nil {
		return domain.Progress{}, false, err
	}
	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return domain.Progress{}, false, err
	}
	defer tx.Rollback()

	var dataStr, convID, status string
	var directReply int
	err = tx.queryRow(ctx, `UPDATE openalbert_progress SET
 delivery_state = ?3,
 delivery_version = ?4,
 delivery_removing = ?5,
 delivery_updated_at = ?6,
 delivery_retry_at = ?7,
 data = json_set(data, '$.delivery', json(?8))
 WHERE id = ?1 AND delivery_version = ?2
 RETURNING data, conversation_id, status, direct_reply`,
		id, version, next.State, next.Version, boolInt(next.Removing),
		formatTime(next.UpdatedAt), formatTime(next.RetryAt), string(raw),
	).Scan(&dataStr, &convID, &status, &directReply)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Progress{}, false, nil
	}
	if err != nil {
		return domain.Progress{}, false, err
	}
	p, err := decode[domain.Progress]([]byte(dataStr))
	if err != nil {
		return domain.Progress{}, false, err
	}
	outcome := status
	if status != "failed" && status != "cancelled" {
		var count, hasFailed, hasUnsent int
		if err := tx.queryRow(ctx, `SELECT COUNT(*),
 COALESCE(MAX(CASE WHEN state IN ('failed', 'unknown') THEN 1 ELSE 0 END), 0),
 COALESCE(MAX(CASE WHEN state <> 'sent' THEN 1 ELSE 0 END), 0)
 FROM openalbert_outgoing
 WHERE turn_id = ?1 AND conversation_id = ?2`, id, convID).Scan(&count, &hasFailed, &hasUnsent); err != nil {
			return domain.Progress{}, false, err
		}
		switch {
		case status == "active":
			if directReply == 1 || count > 0 {
				outcome = "waiting"
			} else {
				outcome = "active"
			}
		case hasFailed == 1:
			outcome = "delivery_failed"
		case (directReply == 1 || count > 0) && hasUnsent == 0:
			outcome = "delivered"
		default:
			outcome = "waiting"
		}
	}
	p.Outcome = outcome
	if err := tx.Commit(ctx); err != nil {
		return domain.Progress{}, false, err
	}
	return p, true, nil
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
		if count > 0 || p.DirectReply {
			p.Outcome = "waiting"
		}
		return p
	}
	p.Outcome = "waiting"
	if failed {
		p.Outcome = "delivery_failed"
	} else if (count > 0 || p.DirectReply) && count == sent {
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
