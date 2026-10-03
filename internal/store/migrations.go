package store

const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    jellyfin_user_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    can_movie INTEGER NOT NULL,
    can_series INTEGER NOT NULL,
    can_cancel INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_sessions (
    token_hash BLOB PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(jellyfin_user_id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS auth_sessions_expiry ON auth_sessions(expires_at);

CREATE TABLE IF NOT EXISTS release_searches (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(jellyfin_user_id),
    kind TEXT NOT NULL,
    tmdb_id INTEGER NOT NULL,
    tvdb_id INTEGER NOT NULL DEFAULT 0,
    season_number INTEGER,
    episode_number INTEGER,
    backend TEXT NOT NULL DEFAULT '',
    arr_item_id INTEGER NOT NULL DEFAULT 0,
    arr_episode_id INTEGER NOT NULL DEFAULT 0,
    expected_episodes_json TEXT NOT NULL DEFAULT '[]',
    state TEXT NOT NULL,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS release_searches_user ON release_searches(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS release_searches_pending ON release_searches(state, updated_at);

CREATE TABLE IF NOT EXISTS release_candidates (
    id TEXT PRIMARY KEY,
    search_id TEXT NOT NULL REFERENCES release_searches(id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE,
    token_cipher BLOB NOT NULL,
    payload_cipher BLOB NOT NULL,
    title TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    seeders INTEGER,
    quality TEXT NOT NULL,
    indexer_name TEXT NOT NULL,
    protocol TEXT NOT NULL,
    approved INTEGER NOT NULL,
    rejected INTEGER NOT NULL,
    rejections_json TEXT NOT NULL,
    full_season INTEGER NOT NULL,
    release_season_number INTEGER,
    episode_numbers_json TEXT NOT NULL,
    expected_episodes_json TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    consumed_job_id TEXT
);
CREATE INDEX IF NOT EXISTS release_candidates_search ON release_candidates(search_id);
CREATE INDEX IF NOT EXISTS release_candidates_expiry ON release_candidates(expires_at);

CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(jellyfin_user_id),
    candidate_id TEXT NOT NULL UNIQUE REFERENCES release_candidates(id),
    kind TEXT NOT NULL,
    tmdb_id INTEGER NOT NULL,
    tvdb_id INTEGER NOT NULL DEFAULT 0,
    season_number INTEGER,
    episode_number INTEGER,
    backend TEXT NOT NULL,
    arr_item_id INTEGER NOT NULL,
    arr_episode_id INTEGER NOT NULL DEFAULT 0,
    arr_queue_id INTEGER NOT NULL DEFAULT 0,
    expected_episodes_json TEXT NOT NULL DEFAULT '[]',
    release_guid_hash BLOB NOT NULL DEFAULT X'',
    release_indexer_id INTEGER NOT NULL DEFAULT 0,
    release_indexer_hash BLOB NOT NULL DEFAULT X'',
    release_url_hash BLOB NOT NULL DEFAULT X'',
    release_info_hash BLOB NOT NULL DEFAULT X'',
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 0,
    poll_attempt INTEGER NOT NULL DEFAULT 0,
    next_poll_at INTEGER NOT NULL DEFAULT 0,
    percent REAL,
    downloaded_bytes INTEGER,
    total_bytes INTEGER,
    bytes_per_second INTEGER,
    eta_seconds INTEGER,
    status_text TEXT NOT NULL DEFAULT '',
    progress_source TEXT NOT NULL DEFAULT '',
    download_id TEXT NOT NULL DEFAULT '',
    jellyfin_item_id TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_user_subject ON jobs(user_id, kind, tmdb_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS jobs_active ON jobs(state, updated_at);
CREATE INDEX IF NOT EXISTS jobs_poll_due ON jobs(state, next_poll_at);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id TEXT NOT NULL REFERENCES users(jellyfin_user_id),
    operation TEXT NOT NULL,
    key TEXT NOT NULL,
    request_hash BLOB NOT NULL,
    job_id TEXT NOT NULL REFERENCES jobs(id),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, operation, key)
);
CREATE INDEX IF NOT EXISTS idempotency_expiry ON idempotency_keys(expires_at);

CREATE TABLE IF NOT EXISTS events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id TEXT NOT NULL REFERENCES users(jellyfin_user_id),
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_user_sequence ON events(user_id, sequence);
`
