# Torza: a personal multi-source debrid service

*   **Author:** Furkan
*   **Status:** Proposed
*   **Date:** 2026-11-14

--------------------------------------------------------------------------------

## Overview

Torza is a small, private debrid service built to learn how services like
Real-Debrid and TorBox work inside. It keeps one shared cache of downloaded
content on a single server. Each piece of content is downloaded once and
stored once, however many users ask for it.

The cache is filled from four sources:

*   torrents, downloaded by qBittorrent on the server
*   Real-Debrid and TorBox, using the owner's own accounts, with the files
    copied into Torza's cache
*   file-hoster links, using the owner's own premium accounts
*   Usenet, downloaded by Torza's own Usenet client

Up to about ten invited users reach the cache through a REST API, a web UI,
a Stremio library add-on, WebDAV and a Telegram bot. Around that core sit RSS
auto-download, a metadata catalog with titles and posters, export of ready
items to a user's own cloud storage, and per-user limits set by the owner.

This document covers the whole system, built in 15 stages (0–14). Stage 1
replaces the identity model of the existing torrent-only code. Later stages
add sources and access methods on top of it without rewriting the core.

## The Problem

**What the owner wants to learn.** Debrid services look simple from the
outside: paste a link, get a fast download. Inside, they solve a set of hard
problems: deciding when two requests are for the same content, sharing one
copy between many users, choosing what to delete when the disk fills,
downloading through several very different protocols, and serving large
files with seeking to players that save links. Building a working service is
the way to learn these.

**Why the existing code can't simply be extended.** Torza already has a
torrent-only first version. It runs locally on the owner's Mac but has never
run on a server, and two design reviews found problems that make it a poor
base for more sources:

*   **The torrent info hash is used for everything.** It is the item's
    database key, its folder on disk, the takedown key and part of the API
    routes. Usenet and hoster content has no info hash.
*   **File IDs change every time an item is downloaded again.** Stremio,
    WebDAV clients and saved links all break after an evicted item comes
    back.
*   **Four confirmed bugs in database steps.** Two of them, the takedown race
    and the duplicate-add race, are already fixed in the torrent-only code
    with `pg_advisory_xact_lock` (user lock, then a per-torrent lock; Block
    takes the torrent lock) and proven by real-Postgres tests in
    `internal/db/pg_test.go` that failed before the fix. The other two
    remain: an unsafe order of steps when a download finishes, and no way
    for a worker to claim a job, so two workers would download the same
    thing.
*   **The download code is written around qBittorrent.** There is no general
    source interface for the other three sources to fit into.

**The proposed solution.** Rebuild the core around a source-independent
model: an *item* (what is cached and evicted), one or more *source keys*
pointing at it, stable *file rows*, and a per-user *library*. All work goes
through one source interface and one shared service layer. The core is
proven with real-database tests and a first deploy using torrents only.
After that, each new source or access method is a separate stage that plugs
into the same core.

## Goals

**Core (stages 0–2)**

*   **One copy per item.** Two users adding the same content, at the same
    moment or days apart, results in exactly one download and one copy on
    disk, and the item appears in both users' libraries.
*   **Stable file IDs.** A file's ID never changes when its item is evicted
    and downloaded again.
*   **The four round-one bugs are fixed and each is proven** by a test that
    runs against a real Postgres database, locally and in CI.
*   **Crash safety.** Killing any Torza process at any moment, then
    restarting it, leaves no item stuck, downloaded twice, or deleted after
    finishing.
*   **One source interface** that torrents, two-step remote services
    (Real-Debrid, TorBox, hosters) and Usenet all fit. Stage 1 proves this
    with qBittorrent plus a fake two-step test source.
*   **Takedowns work across sources.** A blocked item can't be downloaded
    again or served through any source key or access method.
*   **Eviction by recent use.** When the cache is over its limit,
    least-recently-used items go first. An item being streamed is marked as
    used while it plays, and nothing used within `EVICT_IDLE_FOR` (a few
    hours) is evicted.
*   **A first deploy on a cheap VPS** passes a scripted smoke test.

**Works like a debrid service (stage 1)**

*   **Instant availability check.** A client can ask, for up to 100 magnets
    or hashes at once, which are cached, which are downloading and which
    are unknown, without starting any download.
*   **Adding something cached is instant.** The add returns `ready` with its
    files in the same response.
*   **Magnet, hash or `.torrent` upload** in one add endpoint.
*   **Direct links.** One signed link per file, or links for every file of a
    ready item in one call.
*   **Account view.** `GET /v1/me` shows the user's limits, active
    downloads, and library size.

**Sources (stages 3, 5, 6, 9)**

*   Real-Debrid and TorBox imports that fill the same item, keyed by the
    same source key, as a qBittorrent download of that torrent would.
*   File-hoster downloads through the owner's premium accounts.
*   Torza's own Usenet client: NNTP over TLS, a connection pool, yEnc
    decoding, PAR2 checking and repair, and unpacking.

**Access (stages 4, 7, 8, 12)**

*   A Stremio add-on that lists the user's library and plays only content
    that is cached or that the user added.
*   WebDAV for players and file managers, listing ready items only.
*   A server-rendered web UI for adding items, browsing the library and, for
    the owner, setting per-user limits.
*   A Telegram bot that adds items and sends play links.

**Around the core (stages 10, 11, 13)**

*   RSS auto-download from feeds a user supplies, filtered by resolution,
    release type, codec, HDR and group.
*   A metadata catalog that adds titles and posters from an official
    metadata API, with manual correction.
*   Export of a ready item to a user's own cloud storage through rclone.

**Limits and access control**

*   The owner sets per-user limits: concurrent downloads, maximum size and
    priority. They apply only when an add starts a new download. Adding
    something already cached is always free.
*   qBittorrent is always tried first. Real-Debrid and TorBox imports are
    off for test users unless the owner turns on automatic fallback for that
    user, or triggers an import by hand. Hoster links are owner-only.
*   Each access method uses its own credential, which can be revoked on its
    own.

**Working constraints**

*   The owner runs Torza locally on a Mac with Docker Desktop:
    `deploy/local.sh up|down|logs|ps|user|lan on|off` with
    `deploy/docker-compose.local.yml`. Caddy is off locally; the API is on
    `:8080` and `stream` on `:8081`, reachable from the Mac only unless
    `lan on` opens them to the home Wi-Fi. `go build`, `go vet` and
    `go test` run on the Mac; database tests run locally against a
    throwaway Postgres named by `TEST_DATABASE_URL`, and always in CI.
*   Every stage after the first deploy is deployed and smoke-tested before
    the next one starts.

## Non-Goals

*   Billing, paid plans, payments, a reseller program and public signup.
*   Legal compliance work and market fit.
*   Searching or scraping torrent sites, scraping IMDb, and scene or
    release-site scrapers.
*   Routing downloads through a VPN.
*   A Stremio add-on that searches for content. It lists only what is cached
    or what the user added.
*   A Real-Debrid-compatible API. It can be revisited after stage 4, since
    the native API now covers the same operations (check, add, info, links,
    delete, account).
*   Streaming while downloading, including Usenet streaming (stage 14, only
    when needed).
*   Recognising the same release arriving from two different sources
    (stage 14, only when needed).
*   Storing the shared cache on cloud storage. The cache stays on the
    server's local disk; cloud storage is only an export target.
*   Per-user shares of the disk or per-user eviction. Eviction is one global
    rule.
*   Running more than one server. Torza runs one container per fetcher on
    one node.

## Open Questions

