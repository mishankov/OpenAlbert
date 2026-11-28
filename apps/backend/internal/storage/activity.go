package storage

import (
	"context"
	"openalbert/apps/backend/internal/domain"
)

const queuedActivityText = "Обрабатываю сообщение…"

// ConversationActivity reads the current scope in one snapshot. Delivery delays,
// Telegram progress suppression, and failed status-message delivery do not hide
// actual work. Scheduled tasks marked running between checks are not work signals.
func (s *Store) ConversationActivity(ctx context.Context, scope string) (domain.ConversationActivity, error) {
	var out domain.ConversationActivity
	err := s.queryRow(ctx, `WITH active AS (
 SELECT json_extract(data, '$.text') AS text FROM openalbert_progress
 WHERE conversation_id = ?1 AND status = 'active'
 ORDER BY started_at DESC, id DESC LIMIT 1
)
SELECT CASE WHEN EXISTS(SELECT 1 FROM active) THEN 'processing'
WHEN EXISTS(SELECT 1 FROM openalbert_incoming WHERE handled = 0 AND conversation_id = ?1) THEN 'queued'
ELSE 'idle' END, COALESCE((SELECT text FROM active), '')`, scope).Scan(&out.State, &out.Text)
	if out.State == "queued" {
		out.Text = queuedActivityText
	}
	return out, err
}

func (m *Memory) ConversationActivity(ctx context.Context, scope string) (domain.ConversationActivity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := domain.ConversationActivity{State: "idle"}
	var latest *domain.Progress
	for _, p := range m.progress {
		if p.Conversation.ID != scope || p.Status != "active" {
			continue
		}
		if latest == nil || p.StartedAt.After(latest.StartedAt) || p.StartedAt.Equal(latest.StartedAt) && p.TurnID > latest.TurnID {
			v := p
			latest = &v
		}
	}
	if latest != nil {
		out.State = "processing"
		out.Text = latest.Text
		return out, ctx.Err()
	}
	for id, in := range m.incoming {
		if in.Conversation.ID == scope && !m.handled[id] {
			out.State = "queued"
			out.Text = queuedActivityText
			break
		}
	}
	return out, ctx.Err()
}
