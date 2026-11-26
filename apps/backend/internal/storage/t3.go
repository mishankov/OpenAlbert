package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"openalbert/apps/backend/internal/domain"
)

var _ domain.T3Store = (*Store)(nil)

func normalizeJSONObject(raw json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return []byte("{}")
	}
	return []byte(trimmed)
}

func nullableJSON(raw json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	return []byte(trimmed)
}

func scanT3Host(row pgx.Row) (domain.T3Host, error) {
	var h domain.T3Host
	var projectsRaw, providersRaw []byte
	err := row.Scan(
		&h.EnvironmentID, &h.HostID, &h.Label,
		&projectsRaw, &providersRaw,
		&h.LastSeenAt, &h.CreatedAt, &h.UpdatedAt,
	)
	if err != nil {
		return domain.T3Host{}, err
	}
	h.Projects = []domain.T3Project{}
	if len(projectsRaw) > 0 {
		if err := json.Unmarshal(projectsRaw, &h.Projects); err != nil {
			return domain.T3Host{}, err
		}
		if h.Projects == nil {
			h.Projects = []domain.T3Project{}
		}
	}
	h.Providers = []domain.T3Provider{}
	if len(providersRaw) > 0 {
		if err := json.Unmarshal(providersRaw, &h.Providers); err != nil {
			return domain.T3Host{}, err
		}
		if h.Providers == nil {
			h.Providers = []domain.T3Provider{}
		}
	}
	return h, nil
}

