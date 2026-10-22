CREATE TABLE IF NOT EXISTS jarvis_conversations (
    id text PRIMARY KEY,
    data jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS jarvis_incoming (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    data jsonb NOT NULL,
    handled boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS jarvis_poll (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    offset_value bigint NOT NULL DEFAULT 0
);
INSERT INTO jarvis_poll(id, offset_value) VALUES(true, 0) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS jarvis_sessions (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    conversation_id text NOT NULL,
    data jsonb NOT NULL,
    analyzed boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS jarvis_messages (
    seq bigserial UNIQUE,
    session_id text NOT NULL REFERENCES jarvis_sessions(id),
    mutation_key text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY(session_id, mutation_key)
);
CREATE TABLE IF NOT EXISTS jarvis_tasks (
    seq bigserial UNIQUE,
    conversation_id text NOT NULL,
    id text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY(conversation_id, id)
);
UPDATE jarvis_tasks SET data=jsonb_set(data,'{version}','1'::jsonb) WHERE NOT data ? 'version';
CREATE TABLE IF NOT EXISTS jarvis_outgoing (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    data jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS jarvis_artifacts (
    conversation_id text NOT NULL,
    id text NOT NULL,
    data jsonb NOT NULL,
    content bytea NOT NULL,
    PRIMARY KEY(conversation_id, id)
);
CREATE INDEX IF NOT EXISTS jarvis_incoming_pending ON jarvis_incoming(seq) WHERE NOT handled;
CREATE INDEX IF NOT EXISTS jarvis_sessions_analysis ON jarvis_sessions(seq) WHERE NOT analyzed;
CREATE INDEX IF NOT EXISTS jarvis_outgoing_pending ON jarvis_outgoing(seq) WHERE state = 'pending';
CREATE TABLE IF NOT EXISTS jarvis_checkpoints (
    id text PRIMARY KEY,
    data jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS jarvis_progress (
    id text PRIMARY KEY,
    data jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS jarvis_progress_ready ON jarvis_progress(id) WHERE data->'delivery'->>'state'='ready';
CREATE INDEX IF NOT EXISTS jarvis_outgoing_turn ON jarvis_outgoing((data->>'turn_id'),(data->'conversation'->>'id'));

ALTER TABLE jarvis_messages ADD COLUMN IF NOT EXISTS recorded_at timestamptz;
CREATE INDEX IF NOT EXISTS jarvis_messages_session_seq ON jarvis_messages(session_id,seq);
CREATE TABLE IF NOT EXISTS jarvis_traces (
 id text PRIMARY KEY,
 session_id text NOT NULL,
 turn_id text NOT NULL,
 started_at timestamptz NOT NULL,
 data jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS jarvis_traces_session ON jarvis_traces(session_id,started_at,id);
CREATE INDEX IF NOT EXISTS jarvis_turns_session ON jarvis_checkpoints((data->'Input'->>'SessionID')) WHERE id LIKE 'turn-done/%';
