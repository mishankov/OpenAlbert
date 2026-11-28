CREATE TABLE IF NOT EXISTS openalbert_conversations (
    id TEXT PRIMARY KEY,
    data TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS openalbert_incoming (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    conversation_id TEXT NOT NULL,
    data TEXT NOT NULL,
    handled INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS openalbert_incoming_pending
    ON openalbert_incoming(seq) WHERE handled = 0;
CREATE INDEX IF NOT EXISTS openalbert_incoming_scope_pending
    ON openalbert_incoming(conversation_id) WHERE handled = 0;

CREATE TABLE IF NOT EXISTS openalbert_poll (
    id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    offset_value INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO openalbert_poll(id, offset_value) VALUES(1, 0);

CREATE TABLE IF NOT EXISTS openalbert_sessions (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    conversation_id TEXT NOT NULL,
    data TEXT NOT NULL,
    analyzed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS openalbert_sessions_analysis
    ON openalbert_sessions(seq) WHERE analyzed = 0;
CREATE INDEX IF NOT EXISTS openalbert_sessions_conv
    ON openalbert_sessions(conversation_id, seq);

CREATE TABLE IF NOT EXISTS openalbert_messages (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES openalbert_sessions(id),
    mutation_key TEXT NOT NULL,
    data TEXT NOT NULL,
    recorded_at TEXT,
    UNIQUE(session_id, mutation_key)
);
CREATE INDEX IF NOT EXISTS openalbert_messages_session_seq
    ON openalbert_messages(session_id, seq);

CREATE TABLE IF NOT EXISTS openalbert_tasks (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id TEXT NOT NULL,
    id TEXT NOT NULL,
    data TEXT NOT NULL,
    UNIQUE(conversation_id, id)
);

CREATE TABLE IF NOT EXISTS openalbert_outgoing (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    turn_id TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    data TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS openalbert_outgoing_pending
    ON openalbert_outgoing(seq) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS openalbert_outgoing_turn
    ON openalbert_outgoing(turn_id, conversation_id);
CREATE INDEX IF NOT EXISTS openalbert_outgoing_session
    ON openalbert_outgoing(session_id, seq);

CREATE TABLE IF NOT EXISTS openalbert_artifacts (
    conversation_id TEXT NOT NULL,
    id TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    data TEXT NOT NULL,
    content BLOB NOT NULL,
    PRIMARY KEY(conversation_id, id)
);

CREATE TABLE IF NOT EXISTS openalbert_checkpoints (
    id TEXT PRIMARY KEY,
    data TEXT NOT NULL,
    turn_session_id TEXT,
    turn_task_id TEXT,
    turn_initiative INTEGER,
    turn_started_at TEXT,
    consolidation_scope TEXT,
    consolidation_completed_at TEXT,
    audio_session_id TEXT,
    audio_queued_at TEXT
);
CREATE INDEX IF NOT EXISTS openalbert_checkpoints_turn_done_origin
    ON openalbert_checkpoints(turn_session_id, turn_task_id, turn_initiative)
    WHERE turn_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_checkpoints_turn_done_task
    ON openalbert_checkpoints(turn_session_id, turn_task_id)
    WHERE turn_session_id IS NOT NULL AND turn_task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_checkpoints_consolidation_latest
    ON openalbert_checkpoints(consolidation_scope, consolidation_completed_at DESC, id DESC)
    WHERE consolidation_scope IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_audio_session
    ON openalbert_checkpoints(audio_session_id, audio_queued_at, id)
    WHERE audio_session_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS openalbert_transcriptions (
    id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    lease_until TEXT NOT NULL,
    data TEXT
);

CREATE TABLE IF NOT EXISTS openalbert_progress (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    delivery_channel TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    status TEXT NOT NULL,
    direct_reply INTEGER NOT NULL DEFAULT 0,
    delivery_state TEXT NOT NULL,
    delivery_version INTEGER NOT NULL DEFAULT 0,
    delivery_removing INTEGER NOT NULL DEFAULT 0,
    delivery_updated_at TEXT NOT NULL,
    delivery_retry_at TEXT NOT NULL,
    data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_progress_due
    ON openalbert_progress(started_at, id)
    WHERE delivery_state = 'ready' OR (delivery_state = 'suspended' AND delivery_channel <> 'desktop');
CREATE INDEX IF NOT EXISTS openalbert_progress_scope_active
    ON openalbert_progress(conversation_id, started_at DESC, id DESC)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS openalbert_traces (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    data TEXT NOT NULL,
    trigger_kind TEXT,
    task_id TEXT,
    search_provider TEXT,
    search_mode TEXT,
    search_resolved_mode TEXT,
    search_cost_usd REAL,
    search_http_status INTEGER,
    search_results INTEGER,
    search_rate_limited INTEGER NOT NULL DEFAULT 0,
    search_completed_at TEXT,
    search_outcome_at TEXT NOT NULL,
    trace_status TEXT
);
CREATE INDEX IF NOT EXISTS openalbert_traces_session
    ON openalbert_traces(session_id, started_at, id);
CREATE INDEX IF NOT EXISTS openalbert_traces_session_trigger
    ON openalbert_traces(session_id, started_at, trigger_kind)
    WHERE trigger_kind IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_traces_session_task
    ON openalbert_traces(session_id, started_at, task_id)
    WHERE task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS openalbert_traces_search_usage
    ON openalbert_traces(search_outcome_at DESC, id DESC, session_id, started_at, trace_status, trigger_kind, task_id, search_provider, search_mode, search_resolved_mode, search_cost_usd, search_http_status, search_results, search_rate_limited)
    WHERE search_provider IS NOT NULL;

-- A common sequence for transport-visible history, independent of model turns.
CREATE TABLE IF NOT EXISTS openalbert_public_timeline (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL CHECK(kind IN ('incoming', 'outgoing')),
    source_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    UNIQUE(kind, source_id)
);
CREATE INDEX IF NOT EXISTS openalbert_public_scope
    ON openalbert_public_timeline(conversation_id, seq);

CREATE TABLE IF NOT EXISTS openalbert_memory_consolidations (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    scope TEXT NOT NULL,
    started_at TEXT NOT NULL,
    data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_memory_consolidations_scope
    ON openalbert_memory_consolidations(scope, seq DESC);

CREATE TABLE IF NOT EXISTS openalbert_t3_hosts (
    environment_id TEXT PRIMARY KEY,
    host_id TEXT NOT NULL UNIQUE,
    label TEXT NOT NULL,
    projects_json TEXT NOT NULL DEFAULT '[]',
    providers_json TEXT NOT NULL DEFAULT '[]',
    last_seen_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS openalbert_t3_commands (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    environment_id TEXT NOT NULL,
    action TEXT NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL CHECK (status IN ('pending', 'dispatched', 'succeeded', 'failed')),
    result_json TEXT,
    error_text TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_t3_commands_env_pending
    ON openalbert_t3_commands(environment_id, seq)
    WHERE status IN ('pending', 'dispatched');

CREATE TABLE IF NOT EXISTS openalbert_t3_threads (
    thread_id TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL DEFAULT '',
    task_id TEXT NOT NULL DEFAULT '',
    project_id TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    link TEXT NOT NULL DEFAULT '',
    provider_instance_id TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    pending_request_json TEXT,
    last_output_json TEXT,
    last_summary TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    last_event_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_t3_threads_env
    ON openalbert_t3_threads(environment_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS openalbert_t3_threads_conv
    ON openalbert_t3_threads(conversation_id, updated_at DESC)
    WHERE conversation_id <> '';
CREATE INDEX IF NOT EXISTS openalbert_t3_threads_status
    ON openalbert_t3_threads(status, updated_at DESC);

CREATE TABLE IF NOT EXISTS openalbert_t3_events (
    event_id TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    received_at TEXT NOT NULL,
    woken_at TEXT
);
CREATE INDEX IF NOT EXISTS openalbert_t3_events_thread
    ON openalbert_t3_events(thread_id, received_at DESC);
CREATE INDEX IF NOT EXISTS openalbert_t3_events_pending_wake
    ON openalbert_t3_events(thread_id, received_at ASC)
    WHERE woken_at IS NULL AND kind IN ('thread_waiting_input', 'thread_completed', 'thread_failed');

CREATE TABLE IF NOT EXISTS openalbert_logs (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at TEXT NOT NULL,
    level TEXT NOT NULL,
    msg TEXT NOT NULL,
    operation TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL DEFAULT '',
    task_id TEXT NOT NULL DEFAULT '',
    tool TEXT NOT NULL DEFAULT '',
    fields_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS openalbert_logs_conv_time
    ON openalbert_logs(conversation_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS openalbert_logs_occurred_at
    ON openalbert_logs(occurred_at);
