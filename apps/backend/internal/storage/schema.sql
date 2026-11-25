CREATE TABLE IF NOT EXISTS openalbert_conversations (
    id text PRIMARY KEY,
    data jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS openalbert_incoming (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    data jsonb NOT NULL,
    handled boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS openalbert_poll (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    offset_value bigint NOT NULL DEFAULT 0
);
INSERT INTO openalbert_poll(id, offset_value) VALUES(true, 0) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS openalbert_sessions (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    conversation_id text NOT NULL,
    data jsonb NOT NULL,
    analyzed boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS openalbert_messages (
    seq bigserial UNIQUE,
    session_id text NOT NULL REFERENCES openalbert_sessions(id),
    mutation_key text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY(session_id, mutation_key)
);
CREATE TABLE IF NOT EXISTS openalbert_tasks (
    seq bigserial UNIQUE,
    conversation_id text NOT NULL,
    id text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY(conversation_id, id)
);
UPDATE openalbert_tasks SET data=jsonb_set(data,'{version}','1'::jsonb) WHERE NOT data ? 'version';
CREATE TABLE IF NOT EXISTS openalbert_outgoing (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    data jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS openalbert_artifacts (
    conversation_id text NOT NULL,
    id text NOT NULL,
    data jsonb NOT NULL,
    content bytea NOT NULL,
    PRIMARY KEY(conversation_id, id)
);
CREATE INDEX IF NOT EXISTS openalbert_incoming_pending ON openalbert_incoming(seq) WHERE NOT handled;
CREATE INDEX IF NOT EXISTS openalbert_sessions_analysis ON openalbert_sessions(seq) WHERE NOT analyzed;
CREATE INDEX IF NOT EXISTS openalbert_outgoing_pending ON openalbert_outgoing(seq) WHERE state = 'pending';
CREATE TABLE IF NOT EXISTS openalbert_checkpoints (
    id text PRIMARY KEY,
    data jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS openalbert_transcriptions (
 id text PRIMARY KEY,
 owner text NOT NULL,
 lease_until timestamptz NOT NULL,
 data jsonb
);
CREATE TABLE IF NOT EXISTS openalbert_progress (
    id text PRIMARY KEY,
    data jsonb NOT NULL
);
DROP INDEX IF EXISTS openalbert_progress_ready;
CREATE INDEX IF NOT EXISTS openalbert_progress_due ON openalbert_progress((data->>'started_at'),(data->>'turn_id')) WHERE data->'delivery'->>'state'='ready' OR (data->'delivery'->>'state'='suspended' AND COALESCE(data->'conversation'->>'delivery_channel','')<>'desktop');
CREATE INDEX IF NOT EXISTS openalbert_outgoing_turn ON openalbert_outgoing((data->>'turn_id'),(data->'conversation'->>'id'));

ALTER TABLE openalbert_messages ADD COLUMN IF NOT EXISTS recorded_at timestamptz;
CREATE INDEX IF NOT EXISTS openalbert_messages_session_seq ON openalbert_messages(session_id,seq);
CREATE TABLE IF NOT EXISTS openalbert_traces (
 id text PRIMARY KEY,
 session_id text NOT NULL,
 turn_id text NOT NULL,
 started_at timestamptz NOT NULL,
 data jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_traces_session ON openalbert_traces(session_id,started_at,id);
CREATE INDEX IF NOT EXISTS openalbert_turns_session ON openalbert_checkpoints((data->'Input'->>'SessionID')) WHERE id LIKE 'turn-done/%';
CREATE INDEX IF NOT EXISTS openalbert_audio_session ON openalbert_checkpoints((data->>'session_id')) WHERE id LIKE 'audio/%';

-- A common sequence for transport-visible history, independent of model turns.
CREATE TABLE IF NOT EXISTS openalbert_public_timeline (
 seq bigserial UNIQUE,
 kind text NOT NULL CHECK(kind IN ('incoming','outgoing')),
 source_id text NOT NULL,
 conversation_id text NOT NULL,
 PRIMARY KEY(kind,source_id)
);
CREATE INDEX IF NOT EXISTS openalbert_public_scope ON openalbert_public_timeline(conversation_id,seq);
INSERT INTO openalbert_public_timeline(kind,source_id,conversation_id)
 SELECT kind,id,scope FROM (
  SELECT 'incoming' AS kind,id,data->'conversation'->>'id' AS scope,COALESCE(NULLIF(data->>'created_at',''),'1970-01-01T00:00:00Z') AS at FROM openalbert_incoming
  UNION ALL
  SELECT 'outgoing',id,data->'conversation'->>'id',COALESCE(NULLIF(data->>'enqueued_at',''),'1970-01-01T00:00:00Z') FROM openalbert_outgoing
 ) legacy ORDER BY at,kind,id ON CONFLICT DO NOTHING;

-- Status polling reads only this conversation's pending inbox/public progress.
CREATE INDEX IF NOT EXISTS openalbert_incoming_scope_pending ON openalbert_incoming((data->'conversation'->>'id')) WHERE NOT handled;
CREATE INDEX IF NOT EXISTS openalbert_progress_scope_active ON openalbert_progress((data->'conversation'->>'id')) WHERE data->>'status'='active';

CREATE TABLE IF NOT EXISTS openalbert_memory_consolidations (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    scope text NOT NULL,
    started_at timestamptz NOT NULL,
    data jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_memory_consolidations_scope ON openalbert_memory_consolidations(scope, seq DESC);

-- Stored scalar columns and partial covering indexes so InspectSessionOrigins,
-- sessionTriggerSQL, and InspectLatestConsolidation use index-only scans without
-- detoasting full trace payloads or turn-done checkpoint message histories.
CREATE OR REPLACE FUNCTION openalbert_rfc3339_timestamptz(raw text) RETURNS timestamptz
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    IF raw IS NULL THEN
        RETURN NULL;
    END IF;
    IF raw ~ '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$' THEN
        RETURN raw::timestamptz;
    END IF;
    IF raw ~ '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?$' THEN
        RETURN raw::timestamp AT TIME ZONE 'UTC';
    END IF;
    RETURN NULL;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$;

ALTER TABLE openalbert_traces
    ADD COLUMN IF NOT EXISTS trigger_kind text GENERATED ALWAYS AS (
        CASE WHEN data->>'trigger' IN ('message','task','initiative','coding') THEN data->>'trigger' END
    ) STORED,
    ADD COLUMN IF NOT EXISTS task_id text GENERATED ALWAYS AS (
        NULLIF(data->>'task_id', '')
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_provider text GENERATED ALWAYS AS (
        NULLIF(data->'search'->>'provider', '')
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_mode text GENERATED ALWAYS AS (
        NULLIF(data->'search'->>'mode', '')
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_resolved_mode text GENERATED ALWAYS AS (
        NULLIF(data->'search'->>'resolved_mode', '')
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_cost_usd double precision GENERATED ALWAYS AS (
        CASE
            WHEN (data->'search'->>'cost_usd') ~ '^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$'
            THEN (data->'search'->>'cost_usd')::double precision
        END
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_http_status integer GENERATED ALWAYS AS (
        CASE
            WHEN (data->'search'->>'http_status') ~ '^[0-9]+$'
            THEN (data->'search'->>'http_status')::integer
        END
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_results integer GENERATED ALWAYS AS (
        CASE
            WHEN (data->'search'->>'result_count') ~ '^[0-9]+$'
            THEN (data->'search'->>'result_count')::integer
        END
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_rate_limited boolean GENERATED ALWAYS AS (
        COALESCE(data->'search'->>'rate_limited', '') = 'true'
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_completed_at timestamptz GENERATED ALWAYS AS (
        openalbert_rfc3339_timestamptz(data->'search'->>'completed_at')
    ) STORED,
    ADD COLUMN IF NOT EXISTS search_outcome_at timestamptz GENERATED ALWAYS AS (
        COALESCE(openalbert_rfc3339_timestamptz(data->'search'->>'completed_at'), started_at)
    ) STORED,
    ADD COLUMN IF NOT EXISTS trace_status text GENERATED ALWAYS AS (
        NULLIF(data->>'status', '')
    ) STORED;
CREATE INDEX IF NOT EXISTS openalbert_traces_session_trigger
    ON openalbert_traces(session_id, started_at)
    INCLUDE (trigger_kind)
    WHERE trigger_kind IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_traces_session_task
    ON openalbert_traces(session_id, started_at)
    INCLUDE (task_id)
    WHERE task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_traces_search_usage
    ON openalbert_traces(search_outcome_at DESC, id)
    INCLUDE (session_id, started_at, trace_status, trigger_kind, task_id, search_provider, search_mode, search_resolved_mode, search_cost_usd, search_http_status, search_results, search_rate_limited)
    WHERE search_provider IS NOT NULL;

ALTER TABLE openalbert_checkpoints
    ADD COLUMN IF NOT EXISTS turn_session_id text GENERATED ALWAYS AS (
        CASE WHEN id LIKE 'turn-done/%' THEN NULLIF(data->'Input'->>'SessionID', '') END
    ) STORED,
    ADD COLUMN IF NOT EXISTS turn_task_id text GENERATED ALWAYS AS (
        CASE WHEN id LIKE 'turn-done/%' THEN NULLIF(data->'Input'->>'TaskID', '') END
    ) STORED,
    ADD COLUMN IF NOT EXISTS turn_initiative boolean GENERATED ALWAYS AS (
        CASE WHEN id LIKE 'turn-done/%' THEN (COALESCE(data->'Input'->>'Initiative', '') = 'true') END
    ) STORED,
    ADD COLUMN IF NOT EXISTS consolidation_scope text GENERATED ALWAYS AS (
        CASE WHEN id LIKE 'memory-consolidation/%' THEN NULLIF(data->>'scope', '') END
    ) STORED,
    ADD COLUMN IF NOT EXISTS consolidation_completed_at timestamptz GENERATED ALWAYS AS (
        CASE WHEN id LIKE 'memory-consolidation/%' THEN openalbert_rfc3339_timestamptz(data->>'completed_at') END
    ) STORED;
CREATE INDEX IF NOT EXISTS openalbert_checkpoints_turn_done_origin
    ON openalbert_checkpoints(turn_session_id)
    INCLUDE (turn_task_id, turn_initiative)
    WHERE turn_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_checkpoints_turn_done_task
    ON openalbert_checkpoints(turn_session_id)
    INCLUDE (turn_task_id)
    WHERE turn_session_id IS NOT NULL AND turn_task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_checkpoints_consolidation_latest
    ON openalbert_checkpoints(consolidation_scope, consolidation_completed_at DESC NULLS LAST, id DESC)
    WHERE consolidation_scope IS NOT NULL;

CREATE TABLE IF NOT EXISTS openalbert_t3_hosts (
    environment_id text PRIMARY KEY,
    host_id text NOT NULL UNIQUE,
    label text NOT NULL,
    projects_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    providers_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    last_seen_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS openalbert_t3_commands (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    environment_id text NOT NULL,
    action text NOT NULL,
    payload_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL CHECK (status IN ('pending','dispatched','succeeded','failed')),
    result_json jsonb,
    error_text text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_t3_commands_env_pending
    ON openalbert_t3_commands(environment_id, seq)
    WHERE status IN ('pending', 'dispatched');
CREATE TABLE IF NOT EXISTS openalbert_t3_threads (
    thread_id text PRIMARY KEY,
    environment_id text NOT NULL,
    conversation_id text NOT NULL DEFAULT '',
    task_id text NOT NULL DEFAULT '',
    project_id text NOT NULL DEFAULT '',
    title text NOT NULL DEFAULT '',
    link text NOT NULL DEFAULT '',
    provider_instance_id text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    branch text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT '',
    pr_url text NOT NULL DEFAULT '',
    pending_request_json jsonb,
    last_output_json jsonb,
    last_summary text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    last_event_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
ALTER TABLE openalbert_t3_threads ADD COLUMN IF NOT EXISTS last_event_at timestamptz;
CREATE INDEX IF NOT EXISTS openalbert_t3_threads_env
    ON openalbert_t3_threads(environment_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS openalbert_t3_threads_conv
    ON openalbert_t3_threads(conversation_id, updated_at DESC)
    WHERE conversation_id <> '';
CREATE INDEX IF NOT EXISTS openalbert_t3_threads_status
    ON openalbert_t3_threads(status, updated_at DESC);
CREATE TABLE IF NOT EXISTS openalbert_t3_events (
    event_id text PRIMARY KEY,
    environment_id text NOT NULL,
    thread_id text NOT NULL,
    kind text NOT NULL,
    payload_json jsonb NOT NULL,
    received_at timestamptz NOT NULL,
    woken_at timestamptz
);
ALTER TABLE openalbert_t3_events ADD COLUMN IF NOT EXISTS woken_at timestamptz;
CREATE INDEX IF NOT EXISTS openalbert_t3_events_thread
    ON openalbert_t3_events(thread_id, received_at DESC);
CREATE INDEX IF NOT EXISTS openalbert_t3_events_pending_wake
    ON openalbert_t3_events(thread_id, received_at ASC)
    WHERE woken_at IS NULL AND kind IN ('thread_waiting_input', 'thread_completed', 'thread_failed');
