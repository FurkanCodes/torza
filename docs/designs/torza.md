# Torza: a personal multi-source debrid service

*   **Author:** Furkan
*   **Status:** Proposed
*   **Date:** 2026-11-13

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
torrent-only first version (commit `d658d24`). It has never run against real
services, and two design reviews found problems that make it a poor base for
more sources:

*   **The torrent info hash is used for everything.** It is the item's
    database key, its folder on disk, the takedown key and part of the API
    routes. Usenet and hoster content has no info hash.
*   **File IDs change every time an item is downloaded again.** Stremio,
    WebDAV clients and saved links all break after an evicted item comes
    back.
*   **Four confirmed bugs, all in database steps that no test runs:** a
    takedown race, a duplicate-add race, an unsafe order of steps when a
    download finishes, and no way for a worker to claim a job, so two workers
    would download the same thing.
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
    runs against a real Postgres database in CI.
*   **Crash safety.** Killing any Torza process at any moment, then
    restarting it, leaves no item stuck, downloaded twice, or deleted after
    finishing.
*   **One source interface** that torrents, two-step remote services
    (Real-Debrid, TorBox, hosters) and Usenet all fit. Stage 1 proves this
    with qBittorrent plus a fake two-step test source.
*   **Takedowns work across sources.** A blocked item can't be downloaded
    again or served through any source key or access method.
*   **Safe eviction.** When the disk fills, least-recently-used items go
    first. An item someone is actively reading is never evicted.
*   **A first deploy on a cheap VPS** passes a scripted smoke test.

**Sources (stages 3, 5, 6, 9)**

*   Real-Debrid and TorBox imports that land in the same item a qBittorrent
    download of that torrent would.
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

*   Nothing runs on the owner's Mac except `go build`, `go vet` and
    `go test`. Database tests run on GitHub Actions. Everything else runs on
    the server.
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
*   A Real-Debrid-compatible API.
*   Streaming while downloading, including Usenet streaming (stage 14, only
    when needed).
*   Recognising the same release arriving from two different sources
    (stage 14, only when needed).
*   Storing the shared cache on cloud storage. The cache stays on the
    server's local disk; cloud storage is only an export target.
*   Per-user shares of the disk or per-user eviction. Eviction is one global
    rule.
*   Running more than one server. Stage 1 runs on one node; job leases are
    for surviving crashes, not for scaling out.

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
    qBittorrent, using none of the owner's RD quota.

### CUJ 6: Watching through WebDAV while a media server scans

*   **Given**: A media server has the WebDAV share mounted, and some items
    in the library are evicted.
*   **When**: The media server scans every file.
*   **Then**: Only ready items are listed. Nothing is queued again, and
    opening a file without reading real data doesn't count as use for
    eviction.

### CUJ 7: An RSS feed matches a new release

*   **Given**: User E has an RSS feed with a 1080p WEB-DL filter.
*   **When**: A matching entry appears.
*   **Then**: It is added at the lowest priority, within user E's limits.
    It is never fetched again after being evicted unless someone asks for
    it. A feed URL pointing at an internal address is refused.

## Technical Plan (Architecture)

### 1. Programs and containers

Torza stays one Go codebase built into a few programs, each in its own
container, sharing one Postgres database. Stage 1 keeps the four existing
programs; later stages add packages to them, not new programs, except where
noted.

| Program | Holds | Added in stage |
|---|---|---|
| `api` | REST API, web UI (server-rendered), Stremio add-on, admin, Telegram webhook | REST now; Stremio 4, web UI 8, Telegram 12 |
| `stream` | Serves file bytes with signed links and byte ranges; WebDAV under `/dav/` | WebDAV 7 |
| `ingest` | Runs sources. One loop per source kind; `-sources=torrent,http,usenet` picks which kinds this copy runs | torrent 1, http 5–6, usenet 3/9 |
| `scheduler` | Background jobs: eviction, purging blocked content, RSS polling, catalog lookups, exports | RSS 10, catalog 11, export 13 |
| `migrate` | Runs database migrations once per deploy, then exits | 1 |

Supporting containers: Postgres 17, qBittorrent and Caddy (HTTPS). NATS is
removed: programs wake each other with Postgres `LISTEN`/`NOTIFY`
(channels `torza_queued_<kind>` and `torza_item_ready`), sent inside the
transaction that creates the work, so a wake-up never arrives before the
row is visible. Every loop also checks on a timer, so a missed
notification only delays work until the next check. On the server, Usenet ingest
runs as a second `ingest` container (`-sources=usenet`) with its own CPU and
memory limits, because PAR2 repair can use all CPU for hours.

```text
                   users (Stremio, players, browser, Telegram)
                                    │ HTTPS
                                 ┌──▼───┐
                                 │Caddy │
                                 └┬────┬┘
                  /v1, /ui, /stremio   /dl, /dav
                       ┌──────▼─┐    ┌─▼──────┐
                       │  api   │    │ stream │──reads──┐
                       └───┬────┘    └───┬────┘         │
                           │ internal/service (shared rules)
                           ▼             ▼              │
                     ┌─────────────────────────┐        │
                     │        Postgres         │        │
                     │ items, sources, files,  │        │
                     │ libraries, leases, ...  │        │
                     └──▲───────────────────▲──┘        │
        claim / report  │                   │ evict,    │
                  ┌─────┴──────┐      ┌─────┴─────┐ RSS │
                  │  ingest    │      │ scheduler │     │
                  │ torrent    │      └─────┬─────┘     │
                  │ http       │            │ deletes   │
                  │ usenet     │            ▼           │
                  └─┬───┬────┬─┘      ┌─────────────────▼──┐
       qBittorrent ◄┘   │    └─NNTP─► │ /data/store (local │
       RD / TorBox ◄─HTTP           ▲ │ disk, the cache)   │
       hosters    ◄─HTTP            │ └────────────────────┘
                    downloads land in /data/incoming, then are renamed in
```

