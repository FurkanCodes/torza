# Torza architecture

Torza is a cloud torrent service. A user adds a magnet link. Torza downloads the
torrent once, keeps it in a shared cache, and gives the user signed HTTPS links
to download or stream the files. When a second user adds the same torrent, it's
already cached and ready immediately.

Version 1 handles torrents only. File-hoster links, Usenet, WebDAV, a
Real-Debrid-compatible API and payments come later and build on this design.

## Services

Torza is one Go codebase that builds into four small programs. Each runs in its
own container, and they share one Postgres database.

| Service     | What it does |
|-------------|--------------|
| `api`       | Public REST API: API-key auth, adding torrents, a user's library, download links, admin takedowns |
| `ingest`    | Hands queued torrents to qBittorrent, tracks progress, moves finished torrents into the cache |
| `stream`    | Serves cached files to anyone holding a valid signed link, with byte ranges so video can stream |
| `scheduler` | Evicts the least recently used torrents when the disk fills up |

These containers support them:

- **Postgres** holds all state.
- **NATS** carries wake-up messages between services.
- **qBittorrent** is the torrent engine.
- **Caddy** provides HTTPS in front of `api` and `stream`.

The split exists so each part can restart, scale or move to another machine on
its own. For example, adding another `ingest` + qBittorrent pair on a second
server adds download capacity without touching the API.

## Design rules

1. **Postgres is the source of truth.** A NATS message only says "there's new
   work, check now." If a message is lost or a service was down, its next
   scheduled check finds the work anyway.
2. **One copy per torrent.** A torrent is identified by its info hash. `torrents`
   has one row per hash, and `user_torrents` links users to it.
3. **Links are signed, not stored.** A link token says "user U may read file F
   until time T" and carries an HMAC signature. `stream` checks the signature,
   then confirms in the database that the user still has the torrent and it's
   still available.
4. **Takedowns win.** A blocklisted torrent can't be added, its links stop
   working, and `ingest` deletes its files.
5. **No seeding.** `ingest` removes a torrent from qBittorrent as soon as it
   finishes downloading.

## Adding a torrent

```
POST /v1/torrents {"magnet": "..."}
  │
  ├─ on the blocklist? ─────────────── 451 Unavailable For Legal Reasons
  ├─ already ready / downloading? ──── add to the user's library, done
  ├─ user already has MAX_ACTIVE_PER_USER downloads running? ── 429
  └─ otherwise: status = queued, add to library, publish torza.torrent.queued
        │
        ▼
ingest: create data/incoming/<hash>/, hand the magnet to qBittorrent → downloading
        │   every POLL_INTERVAL: record progress; fail it if it's over
        │   MAX_TORRENT_BYTES or finds no peers within METADATA_TIMEOUT
        ▼
finished: remove from qBittorrent (keep files) → rename incoming/<hash> to store/<hash>
          → record the file list → status = ready
```

## Getting a file

```
POST /v1/files/{id}/link  →  {"url": "https://<PUBLIC_URL>/d/<token>/<name>", "expires_at": ...}

GET /d/<token>/<name>  (stream)
  ├─ signature wrong ───────────────── 403
  ├─ expired ───────────────────────── 410 (create a new link)
  ├─ user no longer has the torrent ── 404
  ├─ torrent blocked ───────────────── 451
  └─ serve the file (supports Range), and mark the torrent as recently used
```

Token format: `v1.<fileID>.<userID>.<expiresUnix>.<signature>`. The signature
is HMAC-SHA256 over everything before it, keyed with `SIGNING_KEY` and
base64url-encoded.

## Data model

| Table           | Holds |
|-----------------|-------|
| `users`         | Accounts. Only a SHA-256 hash of each API key is stored |
| `torrents`      | One row per info hash: status, size, progress, who started the download, when it was last used |
| `torrent_files` | Files inside each ready torrent (path relative to the torrent folder) |
| `user_torrents` | Each user's library: which shared torrents they've added |
| `blocklist`     | Info hashes removed after takedown requests |

Torrent status:

```
queued ──▶ downloading ──▶ ready ──▶ evicted ──(added again)──▶ queued
               │
               └──▶ failed ──(added again)──▶ queued

any status ──(takedown)──▶ blocked   (permanent)
```

## Disk layout

```
$DATA_DIR/
  incoming/<infohash>/...   qBittorrent's save folder while downloading
  store/<infohash>/...      finished torrents, served by stream
```

Moving a finished torrent from `incoming/` to `store/` is one rename on the same
disk. It's instant, and a half-copied torrent can never be served. qBittorrent
and Torza's services must mount the data folder at the same path (`/data`)
because Torza tells qBittorrent where to save each torrent.

File paths inside torrents come from strangers, so `store` refuses any path
that would escape the torrent's folder (for example `../../etc/passwd`).

## Cache eviction

Every `EVICT_INTERVAL`, `scheduler` checks two limits:

- the total size of ready torrents against `CACHE_MAX_BYTES`
- free disk space against `CACHE_MIN_FREE_BYTES`

If either limit is crossed, it evicts ready torrents, least recently used first,
until both are fine. Anything used within `EVICT_IDLE_FOR` is never evicted, so
a file isn't deleted while someone is watching it. An evicted torrent is
downloaded again the next time someone adds it.

## Takedowns

An admin calls `POST /v1/admin/blocklist` with the torrent (magnet or info hash),
a reason and a reference, such as the notice ID. This:

1. adds the hash to `blocklist`, so it can never be added again
2. sets the torrent's status to `blocked`, so existing links stop working
3. wakes `ingest`, which removes it from qBittorrent and deletes its files

## Cost controls

| Setting                | Default | Why |
|------------------------|---------|-----|
| `MAX_ACTIVE_PER_USER`  | 3       | Caps how much download work one user can start at once |
| `MAX_TORRENT_BYTES`    | 100GB   | Stops one huge torrent filling the disk |
| `METADATA_TIMEOUT`     | 2h      | Gives up on dead torrents with no peers |
| `CACHE_MIN_FREE_BYTES` | 20GB    | Keeps the disk from filling up |
| `LINK_TTL`             | 6h      | Limits how long a shared link keeps working |

## Configuration

Every service reads environment variables; see `internal/config/config.go`.
Required in production: `DATABASE_URL`, `NATS_URL`, `PUBLIC_URL`, `SIGNING_KEY`
(`openssl rand -hex 32`), and `QBIT_PASS` for `ingest`.

## Not built yet

- File-hoster links, Usenet
- WebDAV, Real-Debrid-compatible API, Stremio/Sonarr integrations
- Choosing which files in a torrent to download
- Web interface, signup, plans and payments
- Public takedown form (admins enter takedowns through the API for now)
- Per-key rate limiting
- Multiple download servers