func (s *Store) UpsertT3Host(ctx context.Context, host domain.T3Host) (domain.T3Host, error) {
	if err := validID(host.EnvironmentID); err != nil {
		return domain.T3Host{}, fmt.Errorf("environment_id: %w", err)
	}
	if err := validID(host.HostID); err != nil {
		return domain.T3Host{}, fmt.Errorf("host_id: %w", err)
	}
	if strings.TrimSpace(host.Label) == "" {
		return domain.T3Host{}, errors.New("label is required")
	}
	now := nowIfZero(host.UpdatedAt).UTC()
	lastSeen := nowIfZero(host.LastSeenAt).UTC()
	created := nowIfZero(host.CreatedAt).UTC()
	projects := host.Projects
	if projects == nil {
		projects = []domain.T3Project{}
	}
	providers := host.Providers
	if providers == nil {
		providers = []domain.T3Provider{}
	}
	projectsJSON, err := json.Marshal(projects)
	if err != nil {
		return domain.T3Host{}, err
	}
	providersJSON, err := json.Marshal(providers)
	if err != nil {
		return domain.T3Host{}, err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO openalbert_t3_hosts (
			environment_id, host_id, label, projects_json, providers_json, last_seen_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (environment_id) DO UPDATE SET
			label = excluded.label,
			projects_json = CASE WHEN jsonb_array_length(excluded.projects_json) > 0 THEN excluded.projects_json ELSE openalbert_t3_hosts.projects_json END,
			providers_json = CASE WHEN jsonb_array_length(excluded.providers_json) > 0 THEN excluded.providers_json ELSE openalbert_t3_hosts.providers_json END,
			last_seen_at = excluded.last_seen_at,
			updated_at = excluded.updated_at
		RETURNING environment_id, host_id, label, projects_json, providers_json, last_seen_at, created_at, updated_at
	`, host.EnvironmentID, host.HostID, host.Label, projectsJSON, providersJSON, lastSeen, created, now)
	return scanT3Host(row)
}

func (s *Store) TouchT3Host(ctx context.Context, environmentID string, at time.Time) (domain.T3Host, bool, error) {
	if err := validID(environmentID); err != nil {
		return domain.T3Host{}, false, err
	}
	now := nowIfZero(at).UTC()
	row := s.pool.QueryRow(ctx, `
		UPDATE openalbert_t3_hosts
		SET last_seen_at = GREATEST(last_seen_at, $2), updated_at = GREATEST(updated_at, $2)
		WHERE environment_id = $1
		RETURNING environment_id, host_id, label, projects_json, providers_json, last_seen_at, created_at, updated_at
	`, environmentID, now)
	h, err := scanT3Host(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.T3Host{}, false, nil
	}
	if err != nil {
		return domain.T3Host{}, false, err
	}
	return h, true, nil
}

func (s *Store) T3Host(ctx context.Context, hostOrEnvID string) (domain.T3Host, bool, error) {
	if strings.TrimSpace(hostOrEnvID) == "" {
		return domain.T3Host{}, false, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT environment_id, host_id, label, projects_json, providers_json, last_seen_at, created_at, updated_at
		FROM openalbert_t3_hosts
		WHERE environment_id = $1 OR host_id = $1
		LIMIT 1
	`, hostOrEnvID)
	h, err := scanT3Host(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.T3Host{}, false, nil
	}
	if err != nil {
		return domain.T3Host{}, false, err
	}
	return h, true, nil
}

func (s *Store) ListT3Hosts(ctx context.Context) ([]domain.T3Host, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT environment_id, host_id, label, projects_json, providers_json, last_seen_at, created_at, updated_at
		FROM openalbert_t3_hosts
		ORDER BY created_at ASC, environment_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.T3Host{}
	for rows.Next() {
		h, err := scanT3Host(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanT3Command(row pgx.Row) (domain.T3Command, error) {
	var c domain.T3Command
	var payloadRaw, resultRaw []byte
	err := row.Scan(
		&c.Seq, &c.ID, &c.EnvironmentID, &c.Action,
		&payloadRaw, &c.Status, &resultRaw, &c.ErrorText,
		&c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return domain.T3Command{}, err
	}
	c.Payload = json.RawMessage(normalizeJSONObject(payloadRaw))
	if len(resultRaw) > 0 {
		c.Result = json.RawMessage(append([]byte(nil), resultRaw...))
	}
	return c, nil
}

func (s *Store) CreateT3Command(ctx context.Context, cmd domain.T3Command) (domain.T3Command, bool, error) {
	if err := validID(cmd.ID); err != nil {
		return domain.T3Command{}, false, err
	}
	if err := validID(cmd.EnvironmentID); err != nil {
		return domain.T3Command{}, false, err
	}
	if !domain.ValidT3CommandAction(cmd.Action) {
		return domain.T3Command{}, false, errors.New("invalid command action")
	}
	now := nowIfZero(cmd.CreatedAt).UTC()
	updated := nowIfZero(cmd.UpdatedAt).UTC()
	status := cmd.Status
	if status == "" {
		status = domain.T3CommandStatusPending
	}
	payload := normalizeJSONObject(cmd.Payload)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO openalbert_t3_commands (
			id, environment_id, action, payload_json, status, error_text, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO NOTHING
	`, cmd.ID, cmd.EnvironmentID, cmd.Action, payload, status, cmd.ErrorText, now, updated)
	if err != nil {
		return domain.T3Command{}, false, err
	}
	saved, found, err := s.T3Command(ctx, cmd.ID)
	if err != nil {
		return domain.T3Command{}, false, err
	}
	if !found {
		return domain.T3Command{}, false, ErrNotFound
	}
	return saved, tag.RowsAffected() > 0, nil
}

func (s *Store) T3Command(ctx context.Context, id string) (domain.T3Command, bool, error) {
	if strings.TrimSpace(id) == "" {
		return domain.T3Command{}, false, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT seq, id, environment_id, action, payload_json, status, result_json, error_text, created_at, updated_at
		FROM openalbert_t3_commands
		WHERE id = $1
	`, id)
	c, err := scanT3Command(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.T3Command{}, false, nil
	}
	if err != nil {
		return domain.T3Command{}, false, err
	}
	return c, true, nil
}

func (s *Store) ClaimPendingT3Commands(ctx context.Context, environmentID string, limit int, at, redeliveryBefore time.Time) ([]domain.T3Command, error) {
	if err := validID(environmentID); err != nil {
		return nil, err
	}
	now := nowIfZero(at).UTC()
	var redeliverPtr *time.Time
	if !redeliveryBefore.IsZero() {
		t := redeliveryBefore.UTC()
		redeliverPtr = &t
	}
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id
			FROM openalbert_t3_commands
			WHERE environment_id = $1
			  AND (
				status = 'pending'
				OR ($4::timestamptz IS NOT NULL AND status = 'dispatched' AND updated_at <= $4::timestamptz)
			  )
			ORDER BY seq ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE openalbert_t3_commands c
		SET status = 'dispatched', updated_at = $3
		FROM claimed
		WHERE c.id = claimed.id
		RETURNING c.seq, c.id, c.environment_id, c.action, c.payload_json, c.status, c.result_json, c.error_text, c.created_at, c.updated_at
	`, environmentID, limitValue(limit), now, redeliverPtr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.T3Command{}
	for rows.Next() {
		c, err := scanT3Command(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Seq < out[j-1].Seq; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (s *Store) RequeueT3Command(ctx context.Context, id string, at time.Time) error {
	if err := validID(id); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	_, err := s.pool.Exec(ctx, `
		UPDATE openalbert_t3_commands
		SET status = 'pending', updated_at = $2
		WHERE id = $1 AND status = 'dispatched'
	`, id, now)
	return err
}

func (s *Store) RequeueDispatchedT3Commands(ctx context.Context, environmentID string, at time.Time) error {
	if err := validID(environmentID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	_, err := s.pool.Exec(ctx, `
		UPDATE openalbert_t3_commands
		SET status = 'pending', updated_at = $2
		WHERE environment_id = $1 AND status = 'dispatched'
	`, environmentID, now)
	return err
}

func (s *Store) CompleteT3Command(
	ctx context.Context,
	id, environmentID string,
	ok bool,
	result json.RawMessage,
	errText string,
	at time.Time,
	buildEffect func(cmd domain.T3Command) *domain.T3CommandThreadEffect,
) (domain.T3Command, *domain.T3Thread, bool, bool, error) {
	if err := validID(id); err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	if err := validID(environmentID); err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	now := nowIfZero(at).UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	defer tx.Rollback(ctx)

	existing, err := scanT3Command(tx.QueryRow(ctx, `
		SELECT seq, id, environment_id, action, payload_json, status, result_json, error_text, created_at, updated_at
		FROM openalbert_t3_commands
		WHERE id = $1
		FOR UPDATE
	`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.T3Command{}, nil, false, false, nil
	}
	if err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	if existing.EnvironmentID != environmentID {
		return domain.T3Command{}, nil, false, false, domain.ErrT3EnvironmentMismatch
	}
	if existing.Status == domain.T3CommandStatusSucceeded || existing.Status == domain.T3CommandStatusFailed {
		if err := tx.Commit(ctx); err != nil {
			return domain.T3Command{}, nil, false, false, err
		}
		return existing, nil, true, false, nil
	}
	status := domain.T3CommandStatusSucceeded
	if !ok {
		status = domain.T3CommandStatusFailed
	}
	updated, err := scanT3Command(tx.QueryRow(ctx, `
		UPDATE openalbert_t3_commands
		SET status = $3, result_json = $4, error_text = $5, updated_at = $6
		WHERE id = $1 AND environment_id = $2
		RETURNING seq, id, environment_id, action, payload_json, status, result_json, error_text, created_at, updated_at
	`, id, environmentID, status, nullableJSON(result), errText, now))
	if err != nil {
		return domain.T3Command{}, nil, false, false, err
	}

	var updatedThread *domain.T3Thread
	if ok && buildEffect != nil {
		if eff := buildEffect(updated); eff != nil {
			if eff.ClearThreadID != "" {
				if err := clearT3ThreadPendingTx(ctx, tx, eff.ClearThreadID, eff.ClearRequestID, now); err != nil {
					return domain.T3Command{}, nil, false, false, err
				}
				if th, found, err := getT3ThreadTx(ctx, tx, eff.ClearThreadID); err != nil {
					return domain.T3Command{}, nil, false, false, err
				} else if found {
					updatedThread = &th
				}
			}
			if eff.Patch != nil {
				th, err := upsertT3ThreadTx(ctx, tx, *eff.Patch)
				if err != nil {
					return domain.T3Command{}, nil, false, false, err
				}
				updatedThread = &th
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	return updated, updatedThread, true, true, nil
}

func scanT3Thread(row pgx.Row) (domain.T3Thread, error) {
	var th domain.T3Thread
	var pendingRaw, outputRaw []byte
	err := row.Scan(
		&th.ThreadID, &th.EnvironmentID, &th.HostID,
		&th.ConversationID, &th.TaskID, &th.ProjectID,
		&th.Title, &th.Link, &th.ProviderInstanceID,
		&th.Model, &th.Branch, &th.Status, &th.PRURL,
		&pendingRaw, &outputRaw, &th.LastSummary, &th.LastError,
		&th.CreatedAt, &th.UpdatedAt,
	)
	if err != nil {
		return domain.T3Thread{}, err
	}
	if len(pendingRaw) > 0 && strings.TrimSpace(string(pendingRaw)) != "null" {
		var pr domain.T3PendingRequest
		if err := json.Unmarshal(pendingRaw, &pr); err != nil {
			return domain.T3Thread{}, err
		}
		if pr.Questions == nil {
			pr.Questions = []json.RawMessage{}
		}
		th.PendingRequest = &pr
	}
	if len(outputRaw) > 0 && strings.TrimSpace(string(outputRaw)) != "null" {
		th.LastOutput = json.RawMessage(append([]byte(nil), outputRaw...))
	}
	return th, nil
}

type pgxQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getT3ThreadTx(ctx context.Context, q pgxQuerier, threadID string) (domain.T3Thread, bool, error) {
	if strings.TrimSpace(threadID) == "" {
		return domain.T3Thread{}, false, nil
	}
	row := q.QueryRow(ctx, `
		SELECT
			t.thread_id, t.environment_id, COALESCE(h.host_id, ''),
			t.conversation_id, t.task_id, t.project_id,
			t.title, t.link, t.provider_instance_id,
			t.model, t.branch, t.status, t.pr_url,
			t.pending_request_json, t.last_output_json, t.last_summary, t.last_error,
			t.created_at, t.updated_at
		FROM openalbert_t3_threads t
		LEFT JOIN openalbert_t3_hosts h ON h.environment_id = t.environment_id
		WHERE t.thread_id = $1
	`, threadID)
	th, err := scanT3Thread(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.T3Thread{}, false, nil
	}
	if err != nil {
		return domain.T3Thread{}, false, err
	}
	return th, true, nil
}

func clearT3ThreadPendingTx(ctx context.Context, tx pgx.Tx, threadID, requestID string, at time.Time) error {
	if err := validID(threadID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	_, err := tx.Exec(ctx, `
		UPDATE openalbert_t3_threads
		SET pending_request_json = NULL, updated_at = GREATEST(updated_at, $3)
		WHERE thread_id = $1
		  AND ($2 = '' OR pending_request_json->>'request_id' = $2)
	`, threadID, requestID, now)
	return err
}

func upsertT3ThreadTx(ctx context.Context, q pgxQuerier, patch domain.T3ThreadPatch) (domain.T3Thread, error) {
	if err := validID(patch.ThreadID); err != nil {
		return domain.T3Thread{}, fmt.Errorf("thread_id: %w", err)
	}
	if err := validID(patch.EnvironmentID); err != nil {
		return domain.T3Thread{}, fmt.Errorf("environment_id: %w", err)
	}
	now := nowIfZero(patch.At).UTC()
	var pendingBytes []byte
	if patch.SetPendingRequest && patch.PendingRequest != nil {
		pr := *patch.PendingRequest
		if pr.Questions == nil {
			pr.Questions = []json.RawMessage{}
		}
		var err error
		pendingBytes, err = json.Marshal(pr)
		if err != nil {
			return domain.T3Thread{}, err
		}
	}
	var outputBytes []byte
	if patch.SetLastOutput {
		outputBytes = nullableJSON(patch.LastOutput)
	}
	var eventAt *time.Time
	if patch.RequireCurrent {
		t := now
		eventAt = &t
	}
	row := q.QueryRow(ctx, `
		WITH upserted AS (
			INSERT INTO openalbert_t3_threads (
				thread_id, environment_id, conversation_id, task_id, project_id,
				title, link, provider_instance_id, model, branch, status, pr_url,
				pending_request_json, last_output_json, last_summary, last_error,
				last_event_at, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10, $11, $12,
				$13, $14, $15, $16,
				$23, $17, $17
			)
			ON CONFLICT (thread_id) DO UPDATE SET
				environment_id = COALESCE(NULLIF(excluded.environment_id, ''), openalbert_t3_threads.environment_id),
				conversation_id = COALESCE(NULLIF(excluded.conversation_id, ''), openalbert_t3_threads.conversation_id),
				task_id = COALESCE(NULLIF(excluded.task_id, ''), openalbert_t3_threads.task_id),
				project_id = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.project_id, ''), excluded.project_id)
					ELSE COALESCE(NULLIF(excluded.project_id, ''), openalbert_t3_threads.project_id)
				END,
				title = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.title, ''), excluded.title)
					ELSE COALESCE(NULLIF(excluded.title, ''), openalbert_t3_threads.title)
				END,
				link = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.link, ''), excluded.link)
					ELSE COALESCE(NULLIF(excluded.link, ''), openalbert_t3_threads.link)
				END,
				provider_instance_id = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.provider_instance_id, ''), excluded.provider_instance_id)
					ELSE COALESCE(NULLIF(excluded.provider_instance_id, ''), openalbert_t3_threads.provider_instance_id)
				END,
				model = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.model, ''), excluded.model)
					ELSE COALESCE(NULLIF(excluded.model, ''), openalbert_t3_threads.model)
				END,
				branch = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.branch, ''), excluded.branch)
					ELSE COALESCE(NULLIF(excluded.branch, ''), openalbert_t3_threads.branch)
				END,
				status = CASE
					WHEN $21 THEN COALESCE(NULLIF(openalbert_t3_threads.status, ''), excluded.status)
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at THEN openalbert_t3_threads.status
					ELSE COALESCE(NULLIF(excluded.status, ''), openalbert_t3_threads.status)
				END,
				pr_url = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at
						THEN COALESCE(NULLIF(openalbert_t3_threads.pr_url, ''), excluded.pr_url)
					ELSE COALESCE(NULLIF(excluded.pr_url, ''), openalbert_t3_threads.pr_url)
				END,
				pending_request_json = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at THEN openalbert_t3_threads.pending_request_json
					WHEN $18 THEN excluded.pending_request_json
					ELSE openalbert_t3_threads.pending_request_json
				END,
				last_output_json = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at THEN openalbert_t3_threads.last_output_json
					WHEN $19 THEN excluded.last_output_json
					ELSE openalbert_t3_threads.last_output_json
				END,
				last_summary = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at THEN openalbert_t3_threads.last_summary
					ELSE COALESCE(NULLIF(excluded.last_summary, ''), openalbert_t3_threads.last_summary)
				END,
				last_error = CASE
					WHEN $22 AND openalbert_t3_threads.last_event_at IS NOT NULL AND excluded.last_event_at < openalbert_t3_threads.last_event_at THEN openalbert_t3_threads.last_error
					WHEN $20 THEN excluded.last_error
					ELSE COALESCE(NULLIF(excluded.last_error, ''), openalbert_t3_threads.last_error)
				END,
				last_event_at = CASE
					WHEN $22 THEN COALESCE(GREATEST(openalbert_t3_threads.last_event_at, excluded.last_event_at), excluded.last_event_at)
					ELSE openalbert_t3_threads.last_event_at
				END,
				updated_at = GREATEST(openalbert_t3_threads.updated_at, excluded.updated_at)
			RETURNING *
		)
		SELECT
			u.thread_id, u.environment_id, COALESCE(h.host_id, ''),
			u.conversation_id, u.task_id, u.project_id,
			u.title, u.link, u.provider_instance_id,
			u.model, u.branch, u.status, u.pr_url,
			u.pending_request_json, u.last_output_json, u.last_summary, u.last_error,
			u.created_at, u.updated_at
		FROM upserted u
		LEFT JOIN openalbert_t3_hosts h ON h.environment_id = u.environment_id
	`,
		patch.ThreadID, patch.EnvironmentID, patch.ConversationID, patch.TaskID, patch.ProjectID,
		patch.Title, patch.Link, patch.ProviderInstanceID, patch.Model, patch.Branch, patch.Status, patch.PRURL,
		pendingBytes, outputBytes, patch.LastSummary, patch.LastError,
		now, patch.SetPendingRequest, patch.SetLastOutput, patch.SetLastError,
		patch.PreserveExistingStatus, patch.RequireCurrent, eventAt,
	)
	return scanT3Thread(row)
}

func (s *Store) UpsertT3Thread(ctx context.Context, patch domain.T3ThreadPatch) (domain.T3Thread, error) {
	return upsertT3ThreadTx(ctx, s.pool, patch)
}

func (s *Store) ClearT3ThreadPendingRequest(ctx context.Context, threadID, requestID string, at time.Time) error {
	if err := validID(threadID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	_, err := s.pool.Exec(ctx, `
		UPDATE openalbert_t3_threads
		SET pending_request_json = NULL, updated_at = GREATEST(updated_at, $3)
		WHERE thread_id = $1
		  AND ($2 = '' OR pending_request_json->>'request_id' = $2)
	`, threadID, requestID, now)
	return err
}

func (s *Store) T3Thread(ctx context.Context, threadID string) (domain.T3Thread, bool, error) {
	return getT3ThreadTx(ctx, s.pool, threadID)
}

func (s *Store) ListT3Threads(ctx context.Context, filter domain.T3ThreadFilter) ([]domain.T3Thread, error) {
	clauses := []string{"1=1"}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf(cond, len(args)))
	}
	if filter.EnvironmentID != "" {
		add("t.environment_id = $%d", filter.EnvironmentID)
	}
	if filter.HostID != "" {
		args = append(args, filter.HostID)
		clauses = append(clauses, fmt.Sprintf("(h.host_id = $%d OR t.environment_id = $%d)", len(args), len(args)))
	}
	if filter.ConversationID != "" {
		add("t.conversation_id = $%d", filter.ConversationID)
	}
	if filter.ProjectID != "" {
		add("t.project_id = $%d", filter.ProjectID)
	}
	if filter.Status != "" {
		add("t.status = $%d", filter.Status)
	}
	args = append(args, limitValue(filter.Limit))
	query := fmt.Sprintf(`
		SELECT
			t.thread_id, t.environment_id, COALESCE(h.host_id, ''),
			t.conversation_id, t.task_id, t.project_id,
			t.title, t.link, t.provider_instance_id,
			t.model, t.branch, t.status, t.pr_url,
			t.pending_request_json, t.last_output_json, t.last_summary, t.last_error,
			t.created_at, t.updated_at
		FROM openalbert_t3_threads t
		LEFT JOIN openalbert_t3_hosts h ON h.environment_id = t.environment_id
		WHERE %s
		ORDER BY t.updated_at DESC, t.thread_id ASC
		LIMIT $%d
	`, strings.Join(clauses, " AND "), len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.T3Thread{}
	for rows.Next() {
		th, err := scanT3Thread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, th)
	}
	return out, rows.Err()
}

func decodeStoredT3Event(eventID, envID, threadID, kind string, payload []byte) (domain.T3Event, error) {
	var wire struct {
		Status             string                   `json:"status"`
		Title              string                   `json:"title"`
		Link               string                   `json:"link"`
		ProjectID          string                   `json:"project_id"`
		ProviderInstanceID string                   `json:"provider_instance_id"`
		Model              string                   `json:"model"`
		Branch             string                   `json:"branch"`
		PRURL              string                   `json:"pr_url"`
		PendingRequest     *domain.T3PendingRequest `json:"pending_request"`
		Output             json.RawMessage          `json:"output"`
		Summary            string                   `json:"summary"`
		Error              string                   `json:"error"`
		OccurredAt         string                   `json:"occurred_at"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &wire); err != nil {
			return domain.T3Event{}, err
		}
	}
	var occurredAt time.Time
	if s := strings.TrimSpace(wire.OccurredAt); s != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			occurredAt = parsed.UTC()
		}
	}
	// Owner routing always comes from the persisted thread, never the stored payload.
	return domain.T3Event{
		EventID:            eventID,
		EnvironmentID:      envID,
		ThreadID:           threadID,
		Kind:               kind,
		Status:             wire.Status,
		Title:              wire.Title,
		Link:               wire.Link,
		ProjectID:          wire.ProjectID,
		ProviderInstanceID: wire.ProviderInstanceID,
		Model:              wire.Model,
		Branch:             wire.Branch,
		PRURL:              wire.PRURL,
		PendingRequest:     wire.PendingRequest,
		Output:             wire.Output,
		Summary:            wire.Summary,
		Error:              wire.Error,
		OccurredAt:         occurredAt,
	}, nil
}

