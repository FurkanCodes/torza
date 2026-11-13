# Torza: multi-source shared cache

* **Author:** Furkan
* **Date:** 2026-11-13

## Overview

Torza is a personal, educational debrid service. It exists to learn how
services like Real-Debrid and TorBox work inside. It fills one shared
cache from four sources:

- torrents, through qBittorrent
- Usenet (NZB files)
- file-hoster links, using the owner's own premium accounts
- the owner's own Real-Debrid or TorBox accounts

Each item is stored once, however many users ask for it. A handful of test
users reach the cache through a REST API, a web UI, a Stremio library
add-on, WebDAV and a Telegram bot. The add-on only lists and plays content
that is already cached or that the user added. Around that core: RSS
auto-download, a metadata catalog, export to a user's own storage, and
per-user limits set by the owner.

Legal questions, payments and market fit are out of scope. So are scraping
torrent sites, release-site scrapers and routing downloads through a VPN.

The starting point is the existing Torza MVP: torrents only, four Go services,
Postgres, NATS, signed links, and an on-disk cache keyed by info hash. None of
it has run against real services yet.

## Analysis

> Sections 1–13 are round one, written before the 2026-11-13 scope
> decisions. Where they conflict with Goals or Decisions, Goals and
> Decisions win. Round two (sections 14–21) reviews the enlarged scope.

Six independent reviewers looked at the idea and the current code. Their
merged findings follow, most important first.

### 1. The info hash does too many jobs

Today the torrent info hash is:

- the item's identity (the primary key of `torrents`)
- the folder name on disk
- the takedown key
- part of the API routes and the NATS message names

NZBs and hoster links have no info hash, so none of this works for them.

The fix is to separate *what an item is* from *how it gets filled*:

- **`items`**: an internal ID, status, size, storage path and last-used time.
  This is what gets cached and evicted.
- **`item_sources`**: a unique `(kind, key)` pair pointing at an item. One item
  can have several. When a request arrives, it is matched to an existing item
  through this table. The keys:
  - Torrent: the lowercase info hash.
  - NZB: a hash of the sorted article IDs inside the NZB, not of the file's
    bytes. Different indexers produce different NZB files for the same post.
  - Hoster: the URL reduced to "host + file ID". A folder link becomes
    several items.
  - Real-Debrid or TorBox import: no new key type. An import fills the same
    torrent item a qBittorrent download would, so an RD import and a
    qBittorrent download of the same torrent end up as the same item. A
    `fetch_attempts` table records which method was tried, with which account,
    and the result.
- **`item_files`**: one row per file in an item, keyed by `(item_id, path)`.

Matching on the files' content (spotting the same release arriving from two
different sources) only works after both copies have downloaded. It stays an
optional later step and is never the main key. Two different NZBs of the same
release therefore stay separate items.

Imports from Real-Debrid or TorBox copy the files into Torza's store. Passing
RD's links through instead would mean eviction, takedowns and "stored once" no
longer apply to that content.

### 2. File IDs change every time an item is downloaded again

When a torrent finishes, the code deletes its file rows and inserts new ones,
and eviction deletes them too. After an item is evicted and downloaded again,
every file has a new ID. That breaks anything that saved an ID: Stremio,
WebDAV clients, RD-style APIs, browser caches.

Fix: write each file row once, when the file list is first known. Mark files
`present` or `missing` instead of deleting the rows. The same flag later
handles choosing which files to download.

### 3. Four bugs in the current code that new sources would copy

The reviewers found these and they were confirmed in the code:

1. **Takedown race.** If a torrent is added at the same moment it is blocked,
   it can end up downloaded and served. The stream service only checks the
   status, not the blocklist.
2. **Duplicate-add race.** When two users add the same new torrent at the same
   moment, the second add resets the first one's download and takes it over.
   The per-user limit can also be exceeded by sending requests at the same
   time.
3. **Unsafe finish order.** If saving "ready" to the database fails after the
   files have been moved, the next round adds the torrent to qBittorrent again
   and then deletes the finished copy.
