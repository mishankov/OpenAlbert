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
