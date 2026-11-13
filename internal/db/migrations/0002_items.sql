-- Source-generic core. Converts the torrent-only tables into items, source
-- keys, stable file rows and libraries, keeping every user ID, API key, file
-- ID and library entry, so existing keys and download links keep working.

-- ── Credentials ─────────────────────────────────────────────────────────────

CREATE TABLE credentials (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('api')), -- more kinds arrive with their stages
    hash         BYTEA NOT NULL UNIQUE,                 -- SHA-256 of the key; the key itself is never stored
    prefix       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX credentials_user_idx ON credentials (user_id);
INSERT INTO credentials (user_id, kind, hash, prefix, created_at)
SELECT id, 'api', api_key_hash, api_key_prefix, created_at FROM users;
ALTER TABLE users DROP COLUMN api_key_hash, DROP COLUMN api_key_prefix;

-- Per-user limits set by the owner. NULL means "use the server default".
CREATE TABLE user_limits (
    user_id        BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    max_active     INT CHECK (max_active >= 0),
    max_item_bytes BIGINT CHECK (max_item_bytes >= 0),
    priority       INT NOT NULL DEFAULT 0
);

-- ── Items ───────────────────────────────────────────────────────────────────

-- What is cached and evicted, whatever source it came from. An item with a
-- non-null handle belongs to the ingest container running its fetcher.
CREATE TABLE items (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'active', 'finalizing', 'ready', 'failed', 'evicted', 'blocked')),
    phase        TEXT NOT NULL DEFAULT '', -- detail only ingest reads: starting, downloading, ...
    fetcher      TEXT NOT NULL DEFAULT '', -- who fills it: qbittorrent, ... ('' once nobody does)
    handle       BYTEA,                    -- what the fetcher needs to find its work again
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    progress     DOUBLE PRECISION NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    started_by   BIGINT REFERENCES users (id) ON DELETE SET NULL, -- whose limits the current download counts against
    priority     INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    active_since TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(), -- drives eviction
    purged_at    TIMESTAMPTZ,                         -- set once a blocked item's files are gone
    legacy_hash  TEXT                                 -- conversion only; dropped below
);

-- How a request finds its item: a torrent's info hash today; NZB and hoster
-- keys in later stages.
CREATE TABLE item_sources (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('torrent')),
    key        TEXT NOT NULL,
    item_id    BIGINT NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    payload    TEXT NOT NULL DEFAULT '', -- what the fetcher needs: a magnet link
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, key)
);
CREATE INDEX item_sources_item_idx ON item_sources (item_id);

-- Files keep their ID across eviction and re-download. 'pending' is a file in
-- the manifest of a download that is being moved into the cache.
CREATE TABLE item_files (
    id         BIGSERIAL PRIMARY KEY,
    item_id    BIGINT NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'missing' CHECK (state IN ('present', 'missing', 'pending')),
    UNIQUE (item_id, path)
);

CREATE TABLE user_items (
    id        BIGSERIAL PRIMARY KEY,
    user_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    item_id   BIGINT NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    added_via TEXT NOT NULL DEFAULT 'api',
    source_id BIGINT REFERENCES item_sources (id) ON DELETE SET NULL,
    added_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, item_id)
);
CREATE INDEX user_items_item_idx ON user_items (item_id);

-- One try by one fetcher.
CREATE TABLE fetch_attempts (
    id         BIGSERIAL PRIMARY KEY,
    item_id    BIGINT NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    fetcher    TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at   TIMESTAMPTZ,
    result     TEXT NOT NULL DEFAULT '', -- ready, failed, switched, requeued, blocked
    error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX fetch_attempts_item_idx ON fetch_attempts (item_id);

-- ── Conversion ──────────────────────────────────────────────────────────────

INSERT INTO items (name, status, fetcher, handle, size_bytes, progress, error, started_by,
                   created_at, updated_at, queued_at, active_since, completed_at, last_used_at, purged_at, legacy_hash)
SELECT name,
       CASE status WHEN 'downloading' THEN 'active' ELSE status END,
       CASE WHEN status IN ('queued', 'downloading') THEN 'qbittorrent' ELSE '' END,
       -- A download already in qBittorrent keeps saving to its old folder.
       CASE WHEN status = 'downloading'
            THEN convert_to(json_build_object('hash', infohash, 'dir', 'incoming/' || infohash)::text, 'UTF8') END,
       size_bytes, progress, error, requested_by,
       created_at, updated_at, updated_at,
       CASE WHEN status = 'downloading' THEN updated_at END,
       completed_at, last_accessed_at, purged_at, infohash
FROM torrents;

INSERT INTO item_sources (kind, key, item_id, payload, created_at)
SELECT 'torrent', t.infohash, i.id, t.magnet, t.created_at
FROM torrents t JOIN items i ON i.legacy_hash = t.infohash;

INSERT INTO item_files (id, item_id, path, size_bytes, state)
SELECT f.id, i.id, f.path, f.size_bytes, CASE WHEN i.status = 'ready' THEN 'present' ELSE 'missing' END
FROM torrent_files f JOIN items i ON i.legacy_hash = f.infohash;
SELECT setval(pg_get_serial_sequence('item_files', 'id'), COALESCE((SELECT max(id) FROM item_files), 0) + 1, false);

INSERT INTO user_items (user_id, item_id, source_id, added_at)
SELECT ut.user_id, i.id, s.id, ut.added_at
FROM user_torrents ut
JOIN items i ON i.legacy_hash = ut.infohash
JOIN item_sources s ON s.item_id = i.id;

-- Ready items still sit in store/<info hash>; ingest renames those folders
-- to store/<item id> when it starts (store.AdoptLegacy).
CREATE TABLE legacy_folders (
    item_id BIGINT PRIMARY KEY REFERENCES items (id) ON DELETE CASCADE,
    folder  TEXT NOT NULL
);
INSERT INTO legacy_folders (item_id, folder)
SELECT id, legacy_hash FROM items WHERE status IN ('ready', 'blocked');

-- Takedowns, keyed by source key so every way of asking for an item is covered.
CREATE TABLE blocks (
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    item_id    BIGINT REFERENCES items (id) ON DELETE SET NULL,
    reason     TEXT NOT NULL,
    reference  TEXT NOT NULL DEFAULT '',
    blocked_by BIGINT REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, key)
);
INSERT INTO blocks (kind, key, item_id, reason, reference, created_at)
SELECT 'torrent', b.infohash, i.id, b.reason, b.reference, b.created_at
FROM blocklist b LEFT JOIN items i ON i.legacy_hash = b.infohash;

DROP TABLE blocklist, user_torrents, torrent_files, torrents;
ALTER TABLE blocks RENAME TO blocklist;
ALTER TABLE items DROP COLUMN legacy_hash;

CREATE INDEX items_claim_idx ON items (fetcher, priority DESC, queued_at) WHERE status = 'queued';
CREATE INDEX items_work_idx ON items (fetcher, status) WHERE handle IS NOT NULL;
CREATE INDEX items_started_by_idx ON items (started_by) WHERE status IN ('queued', 'active', 'finalizing');
CREATE INDEX items_lru_idx ON items (last_used_at) WHERE status = 'ready';
