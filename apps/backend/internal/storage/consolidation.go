package storage

import (
	"context"
	"strings"

	"openalbert/apps/backend/internal/domain"
)

// SaveMemoryConsolidation atomically inserts a consolidation report by run ID.
// If a report for the same ID and scope already exists (for example, on an
// interleaved or retried finalization), the original stored report is preserved
// and returned without overwriting it.
func (s *Store) SaveMemoryConsolidation(ctx context.Context, report domain.MemoryConsolidationReport) (domain.MemoryConsolidationReport, error) {
	if err := validID(report.ID); err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	if err := validID(report.Scope); err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	report.StartedAt = nowIfZero(report.StartedAt).UTC()
	report.CompletedAt = nowIfZero(report.CompletedAt).UTC()
	if report.Operations == nil {
		report.Operations = []domain.MemoryConsolidationOperation{}
	}
	raw, err := encode(report)
	if err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	raw = sanitizeJSONB(raw)
	tag, err := s.pool.Exec(ctx, `INSERT INTO openalbert_memory_consolidations(id,scope,started_at,data) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, report.ID, report.Scope, report.StartedAt, raw)
	if err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	if tag.RowsAffected() == 1 {
		return report, nil
	}
	var existingScope string
	var existingRaw []byte
	if err := s.pool.QueryRow(ctx, `SELECT scope,data FROM openalbert_memory_consolidations WHERE id=$1`, report.ID).Scan(&existingScope, &existingRaw); err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	if existingScope != report.Scope {
		return domain.MemoryConsolidationReport{}, ErrConflict
	}
	return decode[domain.MemoryConsolidationReport](existingRaw)
}

func (s *Store) ListMemoryConsolidations(ctx context.Context, scope string, limit int) ([]domain.MemoryConsolidationReport, error) {
	if err := validID(scope); err != nil {
		return nil, err
	}
	return selectJSON[domain.MemoryConsolidationReport](ctx, s.pool, `SELECT data FROM openalbert_memory_consolidations WHERE scope=$1 ORDER BY seq DESC LIMIT $2`, scope, limitValue(limit))
}

func (m *Memory) SaveMemoryConsolidation(ctx context.Context, report domain.MemoryConsolidationReport) (domain.MemoryConsolidationReport, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	if err := validID(report.ID); err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	if err := validID(report.Scope); err != nil {
		return domain.MemoryConsolidationReport{}, err
	}
	report.StartedAt = nowIfZero(report.StartedAt).UTC()
	report.CompletedAt = nowIfZero(report.CompletedAt).UTC()
	if report.Operations == nil {
		report.Operations = []domain.MemoryConsolidationOperation{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.consolidations[report.ID]; ok {
		if existing.Scope != report.Scope {
			return domain.MemoryConsolidationReport{}, ErrConflict
		}
		return clone(existing), nil
	}
	m.consolidations[report.ID] = clone(report)
	m.consolidationOrder = append(m.consolidationOrder, report.ID)
	return clone(report), nil
}

func (m *Memory) ListMemoryConsolidations(ctx context.Context, scope string, limit int) ([]domain.MemoryConsolidationReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validID(scope); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	capLimit := limitValue(limit)
	out := []domain.MemoryConsolidationReport{}
	for i := len(m.consolidationOrder) - 1; i >= 0 && len(out) < capLimit; i-- {
		report := m.consolidations[m.consolidationOrder[i]]
		if report.Scope == scope {
			out = append(out, clone(report))
		}
	}
	return out, nil
}

func escapeLikePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix) + "%"
}

func (s *Store) DeleteCheckpointsByPrefix(ctx context.Context, prefix string) error {
	if err := validID(prefix); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM openalbert_checkpoints WHERE id LIKE $1 ESCAPE '\'`, escapeLikePrefix(prefix))
	return err
}

func (m *Memory) DeleteCheckpointsByPrefix(ctx context.Context, prefix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(prefix); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.checkpoints {
		if strings.HasPrefix(k, prefix) {
			delete(m.checkpoints, k)
		}
	}
	return nil
}