4. **No claiming of work.** Every ingest process picks up every queued item,
   so running two download servers would make both download the same thing.
   All steps also run one after another, so a slow Usenet or hoster download
   would hold up torrent progress tracking.

### 4. The download engine is shaped like qBittorrent

The ingest code calls qBittorrent's functions directly and reads its status
words. It needs a general source interface: start, check progress, list
files, cancel. Hosters and RD/TorBox work in two steps (the remote side gets
the file ready, then Torza downloads it over HTTP), and the interface has to
allow for that.

The rules for moving between statuses (queued, downloading, ready and so on)
should be ordinary Go code. That can be unit-tested, which matters while
testing is limited to compiling and unit tests.

### 5. Too much untested code before anything has run

No test runs any SQL. Every bug in section 3 lives in SQL, or in the order of
database and disk steps, so none can be proven before a deploy. Adding three
sources and four access methods first would mean one large, untested
integration.

Ways to test more without running anything on your Mac:

- Database tests against real Postgres in GitHub Actions (CI).
- Integration tests that only run on the server.
- Fake qBittorrent, Real-Debrid, TorBox, hoster and Usenet servers built from
  their documentation. Mark them "from the docs, not yet checked" until real
  responses are captured.
- A smoke-test script, and a deploy after every stage.

### 6. Download first, stream later

Every source should download completely before anything is served.

- **Torrents:** qBittorrent can't reliably download pieces in order.
- **Usenet:** repairing damaged posts (par2), unpacking RAR archives and
  missing articles make streaming from Usenet a separate project.
- **Hosters and RD/TorBox:** download into `incoming/` with resume support,
  then move into the cache the same way torrents do. Keep partial files so a
  retry can continue where it stopped.

A reader layer in the stream service keeps the door open for "stream while
downloading" later.

### 7. Eviction can delete a file someone is watching

The "recently used" mark is written at most once a minute, and anything
unused for 15 minutes can be evicted. A long single download, or a video
paused for over 15 minutes, can lose its file in the middle.

On Linux, deleting a file that is still open doesn't free its space until it's
closed. The next eviction round still sees a full disk and deletes more. Disk
used by downloads in progress isn't counted at all.

Fixes:

- Track who is reading each file right now.
- Reserve disk space when a download starts.
- Measure the cache size from the database, not only from free disk space.
- When a user plays an evicted item, queue it for download again instead of
  returning a 404.

### 8. Stremio

Stremio asks for streams by IMDb ID. Torza has no movie metadata, and
scraping is out of scope. The first version should be a per-user "My Torza
library" catalog that shows ready items only, using Torza's own IDs.

- Stream responses point at `/play/...`. That route checks access and
  redirects to a fresh signed link. Signed links expire after six hours, and
  Stremio saves the URLs it is given.
- Each user gets a separate add-on token they can revoke. Stremio puts its
  configuration in the URL, so the API key would end up in logs.
- The add-on needs CORS headers.

### 9. Real-Debrid-compatible API

Dropped by decision 6.

### 10. Takedowns across sources

Block by source key and by item. Otherwise blocked content comes back
through a different NZB or URL. Deleting blocked content goes through each
source's own code.

### 11. Owner accounts and credentials

Only the owner (admin) can add hoster or RD/TorBox sources, because those use
the owner's paid accounts. Test users get the cached result.

- A `provider_accounts` table holds each account's connection limit and daily
  data allowance.
- The credentials themselves stay inside each source's settings, never
  visible to the access methods.

### 12. Where new code goes

- Stremio and the web UI go inside `api`, which controls access.
- WebDAV goes inside `stream`, which serves files.

So no new programs are needed. Each access method gets its own package that
calls a shared service layer, never the database directly. Each one uses its
own type of credential, and each can be revoked separately.

### 13. Considered and overruled (round one suggested cutting these)

- **Cut:** S3 through rclone, bring-your-own-storage, the Telegram bot, and
  separate catalog, Stremio and WebDAV programs. They add moving parts and
  teach nothing about caching that a local disk doesn't.
