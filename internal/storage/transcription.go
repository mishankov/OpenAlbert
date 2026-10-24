package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"openalbert/internal/domain"
	"time"
)

// A short, renewable lease prevents competing activity attempts from starting
// inference. Completion is fenced by owner, including after lease takeover.
func (s *Store) ClaimTranscription(ctx context.Context, key, owner string) (out domain.SpeechText, cached, claimed bool, err error) {
	var raw []byte
	err = s.pool.QueryRow(ctx, `INSERT INTO openalbert_transcriptions(id,owner,lease_until) VALUES($1,$2,clock_timestamp()+interval '2 minutes')
	 ON CONFLICT(id) DO UPDATE SET owner=excluded.owner,lease_until=excluded.lease_until
	 WHERE openalbert_transcriptions.data IS NULL AND (openalbert_transcriptions.owner=$2 OR openalbert_transcriptions.lease_until<clock_timestamp()) RETURNING data`, key, owner).Scan(&raw)
	if err == nil {
		return out, false, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, false, err
	}
	err = s.pool.QueryRow(ctx, `SELECT data FROM openalbert_transcriptions WHERE id=$1`, key).Scan(&raw)
	if err != nil {
		return out, false, false, err
	}
	if raw == nil {
		return out, false, false, nil
	}
	err = json.Unmarshal(raw, &out)
	return out, err == nil, false, err
}
func (s *Store) CompleteTranscription(ctx context.Context, key, owner string, out domain.SpeechText) (bool, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE openalbert_transcriptions SET data=$3,owner='',lease_until=clock_timestamp() WHERE id=$1 AND owner=$2 AND data IS NULL AND lease_until>=clock_timestamp()`, key, owner, raw)
	return tag.RowsAffected() == 1, err
}
func (s *Store) ReleaseTranscription(ctx context.Context, key, owner string) error {
	_, err := s.pool.Exec(ctx, `UPDATE openalbert_transcriptions SET owner='',lease_until=clock_timestamp()-interval '1 second' WHERE id=$1 AND owner=$2 AND data IS NULL`, key, owner)
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
