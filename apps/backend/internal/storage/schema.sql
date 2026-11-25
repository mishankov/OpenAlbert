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
CREATE INDEX IF NOT EXISTS openalbert_progress_ready ON openalbert_progress(id) WHERE data->'delivery'->>'state'='ready';
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
    ) STORED;
CREATE INDEX IF NOT EXISTS openalbert_traces_session_trigger
    ON openalbert_traces(session_id, started_at)
    INCLUDE (trigger_kind)
    WHERE trigger_kind IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_traces_session_task
    ON openalbert_traces(session_id, started_at)
    INCLUDE (task_id)
    WHERE task_id IS NOT NULL;

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