- **Testing eviction:** set the cache limit small (20–50 GB).
- **Three mechanisms, not four:** qBittorrent for torrents, one HTTP
  downloader shared by RD/TorBox and hosters, and Usenet (the only new
  protocol).

### 14. Per-user "eviction budget" doesn't fit a shared cache

One item can sit in several libraries, so every way of charging it fails:
charge every holder and budgets exceed the disk; split it and a user's
usage jumps when someone else leaves; charge the first requester and the
rest ride free. Evicting for one user deletes the item for everyone.

Decided (2026-11-13): the model large debrid services use. Global
least-recently-used eviction is the only rule, and no user gets a share of
the disk. Size caps and concurrency limits apply only when an add *starts*
a download; adding something already cached is free. Priority orders only
the work-claim query. A user's library row survives eviction; the item just
shows as not cached until someone plays it again. `user_limits` goes in the
stage 1 schema. Pins (per-user GB of items exempt from eviction) are left
out for now; adding them later only makes the eviction query skip pinned
items.

### 15. Bring-your-own-storage is undefined

If the shared cache itself lived on an rclone remote, it would break the
atomic rename that publishes a download (`internal/store/store.go:64-73`),
the free-space check, and byte-range serving through `os.Open`
(`internal/stream/server.go:86,108`). A copy per user breaks "stored once"
and takedowns.

Recommended: an **export**, never a storage backend. A job pushes a ready
item to the user's own remote; the cache stays local. In stage 1, turn
`store.Store` into a small interface (`Open`, `Publish`, `Remove`, `Usage`)
anyway. *(Decided 2026-11-13: export.)*

### 16. The source interface needs a written contract

"Start, check progress, list files, cancel" is shaped like qBittorrent:

- **Usenet's file list changes.** An NZB lists RAR and PAR2 files; after
  repair and unpacking the served files are different. File rows must come
  only from the final file list.
- **Torza's own Usenet client has no outside state.** A restart mid-repair
  loses everything unless progress checks can resume from stored state.
- **RD/TorBox is six steps, not two:** add, wait for metadata, select files,
  wait for the remote download, unrestrict, HTTP download with resume.
- **No fetch policy.** Nothing says who chooses qBittorrent vs. an RD/TorBox
  import, or what happens on failure. Automatic imports would let test
  users spend the owner's RD quota.

**Fetch policy (decided 2026-11-13):** qBittorrent is always tried first,
for everyone. RD/TorBox imports are off for test users by default; the
owner can trigger one for any item, and can turn on automatic fallback per
user ("no metadata from qBittorrent after X minutes → import from RD").
Hoster links stay owner-only. An import selects all files, or the item is
ready only when its file set matches.

Recommended shape:

```go
type Source interface {
    Kind() string
    Start(ctx, Job) (Handle, error)         // Handle stored in the DB
    Poll(ctx, Handle) (Report, error)       // phase, bytes, error class, next poll
    Finalize(ctx, Handle) (Manifest, error) // final file list under incoming/
    Cancel(ctx, Handle, deleteData bool) error
}
```

Rules: `Poll` resumes after a restart from the Handle and `incoming/`; file
rows are written only from `Finalize`'s manifest; `Cancel` is safe to call
twice. Two-step sources report a `waiting_remote` phase and do the HTTP
download inside their own implementation. Error classes: retry, permanent,
try another method.

### 17. Stage 1 needs a definition of done

- **States.** Keep `status` small (queued, active, finalizing, ready,
  failed, evicted, blocked) and add a `phase` column only ingest reads
  (starting, waiting_remote, downloading, verifying, repairing,
  unpacking). Transitions live in a pure-Go table; `blocked` is final.
- **Claiming.** Today every worker gets every row
  (`internal/db/repo.go:193-195`), and a second ingest would re-add the
  first one's torrents to its own qBittorrent. Use Postgres leases
  (`FOR UPDATE SKIP LOCKED`, `claimed_by`, `lease_until`, renewed on each
  poll), one loop per source kind. Stage 1 runs on one node; leases are for
  surviving crashes.
