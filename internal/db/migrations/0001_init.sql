CREATE TABLE users (
    id             BIGSERIAL PRIMARY KEY,
    email          TEXT NOT NULL UNIQUE,
    api_key_hash   BYTEA NOT NULL UNIQUE, -- SHA-256 of the API key; the key itself is never stored
    api_key_prefix TEXT NOT NULL,         -- first characters of the key, so users can tell keys apart
    is_admin       BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per torrent, shared by every user who adds it.
CREATE TABLE torrents (
    infohash         TEXT PRIMARY KEY CHECK (infohash ~ '^[0-9a-f]{40}$'),
    name             TEXT NOT NULL DEFAULT '',
    magnet           TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'downloading', 'ready', 'failed', 'evicted', 'blocked')),
    size_bytes       BIGINT NOT NULL DEFAULT 0,
    progress         DOUBLE PRECISION NOT NULL DEFAULT 0,
    error            TEXT NOT NULL DEFAULT '',
    requested_by     BIGINT REFERENCES users (id) ON DELETE SET NULL, -- who started the current download
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT now(), -- drives cache eviction
    purged_at        TIMESTAMPTZ                          -- set once a blocked torrent's files are deleted
);
CREATE INDEX torrents_status_idx ON torrents (status);
CREATE INDEX torrents_requested_by_idx ON torrents (requested_by) WHERE status IN ('queued', 'downloading');
CREATE INDEX torrents_lru_idx ON torrents (last_accessed_at) WHERE status = 'ready';

-- Files inside each ready torrent. Paths are relative to the torrent's folder.
CREATE TABLE torrent_files (
    id         BIGSERIAL PRIMARY KEY,
    infohash   TEXT NOT NULL REFERENCES torrents (infohash) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    UNIQUE (infohash, path)
);

-- Each user's library: which shared torrents they've added.
CREATE TABLE user_torrents (
    user_id  BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    infohash TEXT NOT NULL REFERENCES torrents (infohash) ON DELETE CASCADE,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, infohash)
);
CREATE INDEX user_torrents_infohash_idx ON user_torrents (infohash);

-- Torrents removed after a takedown request. They can never be added again.
CREATE TABLE blocklist (
    infohash   TEXT PRIMARY KEY CHECK (infohash ~ '^[0-9a-f]{40}$'),
    reason     TEXT NOT NULL,
    reference  TEXT NOT NULL DEFAULT '', -- e.g. the takedown notice's ID
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