*   None. Wake-ups (Postgres `LISTEN`/`NOTIFY`), unpacking (7-Zip's `7zz`)
    and the Telegram bot (webhook inside `api`) were decided on 2026-11-13.

## Critical User Journeys (CUJs)

### CUJ 1: Two users add the same torrent

*   **Given**: The torrent is not cached. User A and user B are both under
    their concurrency limits.
*   **When**: Both add its magnet link within the same second.
*   **Then**: qBittorrent downloads it once. Both libraries show the item.
    Only one active download is counted, against whichever add started it.
    When it finishes, both users can play it.

### CUJ 2: Playing an evicted item from Stremio

*   **Given**: An item in user A's library was evicted last week.
*   **When**: User A opens it in Stremio and presses play.
*   **Then**: Torza queues it again and Stremio shows it as downloading.
    When it is ready, the same Stremio entry plays, with the same file IDs
    as before.

### CUJ 3: The owner blocks an item while someone adds it

*   **Given**: An item is being added by a user.
*   **When**: The owner blocks it at the same moment.
*   **Then**: The item ends blocked. It is never downloaded, never served
    through any access method, and adding it again through any source key
    is refused.

### CUJ 4: A crash while a download finishes

*   **Given**: A download has finished and is being moved into the cache.
*   **When**: The ingest process is killed halfway through.
*   **Then**: After a restart, the item becomes ready without being
    downloaded again, and no finished files are deleted.

### CUJ 5: A test user adds a slow torrent

*   **Given**: The owner has turned on automatic RD fallback for user C
    (after 10 minutes without metadata). User D does not have it.
*   **When**: Both add torrents whose metadata doesn't arrive.
*   **Then**: After 10 minutes, user C's item is imported from the owner's
    RD account into the same item. User D's item keeps waiting in
    qBittorrent until `METADATA_TIMEOUT`, using none of the owner's RD
    quota.

### CUJ 6: Watching through WebDAV while a media server scans

*   **Given**: A media server has the WebDAV share mounted, and some items
    in the library are evicted.
*   **When**: The media server scans every file.
*   **Then**: Only ready items are listed. Nothing is queued again, and
    HEAD, PROPFIND or a short read doesn't count as use for eviction.

### CUJ 7: An RSS feed matches a new release

*   **Given**: User E has an RSS feed with a 1080p WEB-DL filter.
*   **When**: A matching entry appears.
*   **Then**: It is added at the lowest priority, within user E's limits;
    if user E is at the limit, the entry is left unseen and tried again on
    the next poll. It is never fetched again after being evicted unless
    someone asks for it. A feed URL pointing at an internal address is
    refused.

### CUJ 8: A user checks a list of magnets before adding

*   **Given**: User F has 20 magnets from a search site; three are in the
    cache, one is downloading for another user, one is blocked.
*   **When**: User F sends all 20 to `POST /v1/items/check`, or pastes one
    into the web page.
*   **Then**: Three come back `ready` with file names and sizes, one
    `downloading`, one `unavailable`, the rest `unknown`. Nothing starts
    downloading and nothing is added to user F's library. The web page
    shows "Cached — instant" or "Will download". Adding a `ready` one
    returns `ready` with its files at once.

## Technical Plan (Architecture)

### 1. Programs and containers

Torza stays one Go codebase built into four programs, each in its own
container, sharing one Postgres database. Later stages add packages to them,
not new programs.

| Program | Holds | Added in stage |
|---|---|---|
| `api` | REST API, embedded web page (later the server-rendered UI), Stremio add-on, admin, Telegram webhook; `api migrate` runs migrations once per deploy and exits | REST now; Stremio 4, web UI 8, Telegram 12 |
| `stream` | Serves file bytes with signed links and byte ranges; WebDAV under `/dav/` | WebDAV 7 |
| `ingest` | Runs fetchers. One loop per fetcher; `-fetchers=qbittorrent,realdebrid` picks which ones this container runs | qbittorrent 1, realdebrid/torbox 5, hoster 6, usenet 9 |
| `scheduler` | Background jobs: eviction, purging blocked items with no handle, the orphan sweep, RSS polling, catalog lookups, exports | RSS 10, catalog 11, export 13 |

Supporting containers: Postgres 17, qBittorrent and, on the server, Caddy
(HTTPS). Stage 1 removes NATS, which the running code still uses: programs
wake each other with Postgres `LISTEN`/`NOTIFY` (channels
`torza_queued_<fetcher>` and `torza_item_ready`), sent inside the
transaction that creates the work, so a wake-up never arrives before the
row is visible. Every loop also checks on a timer, so a missed notification
only delays work until the next check. On the server, Usenet ingest runs as
a second `ingest` container (`-fetchers=usenet`) with its own CPU and memory
limits, because PAR2 repair can use all CPU for hours.

Locally, `deploy/docker-compose.local.yml` overrides the server compose
file: no Caddy, `api` on `:8080`, `stream` on `:8081`.

```text
                   users (Stremio, players, browser, Telegram)
                                    │ HTTPS
                                 ┌──▼───┐
                                 │Caddy │  (server only)
                                 └┬────┬┘
                  /, /v1, /stremio     /d, /dav
                       ┌──────▼─┐    ┌─▼──────┐
                       │  api   │    │ stream │──reads──┐
                       └───┬────┘    └───┬────┘         │
                           │ internal/service (shared rules)
                           ▼             ▼              │
                     ┌─────────────────────────┐        │
                     │        Postgres         │        │
                     │ items, sources, files,  │        │
                     │ libraries, blocklist    │        │
                     └──▲───────────────────▲──┘        │
        claim / report  │                   │ evict,    │
                  ┌─────┴──────┐      ┌─────┴─────┐ RSS │
                  │  ingest    │      │ scheduler │     │
                  │ qbittorrent│      └─────┬─────┘     │
                  │ realdebrid │            │ deletes   │
                  │ usenet ... │            ▼           │
                  └─┬───┬────┬─┘      ┌─────────────────▼──┐
       qBittorrent ◄┘   │    └─NNTP─► │ /data/store (local │
       RD / TorBox ◄─HTTP           ▲ │ disk, the cache)   │
       hosters    ◄─HTTP            │ └────────────────────┘
                    downloads land in /data/incoming, then are renamed in
```

Only `ingest` holds source credentials (qBittorrent, RD/TorBox, hosters,
Usenet providers). Only `api` and `stream` hold the link-signing key. Config
has two structs: one for the HTTP programs (`api`, `stream`) and one for the
workers (`ingest`, `scheduler`).

### 2. Data model

The new tables arrive in a forward migration, `0002_items.sql`, which
converts the torrent-only tables in place: user IDs, API keys (moved to
`credentials`), file IDs and libraries are kept, so existing keys and links
keep working. (The first plan rewrote `0001`; by the time stage 1 was built,
the local run held real users and cached items worth keeping.) Ingest renames
finished folders from `store/<info hash>` to `store/<item id>` on start, from
a `legacy_folders` list the migration fills. `Migrate` refuses a database
that recorded a migration this build doesn't have. Migrations only move
forward.

| Table | Purpose and key columns |
|---|---|
| `users` | `id`, `email`, `is_admin`, `created_at` |
| `credentials` | One row per key a user holds: `id`, `user_id`, `kind`, `hash`, `prefix`, `created_at`, `last_used_at`, `revoked_at`. Stage 1 issues only `api` keys; other kinds arrive with their stages |
| `user_limits` | `user_id`, `max_active`, `max_item_bytes`, `priority`, `rd_fallback_after` (null = off) |
| `items` | What is cached and evicted: `id`, `name`, `status`, `phase`, `size_bytes` (null until metadata), `progress`, `error`, `started_by`, `priority`, `fetcher` (who fills it now), `handle bytea` (the fetcher's state; non-null means `ingest` owns the item), `claimed_by` (debug label only), `progress_updated_at`, `last_used_at`, `completed_at`, `purged_at`, timestamps |
| `item_sources` | How a request is matched to an item: unique `(kind, key)`, `item_id`, `payload` (magnet, NZB file reference, URL) |
| `item_files` | `id` (stable), `item_id`, `path`, `size_bytes`, `state` (`present`, `missing`); unique `(item_id, path)` |
| `user_items` | A user's library: `id`, `user_id`, `item_id`, `added_via` (`api`, `rss`, `telegram`, ...), `source_id`, `added_at`; unique `(user_id, item_id)` |
| `fetch_attempts` | One try by one fetcher: `item_id`, `fetcher` (`qbittorrent`, `realdebrid`, `torbox`, `hoster`, `usenet`), `account_id` (nullable), `started_at`, `ended_at`, `result`, `error_class` |
| `blocklist` | `(kind, key)` primary key, optional `item_id`, `reason`, `reference`, `blocked_by`, `created_at` |
| Later stages | `provider_accounts` (5); `invites` (8); `items.reserved_bytes` (9); `feeds`, `feed_entries_seen` (10); `item_titles` (11); `exports`, `user_remotes` (13) |

Source keys, by kind:

*   **torrent**: lowercase v1 (SHA-1) info hash, taken from parsed metainfo
    (magnet or `.torrent` file), never from the raw text. A v2-only magnet
    is rejected with 400; registering both hashes of hybrid torrents can
    wait.
*   **nzb**: SHA-256 of the sorted article message IDs in the NZB, so
    different indexers' NZBs for the same post match.
*   **hoster**: host plus the hoster's file ID, from a per-host URL parser.
*   **RD/TorBox imports of torrents** use the torrent key and fill the same
    item. TorBox Usenet or web downloads, which have no hash, are keyed by
    their original NZB or URL.

### 3. Item states

`status` is small and shown to users and players. `phase` is detail that
only `ingest` reads.

| status | meaning | allowed next |
|---|---|---|
| `queued` | waiting for its fetcher | `active`, `blocked` |
| `active` | a worker holds it (`handle` set) | `finalizing`, `queued` (`Switched`, or startup recovery), `failed`, `blocked` |
| `finalizing` | manifest stored, files being moved into the store | `ready`, `queued` (no files on disk), `failed` (Permanent), `blocked` |
| `ready` | served | `evicted`, `blocked` |
| `failed` | gave up; a new add re-queues | `queued`, `blocked` |
| `evicted` | files deleted, rows kept | `queued`, `blocked` |
| `blocked` | taken down | none |

`phase` values: `starting`, `waiting_remote`, `downloading`, `verifying`,
`repairing`, `unpacking`. All transitions go through one pure-Go function,
`Next(status, event) (status, error)`, backed by a table, so every allowed
and forbidden move is unit-tested without a database.

### 4. Source interface

```go
type Source interface {
    Kind() string // the fetcher name: "qbittorrent", "realdebrid", ...
    Start(ctx context.Context, job Job) (Handle, error)
    Poll(ctx context.Context, h Handle) (Report, error)
    Finalize(ctx context.Context, h Handle) (Manifest, error)
    Release(ctx context.Context, h Handle) error
    Cancel(ctx context.Context, h Handle, deleteData bool) error
}

// Handle: opaque bytes the source needs to find its work again; stored in items.handle.
// Report: Phase, BytesDone, BytesTotal (0 until known), ErrClass, NextPollAfter.
// Manifest: the final list of regular files (path, size) under incoming/<item id>/.
// Release: stop holding the files, keep the data (qBittorrent: remove the torrent).
// ErrClass: Retry, Permanent, TryAnotherFetcher.
```

Rules every source must follow:

1.  **Poll survives restarts.** After a crash, `Poll` rebuilds its state from
    the stored Handle and what is on disk under `incoming/<item id>/`. For
    `qbitsource`, if qBittorrent has lost the torrent, it re-adds it from
    the Handle and qBittorrent rechecks the data already on disk (as
    `worker.go` does today).
2.  **File rows come only from `Finalize`.** Any earlier file list is an
    estimate. `Finalize` builds its manifest through the store, which
    rejects anything that is not a regular file (symlinks, devices). This
    covers Usenet, where repair and unpacking produce different files from
    the NZB's list.
3.  **Release and Cancel can be called twice** without error.
4.  **Two-step sources** (RD, TorBox, hosters) report `waiting_remote` while
    the remote side prepares, then do the HTTP download with resume inside
    the same implementation. The interface has no extra method for this.

Fetchers and the key kinds they fill: `qbittorrent` (torrent), `realdebrid`
and `torbox` (torrent, plus their own Usenet/web items), `hoster` (hoster),
`usenet` (nzb). The HTTP downloader is one shared package used by RD,
TorBox and hosters.

**Fetch policy.** One decision table in `internal/ingest/policy.go` decides,
from the item, its attempts and the starting user's limits, whether to keep
waiting, switch fetcher or fail. Stage 1 rows:

| Condition | Action |
|---|---|
| No metadata after the starting user's `rd_fallback_after`, and an RD/TorBox fetcher is registered | Switch to it |
| No metadata after `METADATA_TIMEOUT` (default 2 h) | Fail ("no peers found") |
| Size known and above `started_by`'s `max_item_bytes` | Fail (Permanent) |
| Source reports `TryAnotherFetcher` | Switch if another fetcher can fill the key, else fail |

A switch is one transaction: cancel the current fetcher (for qBittorrent:
remove the torrent and its data), close its `fetch_attempts` row, set
`fetcher`, clear `handle`, set `status=queued`, and notify
`torza_queued_<fetcher>`. It is one state event, `Switched`
(`active` → `queued`). The owner can start an import for any item by hand
(stage 5). Hoster and RD/TorBox sources can be added only by the owner.
Each try is a row in `fetch_attempts` naming its fetcher.

### 5. Claiming work and ownership

Each fetcher has its own loop in `ingest`. A loop claims work with one
statement:

```sql
UPDATE items SET status='active', claimed_by=$label
WHERE id IN (
  SELECT id FROM items
  WHERE status='queued' AND fetcher=$fetcher
  ORDER BY priority DESC, created_at
  FOR UPDATE SKIP LOCKED LIMIT $n)
RETURNING ...
```

Ingest stops claiming while `statfs` free space is below
`CACHE_MIN_FREE_BYTES` (section 8). There are no leases: one container runs
each fetcher, so ownership is fixed by these rules.

*   **An item with a non-null `handle` belongs to `ingest`.** Every worker
    write is guarded by `WHERE id=$id AND status IN ('active','finalizing')
    AND fetcher=$fetcher`. If the guard matches no row (the item was blocked
    or switched), the worker calls `Cancel(deleteData=true)` and
    `Store.Remove` itself and stops. `scheduler` only purges items with no
    handle.
*   **Startup recovery.** When a fetcher loop starts, it sets its own
    `active` items back to `queued` and keeps their handle, so the next
    claim resumes instead of restarting (Source rule 1), then runs the
    finishing recovery in section 6.

### 6. Finishing a download

1.  Call `Finalize` to get the manifest. The source still holds its files.
2.  In one transaction (guarded as above): upsert `item_files` by
    `(item_id, path)` with state `missing`, mark paths not in the manifest
    `missing`, set `status=finalizing`.
3.  Call `Release`: the source stops holding the files (qBittorrent: remove
    the torrent, keep the data).
4.  Rename `incoming/<id>` to `store/<id>`. Never delete an existing
    `store/<id>`.
5.  In one transaction: mark the manifest's files `present`, set
    `status=ready` and `completed_at`, clear `handle`, and send
    `NOTIFY torza_item_ready` with the item ID.

**Recovery** reads the database, never a file on disk. For every
`finalizing` item at startup and on every tick: if `store/<id>` exists, do
step 5; if only `incoming/<id>` exists, do steps 3–5; if neither exists,
set it back to `queued` with its handle cleared (it downloads again). A
`Permanent` error from `Release` sets it `failed`.

**Orphan sweep** (scheduler): deletes `store/<id>` and `incoming/<id>`
folders whose item is not `active`, `finalizing` or `ready`. At startup,
`qbitsource` removes torrents in Torza's qBittorrent category that have no
`active` item.

### 7. Shared service layer

`internal/service` is a Go package used in-process by `api`, `stream` and
`scheduler`. Access packages (REST, web UI, Stremio, WebDAV, Telegram, RSS)
call it and never use `internal/db` directly. It owns:

*   **Locks.** Add and Block take `pg_advisory_xact_lock` on a hash of each
    source key; Add takes the user lock first, then the key lock, the same
    order the torrent-only code uses today. This serialises CUJ 3.
*   **Add**: parse the input into a source key; check the blocklist; find or
    create the item; add it to the user's library. A `ready` item returns
    at once with its files. A new, `failed` or `evicted` item goes through
    `Enqueue`. A `blocked` item is never overwritten. One transaction.
*   **Enqueue(user, item, via)**: the only path to `queued`, used by Add,
    Stremio play, RSS, Telegram and owner imports. Under the user lock it
    counts the user's active downloads against `max_active` (429 when
    full), sets `started_by`, `fetcher` (the first fetcher for the key
    kind) and `priority` from the enqueuing user (RSS uses 0), and sends
    `NOTIFY torza_queued_<fetcher>`. Priority is set when queued; a later
    add while queued raises it with `GREATEST`; it is never lowered. The
    size check runs in `ingest` after metadata, against `started_by`'s
    `max_item_bytes`.
*   **Check**: for up to 100 inputs, parse each into a key and return
    `ready` (with file names and sizes), `downloading` (queued, active or
    finalizing), `unknown` (no item, failed or evicted) or `unavailable`
    (blocked key). Read-only; touches no library and starts nothing.
*   **Library**: list, remove (removes the library row only), item detail.
*   **Links**: check the user has the item, it isn't blocked by any of its
    source keys, and it is ready; return a signed link for one file, or for
    every present file of the item.
*   **Account**: email, `max_active`, `max_item_bytes`, current active
    count, library item count and total library bytes.
*   **Block**: under the key locks, set the item `blocked` and add every
    source key of the item to the blocklist, in one transaction, and notify
    the item's fetcher so its worker hits the guard. `ingest` deletes files
    of items with a handle; the purge job deletes the rest.
*   **Authorize**: look up a credential of the expected kind, reject revoked
    ones, update `last_used_at`.
*   **Limits**: read and set `user_limits` (owner only).

### 8. Storage, disk space and eviction

`store.Local` stays a concrete type, keyed by item ID:

```go
func (s *Local) Open(itemID int64, path string) (*os.File, int64, error) // through os.Root
func (s *Local) Manifest(itemID int64) (source.Manifest, error)          // regular files only
func (s *Local) Publish(itemID int64) error                               // rename incoming/<id> -> store/<id>
func (s *Local) Remove(itemID int64) error
func (s *Local) Free() (int64, error)                                     // statfs
```

`Open` opens files through `os.Root` (Go 1.26), so a path can't leave the
item's folder, and keeps the `filepath.IsLocal` check. `incoming/` and
`store/` sit on the same filesystem so the rename is atomic.

**Disk space** is best effort in stage 1. There are no reservations: a
magnet has no size when it is claimed. "Used" is ready bytes plus the known
sizes of `active` items; claims and eviction use the same number, so they
can't wait on each other. As a backstop, ingest stops claiming while
`statfs` free space is below `CACHE_MIN_FREE_BYTES`. Size multipliers for
Usenet and one advisory lock around claiming arrive in stage 9.

**Eviction** (scheduler, one copy under an advisory lock): when used bytes
pass `CACHE_MAX_BYTES` or free space falls below the minimum, delete the
least-recently-used `ready` items not used within `EVICT_IDLE_FOR`, mark
their file rows `missing`, set `status=evicted`. Library rows stay.

**What counts as use.** `stream` updates `last_used_at` (throttled to once
a minute per item, as `TouchTorrent` does today) only after the first
`USE_TOUCH_BYTES` (default 50 MB) written in a response, and again while
it keeps writing. HEAD and PROPFIND never count. `EVICT_IDLE_FOR` rises
from 15 minutes to 4 hours, so a paused film isn't evicted.

### 9. Links and credentials

*   Signed links (`/d/<token>`) carry a `v1.` prefix, user ID, file ID and
    expiry, with an HMAC signature. Only the current key is accepted;
    changing the key ends live links early, which is acceptable with
    six-hour links. `stream` checks the signature, then asks the service
    layer whether the user may still read the file (451 if blocked, 410 if
    expired).
*   Stage 1 has only `api` credentials, created by `api create-user`.
    Stremio play links (`/stremio/<credential>/play/<file id>`, stage 4),
    WebDAV app passwords (7), web sessions with invites (8) and Telegram
    chat links (12) each add their credential kind in their stage.

### 10. Later stages

Each later stage adds packages on the same core; the files are listed in
the Detailed Implementation Plan.

| Stage | Adds | Lives in |
|---|---|---|
| 3 | Usenet spike: NZB key, NNTP/TLS, yEnc, fake NNTP | `ingest` (not registered yet) |
| 4 | Stremio library add-on, IDs `torza:<item id>`, play route | `api` |
| 5–6 | Shared HTTP downloader; RD/TorBox six-step sources; hosters | `ingest` |
| 7 | Read-only WebDAV over ready items | `stream` |
| 8 | Server-rendered web UI, built on the stage 1 web page | `api` |
| 9 | PAR2 verify and repair in Go; unpacking with `7zz`; size reservations | `ingest` (`-fetchers=usenet`) |
| 10 | Release-name parser; RSS through a guarded HTTP client | `scheduler` |
| 11 | Metadata catalog, `item_titles` | `scheduler`, shown in `api` |
| 12 | Telegram webhook `/telegram/<secret>` | `api` |
| 13 | Export of ready items with `rclone copy` | `scheduler` |

### 11. Testing and deployment

*   **Unit tests** (run everywhere): state table, policy table, source
    contract tests against the fake source, link signing, parsers.
*   **Database tests**: a `dbtest` helper (the current `testRepo` in
    `internal/db/pg_test.go`, moved into its own package) creates a separate
    schema per test and runs the migrations. Without `TEST_DATABASE_URL`
    these tests skip locally; in CI (`CI=true`) a missing URL fails the run.
    Locally they run against a throwaway Postgres container.
*   **Fake upstreams:** fake qBittorrent in stage 0; fake NNTP in stage 3;
    fake RD, TorBox and hosters in stages 5–6.
*   **Smoke test:** a script run after each deploy, using named legal test
    torrents, that exits 0 or 1. It also runs against the local stack.
*   **Deploy:** images tagged with the git commit; `pg_dump` before each
    deploy; rollback means restoring the dump, running the previous tag,
    and letting the orphan sweep remove folders the old database doesn't
    know.
*   **Health:** readiness checks that ping Postgres, and a small `/metrics`
    page: queue depth per fetcher, oldest active item age, used and free
    bytes, evicted bytes.

## Alternatives Considered

### Alternative A: Keep the torrent schema and add migrations on top

*   **Pros**: No rewrite of the first migration; the existing tests keep
    working unchanged.
*   **Cons**: The info hash stays the item's key, folder name, takedown key
    and route parameter. Every new source would need its own copy of these
    tables or a fake hash. File IDs would keep changing on re-download.
*   **Reason for Rejection**: Nothing is deployed to a server, so a rewrite
    costs no data. Adding sources later on a torrent-shaped schema would
    mean rewriting the core queries at every stage.

### Alternative B: Drive SABnzbd or NZBGet for Usenet

*   **Pros**: Much less work. Mature repair and unpacking. A second
    external tool would also test the source interface.
*   **Cons**: Hides the parts of Usenet the project exists to teach:
    NNTP, article fetching, yEnc, PAR2 repair.
*   **Reason for Rejection**: The goal is learning how these services work
    inside. The own client is split into download, repair and unpack, and a
    spike comes early (stage 3) to limit the risk.

### Alternative C: Pass Real-Debrid and TorBox links through instead of copying

*   **Pros**: No disk use or download time for imported content; instant
    for anything RD already has.
*   **Cons**: That content would sit outside Torza's cache, so "stored
    once", eviction, takedowns and stable file IDs wouldn't apply to it.
    Every test user's play would spend the owner's RD traffic.
*   **Reason for Rejection**: It turns Torza into a front end for someone
    else's cache, which teaches nothing about running one.

### Alternative D: Per-user eviction budgets or pin quotas

*   **Pros**: Each user would get a guaranteed share of the disk.
*   **Cons**: An item held by several users has no fair owner: charging
    everyone over-counts the disk, splitting the cost changes users' usage
    when others leave, charging the first requester lets others ride free.
    Evicting for one user deletes the item for everyone.
*   **Reason for Rejection**: Large debrid services don't offer this either.
    One global least-recently-used rule is simpler. Pins can be added later
    by making eviction skip pinned items, without changing the schema
    around them.

### Alternative E: Keep the shared cache on cloud storage through rclone

*   **Pros**: Disk size no longer limited by the VPS.
*   **Cons**: Loses the atomic rename used to publish downloads, accurate
    free-space checks, and cheap byte-range reads (every seek becomes a
    remote call). An rclone FUSE mount in Docker needs extra privileges.
*   **Reason for Rejection**: The cache stays on local disk. Cloud storage
    is only an export target (stage 13).

### Alternative F: Let every user's request use the owner's RD/TorBox accounts

*   **Pros**: Faster results for everyone; slow torrents stop mattering.
*   **Cons**: Test users would spend the owner's quota and traffic without
    limit.
*   **Reason for Rejection**: qBittorrent goes first for everyone. RD/TorBox
    import is either triggered by the owner or turned on per user.

### Alternative G: A Real-Debrid-compatible API

*   **Pros**: Existing tools that only speak RD's API could use Torza.
*   **Cons**: A second API to maintain, copying another service's status
    words and quirks, with no specific app needing it.
*   **Reason for Rejection**: Dropped for now. Torza's own API covers the
    same operations, and Stremio and WebDAV cover the planned uses. It can
    be revisited after stage 4.

### Alternative H: Separate programs for each access method

*   **Pros**: Each could restart and scale on its own.
*   **Cons**: More containers, more config, and the same rules repeated in
    more places, on a single small server with at most ten users.
*   **Reason for Rejection**: Access methods are packages inside `api` and
    `stream` that share one service layer. Only Usenet ingest gets its own
    container, for its CPU use. Telegram is a webhook route inside `api`.

### Alternative I: Identify items by their content

*   **Pros**: The same release from two different sources would be stored
    once.
*   **Cons**: Content is only known after both copies have downloaded, so it
    can't be used to avoid the second download. Matching is error-prone.
*   **Reason for Rejection**: Items are matched by source key. Matching by
    content is an optional later step (stage 14).

### Alternative J: Key NZBs by a hash of the NZB file

*   **Pros**: Simple.
*   **Cons**: Different indexers produce different NZB files for the same
    post, so the same content would be downloaded twice.
*   **Reason for Rejection**: The key is a hash of the sorted article
    message IDs inside the NZB.

### Alternative K: Hand out work through the NATS queue group

*   **Pros**: Already in the code.
*   **Cons**: NATS only spreads wake-up messages. Every worker still reads
    every queued row from Postgres, so two workers download the same thing.
    A lost message or a crash leaves no record of who held the work.
*   **Reason for Rejection**: Claiming uses one Postgres `UPDATE ... SKIP
    LOCKED`, and wake-ups use Postgres `LISTEN`/`NOTIFY`. NATS is removed:
    on one server it adds a container and a secret without adding anything
    Postgres can't do.

### Alternative L: Stream while downloading

*   **Pros**: Playback starts in seconds instead of after the full download.
*   **Cons**: Needs piece-priority control for torrents and a very different
    Usenet path; complicates eviction and verification.
*   **Reason for Rejection**: Everything downloads fully before being
    served. Streaming while downloading is stage 14, only when needed.

### Alternative M: A JavaScript single-page web UI

*   **Pros**: A richer interface.
*   **Cons**: Needs a Node build toolchain that nothing in this project's
    workflow runs or checks.
*   **Reason for Rejection**: Server-rendered Go templates embedded in the
    `api` binary, growing from the existing embedded test page, with at
    most one vendored htmx file.

### Alternative N: Evict RSS-added items first

*   **Pros**: Items nobody asked for by hand would leave the cache first.
*   **Cons**: A second eviction rule beside least-recently-used, plus a
    per-feed byte cap and backlog table to keep it fair.
*   **Reason for Rejection**: One global LRU rule (Non-Goals). An RSS item
    nobody opens is never touched, so it is already among the first to go.

### Alternative O: Expiring job leases

*   **Pros**: Lets several workers share one fetcher and survive a worker
    that dies silently.
*   **Cons**: Adds `lease_until`, renewal on every poll, a reaper, a
    duration setting and a stale-lease test, for a design that runs one
    container per fetcher.
*   **Reason for Rejection**: Startup recovery, the finish order in
    Technical Plan section 6 and status-guarded writes cover every crash.
    The atomic `SKIP LOCKED` claim stays, so two claimers still get
    disjoint sets.

--------------------------------------------------------------------------------

## Non-Functional Requirements

*   **Secrets.** Only `ingest` receives provider credentials, and only
    `api` and `stream` receive the link-signing key. Credentials are stored
    as SHA-256 hashes; the plain key is shown once. Users' rclone remote
    configs (stage 13) are encrypted at rest with a key held only by
    `scheduler`.
*   **Outbound requests chosen by users.** RSS feeds (stage 10) and any
    other user-supplied URL go through one guarded HTTP client that refuses
    private, loopback and link-local addresses, including after redirects.
*   **Disk.** Best effort. Eviction keeps ready plus known active bytes
    under `CACHE_MAX_BYTES`, and ingest stops claiming while free space is
    below `CACHE_MIN_FREE_BYTES`. A magnet whose size turns out larger than
    the free space can still fill the disk until it fails; stage 9 adds
    reservations for Usenet.
*   **Bandwidth.** Watched through the VPS provider's monthly traffic
    dashboard. Per-user bytes served are not recorded.
*   **Rollout and rollback.** Images are tagged with the git commit. Each
    deploy takes a `pg_dump` first. Rollback is restoring that dump and
    running the previous tag; there are no down-migrations.

## Risks & Limits

Numbers assumed for planning until the VPS is chosen in stage 2: **disk
1 TB**, **cache limit 800 GB** (`CACHE_MAX_BYTES`, with
`CACHE_MIN_FREE_BYTES` 20 GB), **monthly traffic 20 TB**. Locally the cache
lives on the Mac's Docker Desktop disk.

| Risk | Mitigation |
|---|---|
| VPS provider suspends the server over torrent traffic or abuse reports | Legal test torrents only until stage 2 passes; read the provider's terms before choosing it |
| RD/TorBox/hoster accounts banned or out of quota for use from a datacenter or by others | Owner-only imports and per-user opt-in; check provider terms before stage 5 |
| Fakes built from docs don't match the real service | Each fake is marked unverified; the first deploy of each source is checked against the real one |
| PAR2 repair or `7zz` uses all CPU for hours | Separate `ingest` container with CPU and memory limits (stage 9) |
| A large magnet fills the disk before its size is known | `statfs` backstop, size check after metadata, `CACHE_MIN_FREE_BYTES` |
| Single node, single disk, no cache backup | Cache is rebuildable; only the database is backed up (`pg_dump` per deploy) |
| Signing key leaks | Rotate the key; all live links end at once (six-hour links) |
| qBittorrent's open port and Web UI | Web UI bound to the internal network only; port forwarded for peers only |

--------------------------------------------------------------------------------

### Additional Resources

*   `concepts_brief.md` — brainstorm brief with both review rounds.
*   `intermediate_zoom_out_report.md` — the review whose must-fix list this
    revision applies.
*   `ARCHITECTURE.md`, `README.md` — current (pre-rewrite) design.
*   `~/reports/Debrid service competitive teardown.md` — earlier research
    on how debrid services and their client tools work.

### Future Improvements

*   **Pins:** per-user GB of items that eviction skips.
*   **Streaming while downloading,** including Usenet streaming.
*   **Matching the same release from different sources** by content.
*   **Per-user bytes served:** one upsert when a response closes, if the
    monthly traffic ever needs splitting by user.
*   **More than one server:** would need leases and shared storage.

--------------------------------------------------------------------------------

## Detailed Implementation Plan

Paths are relative to the repository root. Each stage ends with
`go build ./...`, `go vet ./...` and `go test ./...` passing locally (with
`TEST_DATABASE_URL` set) and in CI; from stage 2 on, each stage also ends
with a deploy and a passing smoke test.

Entries for stages 4, 7, 10, 11 and 13 are provisional; each stage gets a
short design at its start.

### Stage 0: Test setup

#### 1. `internal/dbtest/dbtest.go` (Create)

*   **Type / Scope**: Go test helper package.
*   **Proposed Changes**: The `testRepo` helper from
    `internal/db/pg_test.go`, moved into its own package so every package's
    database tests can use it. `New(t *testing.T) *pgxpool.Pool` reads
    `TEST_DATABASE_URL`; if unset, `t.Skip` locally, `t.Fatal` when
    `CI=true`. Creates a schema `t_<random>`, sets `search_path` on the
    pool, runs `db.Migrate`, and drops the schema in `t.Cleanup`.

#### 2. `internal/db/migrate_test.go` (Create)

*   **Type / Scope**: Database test.
*   **Proposed Changes**: Runs `Migrate` twice on a fresh schema and checks
    the second run changes nothing; checks a database that recorded an
    unknown migration name fails to start.

#### 3. `internal/fakes/qbit/server.go` (Create)

*   **Type / Scope**: Fake upstream for tests.
*   **Proposed Changes**: An `httptest` server implementing only the
    qBittorrent Web API endpoints the client uses (login, add, info, files,
    delete, recheck). Torrents move through metadata → downloading → done
    on commands from the test, and can be "lost". Header comment: "built
    from the docs, not yet checked against a real qBittorrent".

#### 4. `internal/qbit/client_test.go` (Modify)

*   **Type / Scope**: Unit test.
*   **Proposed Changes**: Switch from hand-written responses to the shared
    fake server, so the same fake is used by ingest tests.

#### 5. `scripts/smoke.sh` (Create)

*   **Type / Scope**: Shell script, run against the server or the local
    stack.
*   **Proposed Changes**: Takes the base URL and two users' API keys.
    Checks a named legal test torrent with `/v1/items/check`, adds it as
    user A, waits for ready, adds it as user B and checks it returns
    `ready` at once with no second download, fetches a byte range and
    checks 206, blocks a second test item and checks 451, restarts all
    containers and checks health. With `-evict`, adds two small legal
    torrents against a `CACHE_MAX_BYTES` that fits only one and checks the
    older one becomes `evicted` and its file IDs survive a re-add. Exits 0
    or 1 and prints which step failed.

#### 6. `.github/workflows/test.yml` (Modify)

*   **Type / Scope**: CI config.
*   **Proposed Changes**: Add `shellcheck scripts/*.sh deploy/*.sh`. No
    other change; `CI=true` is already set by GitHub.

### Stage 1: Source-generic core, torrents only

Built as five commits, each passing CI on its own:

1.  Schema and state table: entries 7–13.
2.  Source interface, source keys and store: entries 14–20.
3.  Ingest: entries 21–24, 28.
4.  Service layer, API, web page and stream: entries 25–27, 29–33, 35.
5.  Programs, config, build and deploy files, docs: entries 34, 36–41.

#### 7. `internal/db/migrations/0001_init.sql` (Keep)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: None. It stays so databases that ran it convert
    forward through entry 8.

#### 8. `internal/db/migrations/0002_items.sql` (Create)

*   **Type / Scope**: Schema and data conversion.
*   **Proposed Changes**: Converts the torrent tables (see Technical Plan
    section 2) and creates the stage 1 tables:
    `users`, `credentials`, `user_limits`, `items` (with `fetcher` and
    `handle bytea`), `item_sources`, `item_files`, `user_items`,
    `fetch_attempts` (`account_id` nullable, no foreign key yet),
    `blocklist`. `CHECK` constraints for `status`, `phase` and file
    `state`. Indexes: items by `(status, fetcher, priority, created_at)`
    for claiming, by `last_used_at` where ready for eviction,
    `user_items (item_id)`, `item_sources (item_id)`.

#### 9. `internal/db/db.go` (Modify)

*   **Type / Scope**: Database access.
*   **Proposed Changes**: `Migrate` refuses to run if the database recorded
    a migration file this build doesn't have (an older build against a
    newer database). Add `Listen(ctx, channels...) (<-chan string, error)`
    using a dedicated connection that reconnects on error, and
    `Notify(ctx, tx, channel, payload)`.