- **Finish order.** Today: remove from qBittorrent, then `Publish` (which
  first deletes the destination, `store.go:69`), then mark ready. New
  order: set `finalizing` → source releases files → rename into the store
  (never delete an existing destination) → one transaction writes file rows
  and `ready`. Each tick completes any `finalizing` item whose folder
  already exists.
- **Stable file rows.** Upsert by `(item_id, path)`; paths missing from a
  new manifest and evicted files are marked `missing`; rows are deleted
  only when blocked content is purged.
- **Service layer.** Today the same rules are copied between api, stream
  and ingest. An in-process `internal/service` package owns adding
  (dedup, blocklist and limits in one transaction), library queries,
  `/play`, takedowns and limits. Access packages never touch `db`.
- **Credentials.** One `api_key_hash` per user isn't enough for Stremio,
  WebDAV, web UI and Telegram. Add a `credentials` table (kind, hash,
  revoked_at) in stage 1. Each binary gets only the secrets it needs.

### 18. CI is in place but tests no SQL yet

GitHub Actions runs on every push with Postgres 17, but no Go code reads
`TEST_DATABASE_URL`, so the four bugs are still unproven. Stage 0 is done
when a `dbtest` helper gives each test its own schema, runs the migration
(and fails, not skips, when `CI=true` and the URL is missing), and at
least one test runs the migration twice.

Two corrections to the round-one bug list: the takedown race is
`Block`'s `UPDATE` missing the add's uncommitted `INSERT`, after which the
item is served indefinitely; the duplicate add mostly moves the quota
charge to the second user. The limit race is a `count(*)` with no lock.

### 19. Usenet, RSS and WebDAV risks

- **Usenet disk.** Peak use is about 2–2.3× the final size (RAR + PAR2 +
  extracted). Reserve disk per job by source kind and refuse claims that
  don't fit. Run Usenet ingest in its own container. Split the work into
  download and verify, repair, and unpack; decide early whether unpacking
  calls `unrar`/`7z`.
- **RSS.** User-supplied feed URLs let the server fetch internal addresses
  (qBittorrent, NATS, Postgres): a server-side request forgery hole. Use a
  guarded HTTP client (public IPs only, size and time limits), a
  seen-entries table so evicted items aren't re-fetched, a daily byte cap
  per feed, and evict RSS-added items first until someone opens them.
  Support `.torrent` files, not just magnets.
- **WebDAV and media scanners** open every file. Opening must not re-queue
  an evicted item or count as "use"; only real bytes read do. WebDAV lists
  ready items only.

### 20. Catalog and Stremio IDs

Stremio IDs stay `torza:<item>` permanently; the catalog adds titles and
posters through an `item_titles` table that users can correct by hand.
One release-name parser package serves both RSS filters and catalog
matching.

### 21. Smaller items

- The migration runner records filenames only, so a rewritten
  `0001_init.sql` is skipped on a database that already ran it. Give the
  rewrite a new name.
- Signed links need a key ID so the signing key can be rotated.
- `/healthz` checks nothing; add readiness checks and a small `/metrics`.
- The Telegram bot sends `/play` links only (the Bot API caps uploads at
  50 MB) and runs as one copy.
- ARCHITECTURE.md and README.md still describe the old torrent-only design
  with an RD API; rewriting them is part of stage 1.

## Goals vs. Non-Goals

**Scope rule (decided 2026-11-13):** the full core of a debrid service,
without billing, plans, payments or a reseller program. Torza is its own
design, written from scratch.

