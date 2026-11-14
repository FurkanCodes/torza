# Torza architecture

Torza is a small debrid service. A user checks whether content is cached,
adds a magnet link or `.torrent` file, and gets signed HTTP links to download
or stream its files. Content is downloaded once into a shared cache; a second
user adding it gets it instantly. The full design, with later stages (Real-
Debrid/TorBox imports, hosters, Usenet, Stremio, WebDAV), is in
[docs/designs/torza.md](docs/designs/torza.md).

## Programs

One Go codebase, four programs, one Postgres database.

| Program | Does |
|---|---|
| `api` | REST API, test web page and the Stremio add-on (`/stremio/<secret>/…`): API keys, availability check, adding, libraries, links, owner tools. `api migrate`, `api create-user` |
| `stream` | Serves cached files to holders of signed links, with byte ranges |
| `ingest` | One loop per fetcher (`INGEST_FETCHERS`, today `qbittorrent`): claims queued items, drives the source, moves finished files into the cache |
| `scheduler` | Evicts least recently used items, deletes blocked items' files, sweeps folders no item owns |

Supporting containers: Postgres, qBittorrent, and Caddy (HTTPS, server only).
Programs wake each other with Postgres `LISTEN`/`NOTIFY` (`torza_queued_<fetcher>`,
`torza_blocked`, `torza_item_ready`), sent inside the transaction that made
the work, so a wake-up never arrives before its row. Every loop also checks on
a timer, so a lost notification only delays work.

## Data model

| Table | Holds |
|---|---|
| `items` | What is cached and evicted: status, `fetcher` (who fills it), `handle` (what the fetcher needs to resume), size, progress, `started_by`, priority, `last_used_at` |
| `item_sources` | How a request finds its item: unique `(kind, key)` (today `torrent` + info hash) and the payload (magnet) |
| `item_files` | Files with stable IDs; `present`, `missing` or `pending` |
| `user_items` | Each user's library |
| `credentials` | Hashed secrets by kind: `api` keys and `stremio` add-on links (one per device) |
| `user_limits` | Owner-set limits per user; `NULL` = server default |
| `fetch_attempts` | One row per try by a fetcher |
| `blocklist` | Takedowns by `(kind, key)` |

## Statuses

```
queued ──claim──► active ──finished──► finalizing ──► ready ──evict──► evicted
   ▲                │                      │                             │
   └── add again ───┴── failed ◄───────────┘          add / play again ──┘
any status ──block──► blocked (final)
```

## Rules

1. **Postgres is the source of truth**, and the files on disk follow it.
2. **One item per key.** Add takes a per-user lock, then a per-key lock
   (`pg_advisory_xact_lock`), so two users adding the same new key at once
   start one download, one user's parallel adds can't pass their limit, and
   a takedown racing an add always wins. Real-Postgres tests prove all three.
3. **Adding something cached or downloading is free.** Only an add that
   starts a download counts against the user's `max_active`, and it is that
   user whose size limit applies (checked once the size is known).
4. **An item with a handle belongs to its fetcher's ingest loop.** Every write
   it makes is guarded by status and fetcher; after a takedown the write
   does nothing and the loop deletes what it downloaded.
5. **The manifest is stored before files move.** Finishing is: `Finalize`
   (the file list) → one transaction storing file rows as `pending` and
   status `finalizing` → the source releases the files → rename
   `incoming/<id>` to `store/<id>` → `ready`. After a crash, the database and
   the disk decide: already in `store/` → ready; still in the download folder
   → finish the move; neither → download again.
6. **File IDs never change.** Eviction marks files `missing`; a new download
   upserts by path, so links, players and libraries keep working.
7. **Links are signed, not stored.** A token says "user U may read file F
   until T". `stream` checks the signature, then asks the service layer
   whether the user still has the item, it is ready, not blocked, and the
   file is present. Files open through `os.Root`, so no path or link in a
   torrent can reach outside the item's folder; manifests with anything but
   regular files are refused.
8. **Use is real reads.** An item's `last_used_at` moves only after a response
   sends 8 MB (or the whole of a smaller file), never on HEAD or a small
   range, and at most once a minute. Eviction never touches anything used
   within `EVICT_IDLE_FOR` (default 6 hours).
9. **Eviction counts what is arriving.** "Used" is ready bytes plus the known
   sizes of downloads in progress, so the cache makes room for them.
10. **No seeding.** A finished torrent is removed from qBittorrent at once.

## Sources

`internal/source` defines the interface every fetcher fits:
`Start` (returns a handle), `Poll`, `Finalize`, `Root`, `Release`, `Cancel`.
A source rebuilds its state from the handle and the disk after a restart,
and classes its errors as retry, permanent, or try-another-fetcher.
`ingest/policy.go` decides, after each poll, whether to keep going or give up
(no metadata within `METADATA_TIMEOUT`, or over the user's size limit); in
stage 5 it also switches to Real-Debrid or TorBox for users the owner allows.

## Disk layout

```
/data/incoming/<item id>/   downloads in progress
/data/store/<item id>/      finished items, served by stream (read-only mount)
```

`incoming/` and `store/` are on the same filesystem, so publishing is one
rename. The scheduler deletes item folders whose item isn't active,
finalizing or ready once they are an hour old.

## Settings

Required: `DATABASE_URL`, `PUBLIC_URL`, `SIGNING_KEY` (api, stream),
`QBIT_PASS` (ingest). Useful: `MAX_ACTIVE_PER_USER`, `MAX_TORRENT_BYTES`
(default per-user item limit), `CACHE_MAX_BYTES`, `CACHE_MIN_FREE_BYTES`,
`EVICT_IDLE_FOR`, `METADATA_TIMEOUT`, `LINK_TTL`, `POLL_INTERVAL`,
`INGEST_FETCHERS`.
