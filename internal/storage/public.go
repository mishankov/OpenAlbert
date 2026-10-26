package storage

import (
	"context"
	"openalbert/internal/domain"
	"sort"
)

type publicKey struct {
	ID       string
	Incoming bool
}

func publicOutgoing(o domain.Outgoing) bool {
	return o.Conversation.DeliveryChannel == "desktop" || o.State == "sent" || o.State == "unknown"
}

// PublicTimeline returns newest first. Its cursor spans both transport tables.
func (m *Memory) PublicTimeline(ctx context.Context, scope string, before int64, n int) ([]domain.PublicRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.PublicRecord{}
	for i := len(m.publicOrder) - 1; i >= 0 && len(out) < n; i-- {
		seq := int64(i + 1)
		if before > 0 && seq >= before {
			continue
		}
		k := m.publicOrder[i]
		if k.Incoming {
			v := m.incoming[k.ID]
			if v.Conversation.ID != scope || v.Command == "stop" {
				continue
			}
			v = cloneIncoming(v)
			out = append(out, domain.PublicRecord{Seq: seq, Incoming: &v})
		} else {
			v := m.outgoing[k.ID]
			if v.Conversation.ID != scope || !publicOutgoing(v) {
				continue
			}
			v = clone(v)
			out = append(out, domain.PublicRecord{Seq: seq, Outgoing: &v})
		}
	}
	return out, ctx.Err()
}

func (s *Store) PublicTimeline(ctx context.Context, scope string, before int64, n int) ([]domain.PublicRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.seq,p.kind,COALESCE(i.data,o.data),COALESCE(o.state,''),COALESCE(o.attempts,0)
 FROM openalbert_public_timeline p
 LEFT JOIN openalbert_incoming i ON p.kind='incoming' AND i.id=p.source_id
 LEFT JOIN openalbert_outgoing o ON p.kind='outgoing' AND o.id=p.source_id
 WHERE p.conversation_id=$1 AND ($2::bigint=0 OR p.seq<$2)
 AND ((p.kind='incoming' AND COALESCE(i.data->>'command','')<>'stop')
 OR (p.kind='outgoing' AND (o.data->'conversation'->>'delivery_channel'='desktop' OR o.state IN ('sent','unknown'))))
 ORDER BY p.seq DESC LIMIT $3`, scope, before, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PublicRecord{}
	for rows.Next() {
		var p domain.PublicRecord
		var kind, state string
		var b []byte
		var attempts int
		if err = rows.Scan(&p.Seq, &kind, &b, &state, &attempts); err != nil {
			return nil, err
		}
		if kind == "incoming" {
			v, e := decode[domain.Incoming](b)
			if e != nil {
				return nil, e
			}
			p.Incoming = &v
		} else {
			v, e := decode[domain.Outgoing](b)
			if e != nil {
				return nil, e
			}
			v.State = state
			v.Attempts = attempts
			p.Outgoing = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListArtifacts returns metadata only; contents still pass through the blob store.
func (s *Store) ListArtifacts(ctx context.Context, scope string) ([]domain.Artifact, error) {
	return selectJSON[domain.Artifact](ctx, s.pool, "SELECT data FROM openalbert_artifacts WHERE conversation_id=$1 ORDER BY data->>'created_at' DESC,id", scope)
}
func (m *Memory) ListArtifacts(ctx context.Context, scope string) ([]domain.Artifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Artifact{}
	for _, v := range m.artifacts {
		if v.ConversationID == scope {
			if v.SizeBytes == 0 {
				v.SizeBytes = int64(len(v.Data))
			}
			v.Data = nil
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, ctx.Err()
}