Only `ingest` holds source credentials (qBittorrent, RD/TorBox, hosters,
Usenet providers). Only `api` and `stream` hold the link-signing key. Each
program gets its own config struct and its own environment block in compose.

### 2. Data model

The first migration is rewritten and given a new name
(`0001_items.sql`), so a database that ran the old `0001_init.sql` fails
loudly instead of skipping it. After the first deploy, migrations only
move forward and keep IDs.

| Table | Purpose and key columns |
|---|---|
| `users` | `id`, `email`, `is_admin`, `created_at` |
| `credentials` | One row per key a user holds: `id`, `user_id`, `kind` (`api`, `stremio`, `webdav`, `session`, `telegram`), `hash`, `prefix`, `created_at`, `last_used_at`, `revoked_at` |
| `user_limits` | `user_id`, `max_active`, `max_item_bytes`, `priority`, `rd_fallback_after` (null = off) |
| `items` | What is cached and evicted: `id`, `name`, `status`, `phase`, `size_bytes`, `progress`, `error`, `started_by`, `priority`, `source_kind` (kind of the current attempt), `claimed_by`, `lease_until`, `reserved_bytes`, `progress_updated_at`, `last_used_at`, `completed_at`, `purged_at`, timestamps |
| `item_sources` | How a request is matched to an item: unique `(kind, key)`, `item_id`, `payload` (magnet, NZB file reference, URL) |
| `item_files` | `id` (stable), `item_id`, `path`, `size_bytes`, `state` (`present`, `missing`); unique `(item_id, path)` |
| `user_items` | A user's library: `id`, `user_id`, `item_id`, `added_via` (`api`, `rss`, `telegram`, ...), `source_id`, `added_at`; unique `(user_id, item_id)` |
| `fetch_attempts` | One try by one fetcher: `item_id`, `fetcher` (`qbittorrent`, `realdebrid`, `torbox`, `hoster`, `usenet`), `account_id`, `started_at`, `ended_at`, `result`, `error_class` |
| `provider_accounts` | The owner's paid accounts: `id`, `provider`, `label`, `max_connections`, `daily_bytes`, `secret_ref` (name of the env/secret, never the secret itself) |
| `blocklist` | `(kind, key)` primary key, optional `item_id`, `reason`, `reference`, `blocked_by`, `created_at` |
| `readers` | Live reads for safe eviction: `item_id`, `reader_id`, `last_seen` |
| Later stages | `provider_accounts` (5); `feeds`, `feed_entries_seen` (10); `item_titles` (11); `exports`, `user_remotes` (13) |

Source keys, by kind:

*   **torrent**: lowercase info hash, taken from parsed metainfo (magnet or
    `.torrent` file), never from the raw text.
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
| `queued` | waiting for a worker | `active`, `blocked` |
| `active` | a worker holds the lease | `finalizing`, `queued` (lease lost, retry), `failed`, `blocked` |
| `finalizing` | files are being moved into the store | `ready`, `blocked` |
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
    Kind() string
    Start(ctx context.Context, job Job) (Handle, error)
    Poll(ctx context.Context, h Handle) (Report, error)
    Finalize(ctx context.Context, h Handle) (Manifest, error)
    Cancel(ctx context.Context, h Handle, deleteData bool) error
}

// Handle: opaque bytes the source needs to find its work again; stored on the item.
// Report: Phase, BytesDone, BytesTotal, EstimatedFiles, ErrClass, NextPollAfter.
// Manifest: the final list of files (path, size) under incoming/<item id>/.
// ErrClass: Retry, Permanent, TryAnotherFetcher.
```

Rules every source must follow:

1.  **Poll survives restarts.** After a crash, `Poll` rebuilds its state from
    the stored Handle and what is on disk under `incoming/<item id>/`.
2.  **File rows come only from `Finalize`.** Any earlier file list is an
    estimate, used for size checks only. This covers Usenet, where repair
    and unpacking produce different files from the NZB's list.
3.  **Cancel can be called twice** without error.
4.  **Two-step sources** (RD, TorBox, hosters) report `waiting_remote` while
    the remote side prepares, then do the HTTP download with resume inside
    the same implementation. The interface has no extra method for this.

Fetchers and their kinds: `qbittorrent` (torrent), `realdebrid` and `torbox`
(torrent, plus their own Usenet/web items), `hoster` (hoster), `usenet`
(nzb). The HTTP downloader is one shared package used by RD, TorBox and
hosters.

**Fetch policy.** For a torrent item, the order is qBittorrent first. If the
user who started it has `rd_fallback_after` set and no metadata has arrived
in that time, the worker cancels qBittorrent and starts an RD (or TorBox)
fetch for the same item. The owner can start an import for any item by
hand. Hoster and RD/TorBox sources can be added only by the owner. An import
selects all files; the item becomes ready only if its file set matches what
the torrent lists. Each try is a row in `fetch_attempts`.

### 5. Claiming work

Each source kind has its own loop in `ingest`. A loop claims work with one
statement:

```sql
UPDATE items SET status='active', claimed_by=$worker, lease_until=now()+$lease
WHERE id IN (
  SELECT id FROM items
  WHERE status='queued' AND source_kind=$kind AND reserved_fits(...)
  ORDER BY priority DESC, created_at
  FOR UPDATE SKIP LOCKED LIMIT $n)