#### 10. `internal/db/models.go` (Modify)

*   **Type / Scope**: Go types.
*   **Proposed Changes**: Replace torrent types with `Item`, `ItemSource`,
    `ItemFile`, `UserItem`, `Credential`, `UserLimits`, `FetchAttempt`,
    `BlockEntry`; status and phase as typed string constants shared with
    `internal/state`.

#### 11. `internal/db/repo.go` (Delete) and new query files (Create)

*   **Type / Scope**: Database access.
*   **Proposed Changes**: The torrent-shaped repository is replaced by one
    file per concern, all methods taking a `pgx.Tx` or the pool:
    *   `internal/db/locks.go`: `LockUser(tx, id)` and
        `LockKeys(tx, keys...)` (sorted, hashed, `pg_advisory_xact_lock`),
        the lock order kept from today's code.
    *   `internal/db/items.go`: find item by source key, insert item and
        source, `Enqueue` (only from new, `failed` or `evicted`, never from
        `blocked`; priority with `GREATEST`), guarded `SetProgress`,
        `Switch`, `Touch` (throttled `last_used_at`).
    *   `internal/db/claims.go`: `Claim(fetcher, label, n)`,
        `RequeueActive(fetcher)` for startup recovery, `Finalizing`,
        `MarkReady` (sends `torza_item_ready`); every write guarded by
        status and fetcher.
    *   `internal/db/files.go`: `UpsertFiles(itemID, manifest)` as
        `missing`, marking absent paths `missing`; `MarkPresent`;
        `MarkAllMissing(itemID)`.
    *   `internal/db/library.go`: add/remove/list `user_items`; library
        count and bytes.
    *   `internal/db/credentials.go`: create, find by hash and kind, revoke,
        touch `last_used_at`.
    *   `internal/db/limits.go`: get/set `user_limits`; count a user's
        active downloads.
    *   `internal/db/blocklist.go`: block by `(kind, key)` and item; check
        any of an item's keys.
    *   `internal/db/usage.go`: ready bytes plus known `active` bytes.

