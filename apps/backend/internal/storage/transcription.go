package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"openalbert/apps/backend/internal/domain"
	"time"
)

// A short, renewable lease prevents competing activity attempts from starting
// inference. Completion is fenced by owner, including after lease takeover.
func (s *Store) ClaimTranscription(ctx context.Context, key, owner string) (out domain.SpeechText, cached, claimed bool, err error) {
	now := time.Now().UTC()
	nowStr := formatTime(now)
	leaseUntil := formatTime(now.Add(2 * time.Minute))

	tx, err := s.beginImmediate(ctx)
	if err != nil {
		return out, false, false, err
	}
	defer tx.Rollback()

	var raw sql.NullString
	err = tx.queryRow(ctx, `INSERT INTO openalbert_transcriptions(id, owner, lease_until)
 VALUES(?1, ?2, ?3)
 ON CONFLICT(id) DO UPDATE SET owner = excluded.owner, lease_until = excluded.lease_until
 WHERE openalbert_transcriptions.data IS NULL
   AND (openalbert_transcriptions.owner = ?2 OR openalbert_transcriptions.lease_until < ?4)
 RETURNING data`, key, owner, leaseUntil, nowStr).Scan(&raw)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return out, false, false, err
		}
		return out, false, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, false, false, err
	}
	err = tx.queryRow(ctx, `SELECT data FROM openalbert_transcriptions WHERE id = ?1`, key).Scan(&raw)
	if err != nil {
		return out, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, false, false, err
	}
	if !raw.Valid || raw.String == "" {
		return out, false, false, nil
	}
	err = json.Unmarshal([]byte(raw.String), &out)
	return out, err == nil, false, err
}

func (s *Store) CompleteTranscription(ctx context.Context, key, owner string, out domain.SpeechText) (bool, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return false, err
	}
	nowStr := formatTime(time.Now().UTC())
	res, err := s.exec(ctx, `UPDATE openalbert_transcriptions
 SET data = ?3, owner = '', lease_until = ?4
 WHERE id = ?1 AND owner = ?2 AND data IS NULL AND lease_until >= ?4`, key, owner, string(raw), nowStr)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) ReleaseTranscription(ctx context.Context, key, owner string) error {
	expired := formatTime(time.Now().UTC().Add(-time.Second))
	_, err := s.exec(ctx, `UPDATE openalbert_transcriptions
 SET owner = '', lease_until = ?3
 WHERE id = ?1 AND owner = ?2 AND data IS NULL`, key, owner, expired)
	return err
}

type transcriptionLease struct {
	Owner  string
	Until  time.Time
	Result *domain.SpeechText
}

func (m *Memory) ClaimTranscription(ctx context.Context, key, owner string) (out domain.SpeechText, cached, claimed bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.transcriptions[key]
	if l.Result != nil {
		return *l.Result, true, false, nil
	}
	if l.Owner != owner && time.Now().Before(l.Until) {
		return
	}
	m.transcriptions[key] = transcriptionLease{Owner: owner, Until: time.Now().Add(2 * time.Minute)}
	return out, false, true, nil
}

func (m *Memory) CompleteTranscription(ctx context.Context, key, owner string, out domain.SpeechText) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.transcriptions[key]
	if l.Owner != owner || time.Now().After(l.Until) || l.Result != nil {
		return false, nil
	}
	l.Result = &out
	l.Owner = ""
	m.transcriptions[key] = l
	return true, nil
}

func (m *Memory) ReleaseTranscription(ctx context.Context, key, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.transcriptions[key]
	if l.Owner == owner && l.Result == nil {
		delete(m.transcriptions, key)
	}
	return nil
}