func (s *Store) RecordT3EventAndUpsertThread(ctx context.Context, event domain.T3Event, rawPayload json.RawMessage, patch domain.T3ThreadPatch, receivedAt time.Time) (domain.T3Thread, bool, domain.T3StoredEvent, error) {
	if err := validID(event.EventID); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, fmt.Errorf("event_id: %w", err)
	}
	if err := validID(event.EnvironmentID); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, fmt.Errorf("environment_id: %w", err)
	}
	if err := validID(event.ThreadID); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, fmt.Errorf("thread_id: %w", err)
	}
	if !domain.ValidT3EventKind(event.Kind) {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, errors.New("invalid event kind")
	}
	now := nowIfZero(receivedAt).UTC()
	payload := normalizeJSONObject(rawPayload)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		INSERT INTO openalbert_t3_events (event_id, environment_id, thread_id, kind, payload_json, received_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_id) DO NOTHING
	`, event.EventID, event.EnvironmentID, event.ThreadID, event.Kind, payload, now)
	if err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
	}
	if tag.RowsAffected() == 0 {
		var evID, envID, threadID, kind string
		var storedRaw []byte
		var wokenAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT event_id, environment_id, thread_id, kind, payload_json, woken_at
			FROM openalbert_t3_events
			WHERE event_id = $1
		`, event.EventID).Scan(&evID, &envID, &threadID, &kind, &storedRaw, &wokenAt); err != nil {
			return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
		}
		storedEv, err := decodeStoredT3Event(evID, envID, threadID, kind, storedRaw)
		if err != nil {
			return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
		}
		th, _, err := getT3ThreadTx(ctx, tx, storedEv.ThreadID)
		if err != nil {
			return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
		}
		return th, true, domain.T3StoredEvent{Event: storedEv, WokenAt: wokenAt}, nil
	}
	th, err := upsertT3ThreadTx(ctx, tx, patch)
	if err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
	}
	return th, false, domain.T3StoredEvent{Event: event}, nil
}

func (s *Store) PendingWakeT3Events(ctx context.Context, threadID string) ([]domain.T3Event, error) {
	if err := validID(threadID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, environment_id, thread_id, kind, payload_json
		FROM openalbert_t3_events
		WHERE thread_id = $1
		  AND woken_at IS NULL
		  AND kind IN ('thread_waiting_input', 'thread_completed', 'thread_failed')
		ORDER BY received_at ASC, event_id ASC
	`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.T3Event{}
	for rows.Next() {
		var evID, envID, tid, kind string
		var raw []byte
		if err := rows.Scan(&evID, &envID, &tid, &kind, &raw); err != nil {
			return nil, err
		}
		ev, err := decodeStoredT3Event(evID, envID, tid, kind, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) MarkT3EventWoken(ctx context.Context, eventID string, at time.Time) error {
	if err := validID(eventID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	_, err := s.pool.Exec(ctx, `
		UPDATE openalbert_t3_events
		SET woken_at = COALESCE(woken_at, $2)
		WHERE event_id = $1
	`, eventID, now)
	return err
}