#### 12. `internal/db/repo_test.go` (Delete) and `internal/db/*_test.go` (Create)

*   **Type / Scope**: Database tests using `dbtest`.
*   **Proposed Changes**: Port the three tests in `pg_test.go` to the new
    schema. Add: two claimers get disjoint sets; a write after the item was
    blocked or switched matches no row; `RequeueActive` keeps the handle;
    file IDs survive ready → evicted → ready; a path missing from the
    second manifest is marked `missing`; `Enqueue` never leaves `blocked`.
    Block vs add is deterministic: hold an Add transaction, run Block on a
    second connection, assert it waits, commit, assert `blocked`; repeat
    with the order swapped.

#### 13. `internal/state/state.go` and `state_test.go` (Create)

*   **Type / Scope**: Pure Go, unit tested.
*   **Proposed Changes**: The status and phase constants, the event list
    (`Claimed`, `Switched`, `Recovered`, `Finalizing`, `Published`,
    `Lost`, `Failed`, `Evicted`, `Enqueued`, `Blocked`), and
    `Next(status, event)` backed by the table in Technical Plan section 3.
    The test walks every (status, event) pair.

#### 14. `internal/sourcekey/sourcekey.go` and test (Create)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: `type Key struct{ Kind, Value string }` and
    `Parse(input) (Key, Payload, error)`, which detects a magnet, bare
    hash or `.torrent` upload (stage 1), an NZB (stage 3) or a hoster URL
    (stage 6) and returns its key.