RETURNING ...
```

Every `Poll` renews the lease. Every later write to the item includes
`WHERE claimed_by=$worker`, so a worker whose lease expired gets a conflict
and stops. Items whose lease expired go back to `queued` and keep their
Handle, so the next worker resumes rather than restarts. `items.priority` is the
highest `priority` among the users who have the item in their library; the
service layer's Add raises it when a higher-priority user adds the item.
`reserved_fits(...)` stands for the disk-space check in section 8.

### 6. Finishing a download

1.  Set `status=finalizing` (checks the lease).
2.  Call `Finalize` to get the manifest; the source releases its files (for
    qBittorrent: remove the torrent, keep the data).
3.  Rename `incoming/<id>` to `store/<id>`. Never delete an existing
    `store/<id>`: if it exists and `incoming/<id>` doesn't, the rename
    already happened.
4.  In one transaction: upsert `item_files` by `(item_id, path)` as
    `present`, mark paths not in the manifest `missing`, set `status=ready`,
    clear the lease and the disk reservation.

On every tick, a worker completes any `finalizing` item whose `store/<id>`
already exists, without calling the source again.

### 7. Shared service layer

`internal/service` is a Go package used in-process by `api`, `stream` and
`scheduler`. Access packages (REST, web UI, Stremio, WebDAV, Telegram, RSS)
call it and never use `internal/db` directly. It owns:

*   **Add**: parse the input into a source key; check the blocklist; find or
    create the item; add it to the user's library; if this starts a
    download, check the user's limits. All in one transaction, holding
    `pg_advisory_xact_lock(user_id)` while counting active downloads. The
    item upsert never overwrites a `blocked` item and only re-queues items
    that are `failed` or `evicted`.
*   **Library**: list, remove (removes the library row only), item detail.
*   **Play**: check the user has the item, it isn't blocked by any of its
    source keys, and it is ready; return a signed link. If it is evicted,
    re-queue it and report "downloading".
*   **Block**: upsert the item as `blocked` and add every source key of the
    item to the blocklist, in one transaction; the purge job deletes files.
*   **Authorize**: look up a credential of the expected kind, reject revoked
    ones, update `last_used_at`.
*   **Limits**: read and set `user_limits` (owner only).

### 8. Storage, disk space and eviction

`store.Store` becomes an interface:

```go
type Store interface {
    Open(itemID int64, path string) (io.ReadSeekCloser, int64, error)
    Publish(itemID int64) error      // rename incoming/<id> -> store/<id>
    Remove(itemID int64) error
    Usage() (used, free int64, err error)
}
```

The only implementation is local disk. `incoming/` and `store/` sit on the
same filesystem so the rename is atomic.

**Reserving space.** A claim reserves `estimated size × multiplier` bytes
(torrent and HTTP 1.0, Usenet 2.2, because RAR, PAR2 and the unpacked files
exist at once). A claim is refused if ready bytes plus reserved bytes would
pass the cache limit or leave less than the minimum free space.

**Eviction** (scheduler): when over the limit, delete the
least-recently-used `ready` items that have no live reader, mark their file
rows `missing`, set `status=evicted`. Library rows stay.

**What counts as use.** `last_used_at` is updated only after a reader has
received real data (50 MB or 2 minutes), never on open or HEAD. Readers
write a `readers` row with `last_seen` while they stream; rows older than
two minutes don't count.

### 9. Links and credentials

*   Signed links carry a key ID, user ID, file ID and expiry, with an HMAC
    signature. Only the current key is accepted; changing the key ends live
    links early, which is acceptable with six-hour links. The key ID leaves
    room to accept older keys later if that ever matters.
    `stream` checks the signature, then asks the service layer whether the
    user may still read the file.
*   Stremio and players get stable `/play/<file id>` URLs authenticated by
    their own credential; `/play` answers with a redirect to a fresh signed
    link, because signed links expire and Stremio saves URLs.
*   WebDAV uses an app password (`webdav` credential). The web UI exchanges
    an API key or an invite link for an HttpOnly session cookie with CSRF
    protection; there is no email login. Telegram links a chat to a user
    with a one-time code.

### 10. Later stages

Each later stage adds packages on the same core; the files are listed in
the Detailed Implementation Plan.

| Stage | Adds | Lives in |
|---|---|---|
| 3 | Usenet spike: NZB key, NNTP/TLS, yEnc, fake NNTP | `ingest` (not registered yet) |
| 4 | Stremio library add-on, IDs `torza:<item id>` | `api` |
| 5–6 | Shared HTTP downloader; RD/TorBox six-step sources; hosters | `ingest` |
| 7 | Read-only WebDAV over ready items | `stream` |
| 8 | Server-rendered web UI | `api` |
| 9 | PAR2 verify and repair in Go; unpacking with `7zz` | `ingest-usenet` |
| 10 | Release-name parser; RSS through a guarded HTTP client | `scheduler` |
| 11 | Metadata catalog, `item_titles` | `scheduler`, shown in `api` |
| 12 | Telegram webhook `/telegram/<secret>` | `api` |
| 13 | Export of ready items with `rclone copy` | `scheduler` |

### 11. Testing and deployment

*   **Unit tests** (run everywhere): state table, source contract tests
    against the fake source, link signing, parsers.
*   **Database tests** (CI only): a `dbtest` helper creates a separate
    schema per test and runs the migrations. Locally, without
    `TEST_DATABASE_URL`, these tests skip; in CI (`CI=true`) a missing URL
    fails the run.
*   **Fake upstreams:** fake qBittorrent in stage 0; fake NNTP in stage 3;
    fake RD, TorBox and hosters in stages 5–6.
*   **Smoke test:** a script run on the server after each deploy, using a
    named legal test torrent, that exits 0 or 1.
*   **Deploy:** images tagged with the git commit; `pg_dump` before each
    deploy; rollback means restoring the dump and running the previous tag.
*   **Health:** readiness checks that ping Postgres, and a small `/metrics` page: queue depth per kind, lease age,
    reserved bytes, evicted bytes, live readers.

## Alternatives Considered

### Alternative A: Keep the torrent schema and add migrations on top

*   **Pros**: No rewrite of the first migration; the existing 52 tests keep
    working unchanged.
*   **Cons**: The info hash stays the item's key, folder name, takedown key
    and route parameter. Every new source would need its own copy of these
    tables or a fake hash. File IDs would keep changing on re-download.
*   **Reason for Rejection**: Nothing is deployed, so a rewrite costs no data.
    Adding sources later on a torrent-shaped schema would mean rewriting the
    core queries at every stage.

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
*   **Reason for Rejection**: Dropped. Torza's own API, Stremio and WebDAV
    cover the planned uses.

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
*   **Reason for Rejection**: Claiming uses Postgres leases, and wake-ups
    use Postgres `LISTEN`/`NOTIFY`. NATS is removed: on one server it adds a
    container and a secret without adding anything Postgres can't do.

### Alternative L: Stream while downloading

*   **Pros**: Playback starts in seconds instead of after the full download.
*   **Cons**: Needs piece-priority control for torrents and a very different
    Usenet path; complicates eviction and verification.
*   **Reason for Rejection**: Everything downloads fully before being
    served. Streaming while downloading is stage 14, only when needed.

### Alternative M: A JavaScript single-page web UI

*   **Pros**: A richer interface.
*   **Cons**: Needs a Node build toolchain that nothing in this project's
    workflow runs or checks; only `go build`, `go vet` and `go test` run
    locally.
*   **Reason for Rejection**: Server-rendered Go templates embedded in the
    `api` binary, with at most one vendored htmx file.

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
*   **Disk.** A claim is refused if it can't reserve its space; Usenet
    reserves 2.2× its size. The cache never fills the disk past the
    configured minimum free space.
*   **Bandwidth.** `stream` records bytes served per user per day, shown on
    the metrics page, so the VPS's monthly allowance can be watched.
*   **Rollout and rollback.** Images are tagged with the git commit. Each
    deploy takes a `pg_dump` first. Rollback is restoring that dump and
    running the previous tag; there are no down-migrations.

### Additional Resources

*   `concepts_brief.md` — brainstorm brief with both review rounds.
*   `ARCHITECTURE.md`, `README.md` — current (pre-rewrite) design.
*   `~/reports/Debrid service competitive teardown.md` — earlier research
    on how debrid services and their client tools work.

### Future Improvements

*   **Pins:** per-user GB of items that eviction skips.
*   **Streaming while downloading,** including Usenet streaming.
*   **Matching the same release from different sources** by content.
*   **More than one server:** leases already allow it; storage would need
    to become shared.

--------------------------------------------------------------------------------

## Detailed Implementation Plan

Paths are relative to the repository root. Each stage ends with
`go build ./...`, `go vet ./...` and `go test ./...` passing locally and in
CI; from stage 2 on, each stage also ends with a deploy and a passing smoke
test.

### Stage 0: Test setup

#### 1. `internal/dbtest/dbtest.go` (Create)

*   **Type / Scope**: Go test helper package.
*   **Proposed Changes**: `New(t *testing.T) *pgxpool.Pool` reads
    `TEST_DATABASE_URL`. If unset: `t.Skip` locally, `t.Fatal` when
    `CI=true`. Creates a schema named after the test (`t_<random>`), sets
    `search_path` on the pool, runs `db.Migrate`, and drops the schema in
    `t.Cleanup`. Needed because `go test ./...` runs packages in parallel
    and `Migrate` takes one global advisory lock.

#### 2. `internal/db/migrate_test.go` (Create)

*   **Type / Scope**: Database test.
*   **Proposed Changes**: Runs `Migrate` twice on a fresh schema and checks
    the second run changes nothing; checks a database that recorded an
    unknown migration name fails to start. Proves the CI database path
    works end to end.

#### 3. `internal/fakes/qbit/server.go` (Create)

*   **Type / Scope**: Fake upstream for tests.
*   **Proposed Changes**: An `httptest` server implementing only the
    qBittorrent Web API endpoints the client uses (login, add, info, files,
    delete). Torrents move through metadata → downloading → done on
    commands from the test. Header comment: "built from the docs, not yet
    checked against a real qBittorrent".

#### 4. `internal/qbit/client_test.go` (Modify)

*   **Type / Scope**: Unit test.
*   **Proposed Changes**: Switch from hand-written responses to the shared
    fake server, so the same fake is used by ingest tests.

#### 5. `scripts/smoke.sh` (Create)

*   **Type / Scope**: Shell script, run on the server.
*   **Proposed Changes**: Takes the base URL and two users' API keys. Adds a
    named legal test torrent as user A, waits for ready, adds it as user B
    and checks no second download started, fetches a byte range and checks
    206, blocks a test item and checks 451, restarts all containers and
    checks health. Exits 0 or 1 and prints which step failed.

#### 6. `.github/workflows/test.yml` (Modify)

*   **Type / Scope**: CI config.
*   **Proposed Changes**: Add `shellcheck scripts/*.sh`. No other change;
    `CI=true` is already set by GitHub.

### Stage 1: Source-generic core, torrents only

Built as five commits, each passing CI on its own:

1.  Schema and state table: entries 7–13.
2.  Source interface, source keys and store: entries 14–21.
3.  Ingest: entries 22–24, 28.
4.  Service layer, API and stream: entries 25–27, 29–32, 34.
5.  Programs, config, build and deploy files, docs: entries 33, 35–40.

#### 7. `internal/db/migrations/0001_init.sql` (Delete)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: Removed; replaced by `0001_items.sql`.

#### 8. `internal/db/migrations/0001_items.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: All tables in Technical Plan section 2 except the
    later-stage ones: `users`, `credentials`, `user_limits`, `items`,
    `item_sources`, `item_files`, `user_items`, `fetch_attempts`
    (`account_id` nullable, no foreign key yet), `blocklist`, `readers`. `CHECK` constraints for
    `status`, `phase`, credential `kind` and file `state`. Indexes: items
    by `(status, source_kind, priority, created_at)` for claiming, by
    `last_used_at` where ready for eviction, `user_items (item_id)`,
    `item_sources (item_id)`.

#### 9. `internal/db/db.go` (Modify)

*   **Type / Scope**: Database access.
*   **Proposed Changes**: `Migrate` refuses to run if the database recorded
    a migration file that no longer exists (catches the old
    `0001_init.sql`). Add `Listen(ctx, channels...) (<-chan string, error)`
    using a dedicated connection that reconnects on error, and
    `Notify(ctx, tx, channel, payload)`.

#### 10. `internal/db/models.go` (Modify)

*   **Type / Scope**: Go types.
*   **Proposed Changes**: Replace torrent types with `Item`, `ItemSource`,
    `ItemFile`, `UserItem`, `Credential`, `UserLimits`, `FetchAttempt`,
    `BlockEntry`, `Reader` (`ProviderAccount` in stage 5); status and phase as typed
    string constants shared with `internal/state`.

#### 11. `internal/db/repo.go` (Delete) and new query files (Create)

*   **Type / Scope**: Database access.
*   **Proposed Changes**: The torrent-shaped repository is replaced by one
    file per concern, all methods taking a `pgx.Tx` or the pool:
    *   `internal/db/items.go`: find item by source key, insert item and
        source, `QueueAgain` (only from `failed` or `evicted`, never from
        `blocked`), raise priority, set progress.
    *   `internal/db/claims.go`: `Claim(kind, worker, n, lease)`,
        `RenewLease`, `ReleaseExpired`, every write guarded by
        `claimed_by`.
    *   `internal/db/files.go`: `UpsertFiles(itemID, manifest)` marking
        absent paths `missing`; `MarkAllMissing(itemID)`.
    *   `internal/db/library.go`: add/remove/list `user_items`.
    *   `internal/db/credentials.go`: create, find by hash and kind, revoke,
        touch `last_used_at`.
    *   `internal/db/limits.go`: get/set `user_limits`; count a user's
        active downloads under `pg_advisory_xact_lock(user_id)`.
    *   `internal/db/blocklist.go`: block by `(kind, key)` and item; check
        any of an item's keys.
    *   `internal/db/readers.go`: upsert and expire reader rows.
    *   `internal/db/usage.go`: ready bytes plus reserved bytes.

#### 12. `internal/db/repo_test.go` (Delete) and `internal/db/*_test.go` (Create)

*   **Type / Scope**: Database tests using `dbtest`.
*   **Proposed Changes**: Tests for each query file, including: two claimers
    get disjoint sets; an expired lease can be claimed again and the stale
    worker's write fails; file IDs survive ready → evicted → ready; a path
    missing from the second manifest is marked `missing`; `QueueAgain`
    never leaves `blocked`.

#### 13. `internal/state/state.go` and `state_test.go` (Create)

*   **Type / Scope**: Pure Go, unit tested.
*   **Proposed Changes**: The status and phase constants, the event list
    (`Claimed`, `LeaseLost`, `Finalizing`, `Published`, `Failed`,
    `Evicted`, `Requeued`, `Blocked`), and `Next(status, event)` backed by
    the table in Technical Plan section 3. The test walks every
    (status, event) pair.

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
    info dictionary bytes, so the info hash comes from the metainfo, not
    from text. Used by `sourcekey`.

#### 16. `internal/source/source.go` (Create)

*   **Type / Scope**: Interface package.
*   **Proposed Changes**: The `Source` interface, `Job`, `Handle`, `Report`,
    `Manifest`, `ErrClass` from Technical Plan section 4, and a registry
    keyed by fetcher name.

#### 17. `internal/source/sourcetest/contract.go` (Create)

*   **Type / Scope**: Shared test suite.
*   **Proposed Changes**: `Run(t, newSource)` checks the four rules for any
    implementation: Poll resumes after a new instance is created from the
    stored Handle; only the Finalize manifest is final; Cancel twice
    succeeds; error classes are set. Every source's test calls it.

#### 18. `internal/source/qbitsource/source.go` and test (Create)

*   **Type / Scope**: Source implementation.
*   **Proposed Changes**: Wraps `internal/qbit`. Handle holds the info hash
    and save path. Maps qBittorrent states to phases. `Finalize` lists
    files, removes the torrent with `deleteFiles=false`, and returns the
    manifest. Test runs the contract suite against the fake qBittorrent.

#### 19. `internal/source/fakesource/source.go` and test (Create)

*   **Type / Scope**: Test-only source.
*   **Proposed Changes**: Two-step fake: reports `waiting_remote`, then
    `downloading`, then changes its file list at `Finalize`; keeps state
    only in the Handle and `incoming/`, so it resumes after a restart; can
    be told to be slow or to fail with each error class.

#### 20. `internal/store/store.go` and test (Modify)

*   **Type / Scope**: Storage.
*   **Proposed Changes**: Turn `Store` into the interface from Technical
    Plan section 8 with a `Local` implementation keyed by item ID.
    `Publish` never deletes an existing destination: if `store/<id>` exists
    and `incoming/<id>` doesn't, return success. Paths are checked to stay
    inside the item's folder.

#### 21. `internal/store/reserve.go` and test (Create)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: Multipliers per source kind (torrent 1.0, http
    1.0, usenet 2.2), a placeholder size for items with no metadata yet,
    and `Fits(ready, reserved, want, limit, minFree)`.

#### 22. `internal/ingest/worker.go` and test (Modify)

*   **Type / Scope**: Download loop.
*   **Proposed Changes**: One loop per configured source kind, each waking
    on `torza_queued_<kind>` or its timer. Each tick: release expired
    leases, claim up to N items, start or poll each through the registry,
    write progress, renew leases, apply the fetch policy, and record
    `fetch_attempts`. Lazy connection to qBittorrent: a missing qBittorrent
    marks the torrent loop unhealthy instead of stopping the program.

#### 23. `internal/ingest/finalize.go` and test (Create)

*   **Type / Scope**: Download completion.
*   **Proposed Changes**: The four-step finish order from Technical Plan
    section 6, plus the tick-time repair of `finalizing` items. The test
    injects a failure after the rename and checks the next tick reaches
    `ready` without calling `Start` and without deleting files.

#### 24. `internal/ingest/policy.go` and test (Create)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: Given an item, its attempts and the starting
    user's `rd_fallback_after`, decide: keep waiting, switch fetcher, or
    fail. Stage 1 has only qBittorrent and the fake; RD/TorBox are added in
    stage 5.

#### 25. `internal/service/` (Create)

*   **Type / Scope**: Shared business rules.
*   **Proposed Changes**: Files `service.go` (constructor with db pool,
    store, signer), `add.go`, `library.go`, `play.go`, `block.go`,
    `auth.go`, `limits.go`, each with a database test. Behaviour as in
    Technical Plan section 7. Acceptance tests here: two users adding the
    same new key at once start one download and both libraries hold it;
    one user sending more adds than `max_active` at once gets exactly
    `max_active` downloads; a block racing an add, run 100 times, never
    ends queued or ready.

#### 26. `internal/scheduler/evict.go` and test (Modify)

*   **Type / Scope**: Eviction job.
*   **Proposed Changes**: Skip items with a reader seen in the last two
    minutes; mark files `missing` instead of deleting rows; count reserved
    bytes; take an advisory lock so only one copy runs.

#### 27. `internal/scheduler/purge.go` and test (Create)

*   **Type / Scope**: Takedown job.
*   **Proposed Changes**: Delete files of `blocked` items not yet purged,
    cancel any running fetch through the registry, set `purged_at`.

#### 28. `internal/jobs/jobs.go` (Delete)

*   **Type / Scope**: NATS wiring.
*   **Proposed Changes**: Removed; wake-ups use `db.Listen`/`db.Notify`.

#### 29. `internal/auth/keys.go` and test (Modify)

*   **Type / Scope**: Credentials.
*   **Proposed Changes**: `NewKey(kind)` with a kind prefix (`tza_api_`,
    `tza_str_`, `tza_dav_`), so a key's kind is visible and a leaked
    Stremio key can't be used as an API key.

#### 30. `internal/links/links.go` and test (Modify)

*   **Type / Scope**: Signed links.
*   **Proposed Changes**: Token carries a key ID; `Verify` rejects tokens
    whose key ID isn't the current one.

#### 31. `internal/api/server.go` and test (Modify)

*   **Type / Scope**: REST API.
*   **Proposed Changes**: Routes move from `/v1/torrents/{infohash}` to
    `/v1/items` and `/v1/items/{id}`; `POST /v1/items` accepts a magnet,
    hash or `.torrent` upload; `/v1/admin/block`, `/v1/admin/limits`,
    `/v1/admin/import` (owner-triggered RD import, active from stage 5),
    `/play/{file id}`. All handlers call `internal/service`.

#### 32. `internal/stream/server.go` and test (Modify)

*   **Type / Scope**: File serving.
*   **Proposed Changes**: Check access through `service.Play` (includes the
    blocklist by every source key; returns 451); open files through
    `Store.Open`; write a reader row while serving; count use only after
    50 MB or two minutes; count bytes served per user.

#### 33. `internal/config/config.go` and test (Modify)

*   **Type / Scope**: Configuration.
*   **Proposed Changes**: Split into `APIConfig`, `StreamConfig`,
    `IngestConfig`, `SchedulerConfig`, `MigrateConfig`, each loading only
    its own variables. Add `INGEST_SOURCES`, `LEASE_DURATION`,
    `EVICT_IDLE_FOR`, `POLL_INTERVAL`, `SIGNING_KEY_ID`.

#### 34. `internal/metrics/metrics.go` and test (Create)

*   **Type / Scope**: Observability.
*   **Proposed Changes**: A plain-text `/metrics` handler: queue depth per
    kind, oldest lease age, reserved bytes, evicted bytes, live readers,
    bytes served per user today. Readiness handler that pings Postgres.

#### 35. `cmd/migrate/main.go` (Create) and `cmd/{api,ingest,stream,scheduler}/main.go` (Modify)

*   **Type / Scope**: Program entry points.
*   **Proposed Changes**: `migrate` runs migrations and exits. The other
    four stop running migrations, load their own config, remove NATS, wire
    `db.Listen`, register sources (ingest), and mount `/metrics` and
    `/readyz`.

#### 36. `go.mod`, `go.sum` (Modify)

*   **Type / Scope**: Dependencies.
*   **Proposed Changes**: Remove `github.com/nats-io/nats.go` and its
    indirect dependencies; add the bencode package from entry 15.

#### 37. `Dockerfile` (Modify)

*   **Type / Scope**: Build.
*   **Proposed Changes**: Build all five programs into one image; tag passed
    in as the git commit.

#### 38. `deploy/docker-compose.yml`, `deploy/.env.example`, `deploy/Caddyfile` (Modify)

*   **Type / Scope**: Deployment.
*   **Proposed Changes**: Remove `nats`. Add a one-shot `migrate` service
    that the others depend on. One environment block per program. Image
    tag `torza:${TORZA_TAG}`. Pass `EVICT_IDLE_FOR` and `POLL_INTERVAL`.
    Caddy routes `/play/*` to `api`.

#### 39. `scripts/deploy.sh` and `scripts/backup.sh` (Create)

*   **Type / Scope**: Operations.
*   **Proposed Changes**: `backup.sh` runs `pg_dump` into a dated file.
    `deploy.sh` runs the backup, pulls or builds the image for a commit,
    runs `migrate`, restarts services and runs `smoke.sh`.

#### 40. `ARCHITECTURE.md` and `README.md` (Modify)

*   **Type / Scope**: Documentation.
*   **Proposed Changes**: Rewritten to match this design: items and source
    keys, the source interface, leases, finish order, service layer, no
    NATS, no RD-compatible API.

### Stage 2: First server deploy

No new files. Run `scripts/deploy.sh` on the VPS, then `scripts/smoke.sh`.
The stage is done when the smoke test passes twice in a row, including the
restart step, and the eviction check passes with a 20 GB cache limit.

### Stage 3: Usenet spike

#### 41. `internal/nzb/nzb.go` and test (Create)

*   **Type / Scope**: Parser.
*   **Proposed Changes**: Parse NZB XML into files and segments; compute the
    key (SHA-256 of sorted message IDs); wired into `sourcekey`.

#### 42. `internal/nntp/conn.go`, `pool.go` and tests (Create)

*   **Type / Scope**: Protocol client.
*   **Proposed Changes**: NNTP over TLS: connect, `AUTHINFO USER/PASS`,
    `BODY <message-id>`, dot-unstuffing; a pool with a per-provider
    connection limit.

#### 43. `internal/yenc/yenc.go` and test (Create)

*   **Type / Scope**: Decoder.
*   **Proposed Changes**: yEnc decode with `=ybegin`/`=ypart`/`=yend`
    parsing and CRC32 check, including multi-part articles.

#### 44. `internal/fakes/nntp/server.go` (Create)

*   **Type / Scope**: Fake upstream.
*   **Proposed Changes**: In-process TLS NNTP server serving fixed articles,
    with options for missing and damaged articles.

#### 45. `internal/source/usenet/source.go` and test (Create)

*   **Type / Scope**: Source implementation (spike).
*   **Proposed Changes**: Downloads and decodes all segments into
    `incoming/<id>/`, checkpointing finished segments in a file so `Poll`
    resumes after a restart; fails on any damaged article (no repair yet);
    passes the contract suite. Not registered in production until stage 9.

### Stage 4: Stremio library add-on

#### 46. `internal/stremio/handler.go` and test (Create)

*   **Type / Scope**: Access method inside `api`.
*   **Proposed Changes**: `/stremio/<credential>/manifest.json`, catalog of
    the user's library, meta for `torza:<item id>`, streams that point at
    `/play/<file id>`. CORS headers. Authenticated by a `stremio`
    credential.

#### 47. `internal/api/server.go` (Modify)

*   **Type / Scope**: Routing.
*   **Proposed Changes**: Mount the Stremio handler; `POST
    /v1/credentials` to create a Stremio credential.

### Stage 5: HTTP downloader with Real-Debrid and TorBox

#### 48. `internal/httpdl/downloader.go` and test (Create)

*   **Type / Scope**: Shared downloader.
*   **Proposed Changes**: Downloads a URL into `incoming/<id>/<path>` with
    `Range` resume, a per-account connection limit and progress reporting.

#### 49. `internal/source/realdebrid/` and `internal/source/torbox/` (Create)

*   **Type / Scope**: Source implementations.
*   **Proposed Changes**: `client.go` for each API, `source.go` running the
    six steps, Handle holding the remote ID and per-file download state.
    Credentials come from `provider_accounts` via `secret_ref`.

#### 49a. `internal/db/migrations/0002_providers.sql` and `internal/db/providers.go` (Create)

*   **Type / Scope**: Schema and queries.
*   **Proposed Changes**: `provider_accounts` table, and a foreign key from
    `fetch_attempts.account_id` to it; queries to list accounts and count
    their connections and bytes used today.

#### 50. `internal/fakes/realdebrid/server.go`, `internal/fakes/torbox/server.go` (Create)

*   **Type / Scope**: Fake upstreams.
*   **Proposed Changes**: Built from each service's API documentation,
    marked as not yet checked against real responses.

#### 51. `internal/ingest/policy.go` (Modify)

*   **Type / Scope**: Fetch policy.
*   **Proposed Changes**: Add the RD/TorBox fallback and the file-set
    match check.

### Stage 6: File hosters

#### 52. `internal/source/hoster/source.go`, `hosts.go` and tests (Create)

*   **Type / Scope**: Source implementation.
*   **Proposed Changes**: Per-host URL parser (host + file ID) and
    download-link resolution using the owner's account; download through
    `httpdl`. Folder links become one item per file. Owner-only, enforced
    in `service.Add`.

#### 53. `internal/fakes/hoster/server.go` (Create)

*   **Type / Scope**: Fake upstream.
*   **Proposed Changes**: A fake for each supported host's documented flow.

### Stage 7: WebDAV

#### 54. `internal/webdav/fs.go`, `handler.go` and tests (Create)

*   **Type / Scope**: Access method inside `stream`.
*   **Proposed Changes**: A read-only file system over the user's ready
    items using `golang.org/x/net/webdav`; app-password Basic auth with a
    `webdav` credential; opening a file never re-queues; reads go through
    the same reader and use tracking as `stream`.

#### 55. `deploy/Caddyfile`, `go.mod` (Modify)

*   **Type / Scope**: Routing, dependency.
*   **Proposed Changes**: Route `/dav/*` to `stream`; add
    `golang.org/x/net`.

### Stage 8: Web UI

#### 56. `internal/web/` (Create)

*   **Type / Scope**: Access method inside `api`.
*   **Proposed Changes**: `handlers.go` (add, library, item, credentials,
    owner limits and import pages), `session.go` (invite link or API key →
    HttpOnly session cookie), `csrf.go`, `templates/*.html` and
    `static/htmx.min.js` embedded with `embed.FS`, with handler tests.

### Stage 9: Full Usenet

#### 57. `internal/par2/` (Create)

*   **Type / Scope**: Library.
*   **Proposed Changes**: `parse.go` (packets), `verify.go` (block
    checksums), `repair.go` (Reed–Solomon over GF(2^16)), with tests on
    small generated sets with removed blocks.

#### 58. `internal/unpack/unpack.go` and test (Create)

*   **Type / Scope**: Tool wrapper.
*   **Proposed Changes**: Runs `7zz x` with a timeout and low priority,
    checks the exit code, lists the extracted files for the manifest,
    deletes archive volumes after success. The test uses a stub binary.

#### 59. `internal/source/usenet/source.go` (Modify)

*   **Type / Scope**: Source implementation.
*   **Proposed Changes**: Add verify → repair → unpack phases; register in
    production; reservation multiplier 2.2.

#### 60. `Dockerfile`, `deploy/docker-compose.yml` (Modify)

*   **Type / Scope**: Deployment.
*   **Proposed Changes**: A `usenet` build target that adds `7zz`; an
    `ingest-usenet` service with `-sources=usenet` and CPU/memory limits.

### Stage 10: Release-name parser and RSS

#### 61. `internal/release/parse.go` and test (Create)

*   **Type / Scope**: Pure Go.
*   **Proposed Changes**: Parse resolution, release type, codec, HDR,
    group, title, season and episode from a release name; table tests.

#### 62. `internal/netguard/client.go` and test (Create)

*   **Type / Scope**: Guarded HTTP client.
*   **Proposed Changes**: Dials public addresses only (checked after DNS,
    on every redirect), with size and time limits.

#### 63. `internal/rss/` (Create)

*   **Type / Scope**: Background job in `scheduler`.
*   **Proposed Changes**: `fetch.go` (through `netguard`), `filter.go`
    (uses `release`), `job.go` (poll due feeds, skip seen entries, add
    through `service.Add` at lowest priority, backlog when over limits,
    daily byte cap per feed), with tests.

#### 64. `internal/db/migrations/0003_rss.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: `feeds`, `feed_entries_seen`, `feed_backlog`.

#### 65. `internal/api/server.go`, `internal/web/` (Modify)

*   **Type / Scope**: Access.
*   **Proposed Changes**: Routes and pages to manage feeds.

#### 66. `internal/scheduler/evict.go` (Modify)

*   **Type / Scope**: Eviction.
*   **Proposed Changes**: RSS-added items nobody has opened go first.

### Stage 11: Metadata catalog

#### 67. `internal/catalog/` (Create)

*   **Type / Scope**: Background job in `scheduler`.
*   **Proposed Changes**: `client.go` for the metadata API, `match.go`
    (release parser → search → best match), `job.go` (enrich new ready
    items), with tests against a fake.

#### 68. `internal/db/migrations/0004_catalog.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: `item_titles` (item or file, title, year, season,
    episode, poster, external IDs, `manual` flag).

#### 69. `internal/stremio/handler.go`, `internal/web/` (Modify)

*   **Type / Scope**: Access.
*   **Proposed Changes**: Show titles and posters; owner/user can correct a
    match. Stremio IDs stay `torza:<item id>`.

### Stage 12: Telegram bot

#### 70. `internal/telegram/` (Create)

*   **Type / Scope**: Access method inside `api`.
*   **Proposed Changes**: `webhook.go` (route `/telegram/<secret>`, checks
    the secret-token header), `link.go` (one-time code links a chat to a
    user as a `telegram` credential), `commands.go` (add, list, play
    link), `notify.go` (listens on `torza_item_ready` and messages the
    users who have the item), with tests.

#### 71. `deploy/Caddyfile`, `internal/config/config.go` (Modify)

*   **Type / Scope**: Routing, config.
*   **Proposed Changes**: Route `/telegram/*` to `api`; add
    `TELEGRAM_BOT_TOKEN` and `TELEGRAM_WEBHOOK_SECRET` to `APIConfig`.

### Stage 13: Export to the user's own storage

#### 72. `internal/export/` (Create)

*   **Type / Scope**: Background job in `scheduler`.
*   **Proposed Changes**: `job.go` (claims export requests, counts as an
    active slot, marks the item as having a live reader while copying),
    `rclone.go` (runs `rclone copy` with a per-job config file written from
    the decrypted remote and deleted afterwards), with tests using a stub
    binary.

#### 73. `internal/db/migrations/0005_export.sql` (Create)

*   **Type / Scope**: Schema.
*   **Proposed Changes**: `user_remotes` (encrypted config), `exports`
    (item, remote, status, bytes, error).

#### 74. `Dockerfile`, `internal/web/`, `internal/api/server.go` (Modify)

*   **Type / Scope**: Build, access.
*   **Proposed Changes**: Add `rclone` to the scheduler image; pages and
    routes to add a remote and export an item.

### Stage 14: Only when needed

No files planned. Usenet streaming and cross-source matching get their own
design when a need appears.

### Verification Plan

*   **Compilation/Build**: `go build ./...`
*   **Unit Tests**: `go test -race ./...` (database tests skip locally,
    run in CI)
*   **Static Analysis & Linting**: `go vet ./...`; `shellcheck scripts/*.sh`
    in CI
*   **Test Coverage Requirement**: every row of the state table and every
    stage 1 acceptance test listed in files 12, 13, 19, 23 and 25 must
    exist and pass; no percentage target
*   **E2E/Integration**: `scripts/smoke.sh` on the server after every
    deploy from stage 2 on; each source passes the contract suite against
    its fake before deploy