* **In-Scope Goals:**
  - One shared cache with a source-independent identity model
    (`items` / `item_sources` / `item_files` / `user_items`).
  - Sources: torrents (qBittorrent), Real-Debrid/TorBox
    imports, file hosters (owner's own accounts), Usenet (NZB). All
    download completely before being served.
  - Access: REST API, web UI, Stremio library add-on, WebDAV,
    Telegram bot.
  - RSS auto-download from feeds the user supplies, with filters
    (resolution, source, codec, HDR, group).
  - A metadata catalog for titles and posters, from an official metadata API
    (for example TMDB), not scraped.
  - Export to the user's own storage: a job copies a ready item to the
    user's own rclone remote. The shared cache always stays on the
    server's local disk (decided 2026-11-13).
  - Per-user limits in place of paid plans: concurrent downloads, size
    caps and priority, applied only when a new download starts, set by the
    owner, with no billing. No per-user storage share.
  - Stable file IDs, takedowns that work across sources, eviction that is safe
    during playback.
  - A handful of invited test users, at most about 10, with private
    libraries.
  - More testing without running anything locally: CI database tests,
    fake upstream servers, and a smoke test on the server.
* **Out-of-Scope Non-Goals:**
  - Billing, plans as products, payments, the reseller program, public
    signup, legal compliance work, market fit.
  - Scene/HDEncode release scrapers, VPN download routing, and a Stremio
    add-on that searches torrent sites. Torza's add-on
    lists only cached or user-added content.
  - Streaming while downloading, and Usenet streaming (for now).
  - Matching files that arrive from different sources (for now).
  - A Real-Debrid-compatible API.

## Key Decisions & Staging for Next Steps

**Decisions:**

1. **Rewrite or migrate — decided (2026-11-13):** rewrite the first database
   migration around `items`, since nothing is deployed. Ship torrents only
   for the first deploy, but build the schema and ingest code generic over
   sources from the start, so that Usenet, hosters and RD/TorBox are later
   additions, not rewrites. After the first deploy, switch to step-by-step
   migrations that keep IDs, with a backup before every deploy.
2. **Cross-source matching — decided:** later, as an optional step.
3. **RD/TorBox imports — decided:** copy the files into Torza's store, don't
   proxy. qBittorrent first; imports are owner-triggered, or automatic
   only for users the owner enables (see section 16).
4. **Usenet — decided (2026-11-13):** Torza writes its own Go Usenet
   client rather than driving SABnzbd/NZBGet, since learning how it works
   is the point: NNTP over TLS, a pool of connections, yEnc decoding, PAR2 check
   and repair, unpacking. It is the largest single source, so it stays late
   in the build order.
5. **CI testing — decided (2026-11-13), not yet testing SQL:** yes. Tests run on GitHub
   Actions (private repo, free minutes) against a real Postgres 17;
   database tests read `TEST_DATABASE_URL` and skip without it. Nothing
   runs on the owner's Mac beyond `go build`, `go vet` and `go test`.
6. **RD-compatible API — decided (2026-11-13):** no.

**Build order (revised after round two):**

0. **Test setup.** Done when: CI green (it is), a `dbtest` helper used by at
   least one real-Postgres test, a fake qBittorrent server, and a
   smoke-test script that exits 0 or 1.
1. **Source-generic core, torrents only.** Schema with `items`,
   `item_sources`, `item_files`, `user_items`, `credentials`, `user_limits`
   and a blocklist by `(kind, key)`, in a newly named migration; states
   plus `phase`; lease claiming with one loop per kind; the finish order;
   file rows from the manifest; the service layer; the store interface;
   disk reservations and live-reader tracking; the four bug fixes, each
   with a real-Postgres test; a fake two-step source that changes its file
   list at finalize and resumes after a restart; ARCHITECTURE.md
   rewritten.
2. **First server deploy and smoke test.** Deploy after every later stage.
3. **Usenet spike:** NZB parsing, the article-ID key, one NNTP/TLS fetch,
   yEnc, a fake NNTP server in CI. Proves the interface before more is
   built on it; nothing served yet.
4. Stremio library add-on.
5. HTTP downloader with RD/TorBox, with the fetch policy.
6. File hosters.
7. WebDAV (app passwords, ready items only, no re-queue on open).
8. Web UI (server-rendered, session plus CSRF, owner's limits screen).
9. Full Usenet: repair, then unpacking.
10. Release-name parser, then RSS with its safeguards.
11. Metadata catalog.
12. Telegram bot.
13. Export to the user's own storage through rclone.
14. Only when needed: Usenet streaming, cross-source matching.