#### 15. `internal/magnet/magnet.go` and test (Modify)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: Add parsing of `.torrent` metainfo using an
    existing Go bencode package (for example `github.com/jackpal/bencode-go`;
    confirm it is maintained when adding it), taking the SHA-1 of the raw
    info dictionary bytes. Accept v1 and hybrid inputs by their v1 hash;
    reject v2-only magnets (`btmh` without `btih`) and v2-only metainfo
    with an error the API maps to 400.

#### 16. `internal/source/source.go` (Create)

*   **Type / Scope**: Interface package.
*   **Proposed Changes**: The `Source` interface, `Job`, `Handle`, `Report`,
    `Manifest`, `ErrClass` from Technical Plan section 4, and a registry
    keyed by fetcher name.

#### 17. `internal/source/sourcetest/contract.go` (Create)

*   **Type / Scope**: Shared test suite.
*   **Proposed Changes**: `Run(t, newSource)` checks the four rules for any
    implementation: Poll resumes after a new instance is created from the
    stored Handle; only the Finalize manifest is final and holds only
    regular files; Release and Cancel twice succeed; error classes are set.
    Every source's test calls it.

#### 18. `internal/source/qbitsource/source.go` and test (Create)

*   **Type / Scope**: Source implementation.
*   **Proposed Changes**: Wraps `internal/qbit`. `Kind()` returns
    `qbittorrent`. Handle holds the info hash, magnet or metainfo, and save
    path. Maps qBittorrent states to phases. Source rule 1: a torrent
    qBittorrent has lost is re-added from the Handle and rechecked against
    the data on disk. `Finalize` lists files through `store.Manifest`;
    `Release` removes the torrent with `deleteFiles=false`. A startup
    method removes torrents in Torza's category that have no `active` item.
    Test runs the contract suite against the fake qBittorrent.

#### 19. `internal/source/fakesource/source.go` and test (Create)

*   **Type / Scope**: Test-only source.
*   **Proposed Changes**: Two-step fake: reports `waiting_remote`, then
    `downloading`, then changes its file list at `Finalize`; keeps state
    only in the Handle and `incoming/`, so it resumes after a restart; can
    be told to be slow or to fail with each error class. Registered under a
    second fetcher name so the `Switched` path is tested in stage 1.

#### 20. `internal/store/store.go` and test (Modify)

*   **Type / Scope**: Storage.
*   **Proposed Changes**: `store.Local` keyed by item ID with the methods in
    Technical Plan section 8. `Open` goes through `os.Root` and keeps
    `filepath.IsLocal`. `Manifest` walks `incoming/<id>` and returns an
    error on any non-regular file. `Publish` never deletes an existing
    destination: if `store/<id>` exists and `incoming/<id>` doesn't, return
    success. Test includes a symlink pointing outside the folder.

#### 21. `internal/ingest/worker.go` and test (Modify)

*   **Type / Scope**: Download loop.
*   **Proposed Changes**: One loop per fetcher in `-fetchers`, each waking
    on `torza_queued_<fetcher>` or its timer. On start: startup recovery
    (entry 24). Each tick: skip claiming if `statfs` free space is below
    the minimum, claim up to N items, start or poll each through the
    registry, write guarded progress, apply `policy.go`, and record
    `fetch_attempts`. On a guard failure: `Cancel(deleteData=true)`,
    `Store.Remove`, stop. Lazy connection to qBittorrent: a missing
    qBittorrent marks its loop unhealthy instead of stopping the program.

#### 22. `internal/ingest/finalize.go` and test (Create)

*   **Type / Scope**: Download completion.
*   **Proposed Changes**: The five-step finish order and the recovery rules
    from Technical Plan section 6. Tests cover the three disk cases (only
    `store/<id>`, only `incoming/<id>`, neither) and check that no case
    calls `Start` or deletes finished files.

#### 23. `internal/ingest/policy.go` and test (Create)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: The decision table from Technical Plan section 4,
    including `METADATA_TIMEOUT`, `rd_fallback_after` and the
    `max_item_bytes` check. Table-driven test, one case per row.

#### 24. `internal/ingest/recover.go` and test (Create)

*   **Type / Scope**: Startup recovery.
*   **Proposed Changes**: Before a loop's first claim: `RequeueActive` for
    its fetcher, then finishing recovery for `finalizing` items. Test kills
    a worker mid-`active` (fake source half done) and checks the next
    start resumes from the same Handle.

#### 25. `internal/service/` (Create)

*   **Type / Scope**: Shared business rules.
*   **Proposed Changes**: Files `service.go` (constructor with db pool,
    store, signer), `add.go`, `enqueue.go`, `check.go`, `library.go`,
    `links.go`, `account.go`, `block.go`, `auth.go`, `limits.go`, each with
    a database test. Behaviour as in Technical Plan section 7. Acceptance
    tests: two users adding the same new key at once start one download and
    both libraries hold it; one user sending more adds than `max_active` at
    once gets exactly `max_active` downloads; adding a `ready` item returns
    its files in the same call; `Check` of 100 mixed inputs returns the
    right status for each and creates no rows; a blocked key reports
    `unavailable`.

#### 26. `internal/scheduler/evict.go` and test (Modify)

*   **Type / Scope**: Eviction job.
*   **Proposed Changes**: Used bytes are ready plus known `active` bytes;
    skip items used within `EVICT_IDLE_FOR`; mark files `missing` instead
    of deleting rows; take an advisory lock so only one copy runs.

#### 27. `internal/scheduler/purge.go` and `sweep.go`, with tests (Create)

*   **Type / Scope**: Takedown and cleanup jobs.
*   **Proposed Changes**: `purge.go` deletes files of `blocked` items with
    no handle and sets `purged_at` (items with a handle are purged by
    `ingest`). `sweep.go` deletes `store/<id>` and `incoming/<id>` folders
    whose item is not `active`, `finalizing` or `ready`.

#### 28. `internal/jobs/jobs.go` (Delete)

*   **Type / Scope**: NATS wiring.
*   **Proposed Changes**: Removed; wake-ups use `db.Listen`/`db.Notify`.

#### 29. `internal/auth/keys.go` and test (Modify)

*   **Type / Scope**: Credentials.
*   **Proposed Changes**: `NewKey(kind)` with a kind prefix (`tza_api_` in
    stage 1), so a key's kind is visible and a key of one kind can't be
    used as another.

#### 30. `internal/links/links.go` and test (Modify)

*   **Type / Scope**: Signed links.
*   **Proposed Changes**: Token keeps the `v1.` format and carries user ID,
    stable file ID and expiry; only the current key verifies; path `/d/`.

#### 31. `internal/api/server.go` and test (Modify)

*   **Type / Scope**: REST API.
*   **Proposed Changes**: Routes move from `/v1/torrents/{infohash}` to
    the table below. All handlers call `internal/service`. `api` and
    `stream` errors use these status codes: 400 bad input (including
    v2-only magnets), 429 over `max_active`, 451 blocked, 410 expired link.

    | Method | Path | Does |
    |---|---|---|
    | `GET` | `/v1/me` | Email, `max_active`, `max_item_bytes`, active count, library item count and bytes |
    | `POST` | `/v1/items/check` | Up to 100 magnets/hashes: `ready` (with files), `downloading`, `unknown` or `unavailable` each; starts nothing |
    | `POST` | `/v1/items` | Add a magnet, hash or `.torrent` upload; `ready` with files if cached, else `queued`; 400, 429, 451 |
    | `GET` | `/v1/items` | Your library |
    | `GET` | `/v1/items/{id}` | One item, with its files once ready |
    | `DELETE` | `/v1/items/{id}` | Remove from your library |
    | `POST` | `/v1/items/{id}/links` | Signed links for every file of a ready item |
    | `POST` | `/v1/files/{id}/link` | One signed link: `{"url", "expires_at"}` |
    | `POST` | `/v1/admin/block` | Admins: takedown by magnet, hash or item ID |
    | `PUT` | `/v1/admin/limits/{user id}` | Admins: set a user's limits |

#### 32. `internal/api/web.go` and `internal/api/web/` (Modify)

*   **Type / Scope**: Embedded test page.
*   **Proposed Changes**: Point the page at the `/v1/items` routes. After a
    magnet is pasted, call `/v1/items/check` and show "Cached — instant" or
    "Will download" before the user adds it; add a "links for all files"
    button. Keep the strict CSP.

#### 33. `internal/stream/server.go` and test (Modify)

*   **Type / Scope**: File serving.
*   **Proposed Changes**: Check access through the service layer (blocklist
    by every source key; 451); open files through `Store.Open`; call the
    throttled use-touch only after `USE_TOUCH_BYTES` written in a response,
    never on HEAD.

#### 34. `internal/config/config.go` and test (Modify)

*   **Type / Scope**: Configuration.
*   **Proposed Changes**: Two structs: `HTTPConfig` (`api`, `stream`) and
    `WorkerConfig` (`ingest`, `scheduler`), each loading only its own
    variables. New names: `USE_TOUCH_BYTES`; `EVICT_IDLE_FOR` default 4 h;
    the `-fetchers` flag replaces any `INGEST_SOURCES`. `METADATA_TIMEOUT`
    and `POLL_INTERVAL` stay.

#### 35. `internal/metrics/metrics.go` and test (Create)

*   **Type / Scope**: Observability.
*   **Proposed Changes**: A plain-text `/metrics` handler: queue depth per
    fetcher, oldest active item age, used and free bytes, evicted bytes.
    Readiness handler that pings Postgres.

#### 36. `cmd/{api,ingest,stream,scheduler}/main.go` (Modify)

*   **Type / Scope**: Program entry points.
*   **Proposed Changes**: `api migrate` runs migrations and exits;
    `api create-user` also writes an `api` credential row. The four
    programs stop running migrations, load their config struct, remove
    NATS, wire `db.Listen`, register sources and parse `-fetchers`
    (ingest), and mount `/metrics` and `/readyz`.

#### 37. `go.mod`, `go.sum` (Modify)

*   **Type / Scope**: Dependencies.
*   **Proposed Changes**: Remove `github.com/nats-io/nats.go` and its
    indirect dependencies; add the bencode package from entry 15.

#### 38. `Dockerfile` (Modify)

*   **Type / Scope**: Build.
*   **Proposed Changes**: Build the four programs into one image; tag passed
    in as the git commit.

#### 39. `deploy/docker-compose.yml`, `docker-compose.local.yml`, `.env.example`, `Caddyfile`, `local.sh` (Modify)

*   **Type / Scope**: Deployment, server and local.
*   **Proposed Changes**: Remove `nats`. Add a one-shot `migrate` service
    (`api migrate`) that the others depend on. One environment block per
    config struct. `ingest` runs `-fetchers=qbittorrent`. Image tag
    `torza:${TORZA_TAG}`. Pass `EVICT_IDLE_FOR` and `USE_TOUCH_BYTES`.
    Caddy routes `/d/*` to `stream`. `local.sh` keeps its commands.

#### 40. `scripts/deploy.sh` and `scripts/backup.sh` (Create)

*   **Type / Scope**: Operations.
*   **Proposed Changes**: `backup.sh` runs `pg_dump` into a dated file.
    `deploy.sh` runs the backup, pulls or builds the image for a commit,
    runs `api migrate`, restarts services and runs `smoke.sh`.

#### 41. `ARCHITECTURE.md` and `README.md` (Modify)

*   **Type / Scope**: Documentation.
*   **Proposed Changes**: Rewritten to match this design: items, source
    keys and fetchers, the source interface, finish order and recovery,
    service layer, the stage 1 route table, no NATS, no RD-compatible API.

### Stage 2: First server deploy

No new files. Run `scripts/deploy.sh` on the VPS, then `scripts/smoke.sh`.
The stage is done when the smoke test passes twice in a row, including the
restart step, and `smoke.sh -evict` passes with a small `CACHE_MAX_BYTES`
and two small legal torrents.

### Stage 3: Usenet spike

#### 42. `internal/nzb/nzb.go` and test (Create)

*   **Type / Scope**: Parser.
*   **Proposed Changes**: Parse NZB XML into files and segments; compute the
    key (SHA-256 of sorted message IDs); wired into `sourcekey`.

#### 43. `internal/nntp/conn.go`, `pool.go` and tests (Create)

*   **Type / Scope**: Protocol client.
*   **Proposed Changes**: NNTP over TLS: connect, `AUTHINFO USER/PASS`,
    `BODY <message-id>`, dot-unstuffing; a pool with a per-provider
    connection limit.

#### 44. `internal/yenc/yenc.go` and test (Create)

*   **Type / Scope**: Decoder.
*   **Proposed Changes**: yEnc decode with `=ybegin`/`=ypart`/`=yend`
    parsing and CRC32 check, including multi-part articles.

#### 45. `internal/fakes/nntp/server.go` (Create)

*   **Type / Scope**: Fake upstream.
*   **Proposed Changes**: In-process TLS NNTP server serving fixed articles,
    with options for missing and damaged articles.

#### 46. `internal/source/usenet/source.go` and test (Create)

*   **Type / Scope**: Source implementation (spike).
*   **Proposed Changes**: Downloads and decodes all segments into
    `incoming/<id>/`, checkpointing finished segments in a file so `Poll`
    resumes after a restart; fails on any damaged article (no repair yet);
    passes the contract suite. Not registered in production until stage 9.
    Also: a fuzz test for the NZB parser and yEnc decoder.

### Stage 4: Stremio library add-on (provisional; each stage gets a short design at its start)

#### 47. `internal/stremio/handler.go` and test (Create)

*   **Type / Scope**: Access method inside `api`.
*   **Proposed Changes**: `/stremio/<credential>/manifest.json`, catalog of
    the user's library, meta for `torza:<item id>`, streams that point at
    `/stremio/<credential>/play/<file id>`, which redirects to a fresh
    signed link or calls `service.Enqueue` for an evicted item. CORS
    headers. Authenticated by a `stremio` credential.

#### 48. `internal/api/server.go` (Modify)

*   **Type / Scope**: Routing.
*   **Proposed Changes**: Mount the Stremio handler; `POST
    /v1/credentials` to create a Stremio credential.

### Stage 5: HTTP downloader with Real-Debrid and TorBox

#### 49. `internal/httpdl/downloader.go` and test (Create)

*   **Type / Scope**: Shared downloader.
*   **Proposed Changes**: Downloads a URL into `incoming/<id>/<path>` with
    `Range` resume, a per-account connection limit and progress reporting.

#### 50. `internal/source/realdebrid/` and `internal/source/torbox/` (Create)

*   **Type / Scope**: Source implementations.
*   **Proposed Changes**: `client.go` for each API, `source.go` running the
    six steps, Handle holding the remote ID and per-file download state.
    Credentials come from `provider_accounts` via `secret_ref`.

#### 51. `internal/db/migrations/0002_providers.sql` and `internal/db/providers.go` (Create)

*   **Type / Scope**: Schema and queries.
*   **Proposed Changes**: `provider_accounts` (`id`, `provider`, `label`,
    `max_connections`, `daily_bytes`, `secret_ref`), and a foreign key from
    `fetch_attempts.account_id` to it; queries to list accounts and count
    their connections and bytes used today.

#### 52. `internal/fakes/realdebrid/server.go`, `internal/fakes/torbox/server.go` (Create)

*   **Type / Scope**: Fake upstreams.
*   **Proposed Changes**: Built from each service's API documentation,
    marked as not yet checked against real responses.

#### 53. `internal/ingest/policy.go` (Modify)

*   **Type / Scope**: Fetch policy.
*   **Proposed Changes**: The `rd_fallback_after` row now switches to a
    registered RD/TorBox fetcher. The result is accepted when it fills the
    same item key; a qBittorrent recheck of the imported data is optional.

#### 54. `internal/api/server.go` (Modify)

*   **Type / Scope**: Admin route.
*   **Proposed Changes**: `POST /v1/admin/import`: the owner switches an
    item to an RD/TorBox fetcher through `service.Enqueue`.

### Stage 6: File hosters

#### 55. `internal/source/hoster/source.go`, `hosts.go` and tests (Create)

*   **Type / Scope**: Source implementation.
*   **Proposed Changes**: Per-host URL parser (host + file ID) and
    download-link resolution using the owner's account; download through
    `httpdl`. Folder links become one item per file. Owner-only, enforced
    in `service.Add`.

#### 56. `internal/fakes/hoster/server.go` (Create)

*   **Type / Scope**: Fake upstream.
*   **Proposed Changes**: A fake for each supported host's documented flow.

### Stage 7: WebDAV (provisional; each stage gets a short design at its start)

#### 57. `internal/webdav/fs.go`, `handler.go` and tests (Create)

*   **Type / Scope**: Access method inside `stream`.
*   **Proposed Changes**: A read-only file system over the user's ready
    items using `golang.org/x/net/webdav`; app-password Basic auth with a
    `webdav` credential; opening a file never re-queues; reads use the same
    use-touch as `stream`, never on PROPFIND or HEAD.

#### 58. `deploy/Caddyfile`, `go.mod` (Modify)

*   **Type / Scope**: Routing, dependency.
*   **Proposed Changes**: Route `/dav/*` to `stream`; add
    `golang.org/x/net`.

### Stage 8: Web UI

#### 59. `internal/web/` and `internal/db/migrations/0003_invites.sql` (Create)

*   **Type / Scope**: Access method inside `api`.
*   **Proposed Changes**: Grows from the stage 1 page in `internal/api/web/`
    into server-rendered pages: `handlers.go` (add with the cached/download
    hint, library, item, credentials, owner limits and import pages),
    `session.go` (invite link or API key → HttpOnly `session` credential
    cookie), `csrf.go`, `templates/*.html` and `static/htmx.min.js`
    embedded with `embed.FS`, with handler tests. The migration adds
    `invites`.

### Stage 9: Full Usenet

#### 60. `internal/par2/` (Create)

*   **Type / Scope**: Library.
*   **Proposed Changes**: `parse.go` (packets), `verify.go` (block
    checksums), `repair.go` (Reed–Solomon over GF(2^16)), with tests on
    small generated sets with removed blocks and a fuzz test for `parse.go`.

#### 61. `internal/unpack/unpack.go` and test (Create)

*   **Type / Scope**: Tool wrapper.
*   **Proposed Changes**: Runs `7zz x` with a timeout and low priority in
    its own working folder, checks the exit code, lists the extracted files
    through `store.Manifest`, deletes archive volumes after success. The
    test uses a stub binary.

#### 62. `internal/source/usenet/source.go`, `internal/db/migrations/0004_reservations.sql` (Modify, Create)

*   **Type / Scope**: Source implementation and disk reservations.
*   **Proposed Changes**: Add verify → repair → unpack phases; register in
    production. Add `items.reserved_bytes` with a 2.2× multiplier for
    Usenet (RAR, PAR2 and unpacked files exist at once) and one advisory
    lock around claiming that checks reservations.

#### 63. `Dockerfile`, `deploy/docker-compose.yml` (Modify)

*   **Type / Scope**: Deployment.
*   **Proposed Changes**: A `usenet` build target that adds `7zz`; an
    `ingest-usenet` service with `-fetchers=usenet` and CPU/memory limits.

### Stage 10: Release-name parser and RSS (provisional; each stage gets a short design at its start)

#### 64. `internal/release/parse.go` and test (Create)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: Parse resolution, release type, codec, HDR,
    group, title, season and episode from a release name; table tests.

#### 65. `internal/netguard/client.go` and test (Create)

*   **Type / Scope**: Guarded HTTP client.
*   **Proposed Changes**: Dials public addresses only (checked after DNS,
    on every redirect), with size and time limits.

#### 66. `internal/rss/` (Create)

*   **Type / Scope**: Background job in `scheduler`.
*   **Proposed Changes**: `fetch.go` (through `netguard`, including
    `.torrent` enclosures), `filter.go` (uses `release`), `job.go` (poll
    due feeds, skip seen entries, add through `service.Add` with `via=rss`
    at priority 0; an entry refused for limits stays unseen and is retried
    next poll), with tests.

#### 67. `internal/db/migrations/0005_rss.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: `feeds`, `feed_entries_seen`.

#### 68. `internal/api/server.go`, `internal/web/` (Modify)

*   **Type / Scope**: Access.
*   **Proposed Changes**: Routes and pages to manage feeds.

### Stage 11: Metadata catalog (provisional; each stage gets a short design at its start)

#### 69. `internal/catalog/` (Create)

*   **Type / Scope**: Background job in `scheduler`.
*   **Proposed Changes**: `client.go` for the metadata API, `match.go`
    (release parser → search → best match), `job.go` (enrich new ready
    items), with tests against a fake.

#### 70. `internal/db/migrations/0006_catalog.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: `item_titles` (item or file, title, year, season,
    episode, poster, external IDs, `manual` flag).

#### 71. `internal/stremio/handler.go`, `internal/web/` (Modify)

*   **Type / Scope**: Access.
*   **Proposed Changes**: Show titles and posters; owner/user can correct a
    match. Stremio IDs stay `torza:<item id>`.

### Stage 12: Telegram bot

#### 72. `internal/telegram/` (Create)

*   **Type / Scope**: Access method inside `api`.
*   **Proposed Changes**: `webhook.go` (route `/telegram/<secret>`, checks
    the secret-token header), `link.go` (one-time code, stored as an
    `invites` row, links a chat to a user as a `telegram` credential),
    `commands.go` (add, check, list, links), `notify.go` (listens on
    `torza_item_ready`, sent since stage 1, and messages the users who
    have the item), with tests.

#### 73. `deploy/Caddyfile`, `internal/config/config.go` (Modify)

*   **Type / Scope**: Routing, config.
*   **Proposed Changes**: Route `/telegram/*` to `api`; add
    `TELEGRAM_BOT_TOKEN` and `TELEGRAM_WEBHOOK_SECRET` to `HTTPConfig`.

### Stage 13: Export to the user's own storage (provisional; each stage gets a short design at its start)

#### 74. `internal/export/` (Create)

*   **Type / Scope**: Background job in `scheduler`.
*   **Proposed Changes**: `job.go` (claims export requests, counts as an
    active slot, use-touches the item while copying so eviction keeps it),
    `rclone.go` (runs `rclone copy` with a per-job config file written from
    the decrypted remote and deleted afterwards), with tests using a stub
    binary.

#### 75. `internal/db/migrations/0007_export.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: `user_remotes` (encrypted config), `exports`
    (item, remote, status, bytes, error).

#### 76. `Dockerfile`, `internal/web/`, `internal/api/server.go` (Modify)

*   **Type / Scope**: Build, access.
*   **Proposed Changes**: Add `rclone` to the scheduler image; pages and
    routes to add a remote and export an item.

### Stage 14: Only when needed

No files planned. Usenet streaming and cross-source matching get their own
design when a need appears.

### Verification Plan

*   **Compilation/Build**: `go build ./...`
*   **Unit Tests**: `go test -race ./...` (database tests run when
    `TEST_DATABASE_URL` is set, locally against a throwaway Postgres, and
    always in CI)
*   **Static Analysis & Linting**: `go vet ./...`; `shellcheck` on
    `scripts/*.sh` and `deploy/*.sh` in CI
*   **Test Coverage Requirement**: every row of the state and policy
    tables and every stage 1 acceptance test listed in entries 12, 13, 19,
    22, 23, 24 and 25 must exist and pass; no percentage target
*   **E2E/Integration**: `scripts/smoke.sh` against the local stack before
    each deploy and on the server after every deploy from stage 2 on;
    each source passes the contract suite against its fake before deploy
