# Stage 9 — Implementation Readiness Test

* **Target:** `docs/designs/torza.md`, DetailedRange lines 679–1303
* **Reviewers (fresh context, in parallel):** egm-implementation-readiness and egm-codebase-details (implementation-readiness-standard), egm-style (persona rules)
* **Date:** 2026-11-13

## Verdicts

* **Implementation readiness — No.** Stages 0–2 can't be built without
  decisions that change the outcome: the schema (types, keys, missing
  columns), the locking that fixes the two race bugs, the REST and `/play`
  contract the smoke test needs, and where purge runs are left open; the
  five-commit split can't keep CI green; the smoke test can't pass twice in
  a row. Stages 3–13 are a roadmap and file list; each needs its own
  detailed pass, and several hide product choices (metadata API, hosters,
  invites, RSS schema, remote-config encryption).
* **Codebase accuracy — FAIL.** Commits 1–4 don't compile; the new test
  helpers create import cycles; the Dockerfile can't build the current
  module, so the first deploy fails. Several existing behaviours are dropped
  without a word (`create-user`, size and metadata-timeout checks,
  re-adding a lost torrent, ETag/modtime).
* **Style — precision problems.** Little fluff; two undefined terms ("six
  steps", stage 2 "eviction check"), "kind" with three meanings, many limits
  and timeouts without a value or setting, and entries that conflict with
  each other or the technical plan.

Findings are merged and deduplicated; reviewers in brackets (IR =
implementation readiness, CB = codebase details, ST = style).

## 🔴 Blocker

### B1. The stage 1 commits don't each build [IR M1, CB B1, ST M14]
Commit 1 deletes `repo.go` and the torrent types, but `cmd/*/main.go`,
`internal/api/server.go:26-34`, `internal/ingest/worker.go:21-29`,
`internal/stream/server.go:24-27` and `internal/scheduler/evict.go:17-21,52`
use `db.Repo`, `db.Torrent`, `db.File`, `db.FileInput` until commits 3–5.
Commit 2 makes `store.Store` an interface while ingest (`worker.go:110,153`),
stream (`server.go:81`) and `cmd/*` still call `store.New`, `IncomingDir`,
`FilePath`. Commit 3 deletes `jobs.go`, imported by `cmd/api/main.go:18,53-59`
and `cmd/ingest/main.go:9,40-47` until commit 5. Entry 22 (commit 3) needs
config from entry 33 (commit 5).
**Fix:** add new packages beside the old ones, switch callers, then delete;
each commit updates its own callers; or say stage 1 is one commit.

### B2. Test helpers create import cycles [CB B2, B3; IR nit]
`dbtest` imports `internal/db`; `internal/db`'s tests (`package db`,
repo_test.go:1) importing `dbtest` is a cycle. Entry 2 also needs an
un-migrated schema, but `New` always migrates. The shared qBittorrent fake
importing `internal/qbit` cycles with `client_test.go` (`package qbit`), and
its package name `qbit` clashes.
**Fix:** `internal/db` database tests are `package db_test`;
`dbtest.NewEmpty(t)` and `dbtest.New(t)`; set `search_path` via
`pgxpool.ParseConfig` → `ConnConfig.RuntimeParams` → `NewWithConfig`. Name
the fake `qbitfake`, give it its own JSON structs (or make `client_test.go`
`package qbit_test`), and keep what `client_test.go` checks today (403 then
retry, single login, `Fails.` reply, wrong password).

### B3. The Dockerfile can't build the module [CB B4] — confirmed
`Dockerfile:3` is `golang:1.25-alpine`; `go.mod:3` is `go 1.26.0`, and
official images set `GOTOOLCHAIN=local`, so `go mod download` fails. This is
a bug in the current code, not only the plan. "Build all five programs" is
already true (`./cmd/...`); a Dockerfile doesn't tag images.
**Fix:** entry 37 sets `golang:1.26-alpine` (optionally `ARG COMMIT` and
`-ldflags "-X main.version=$COMMIT"`); tagging moves to `deploy.sh`
(`docker build -t torza:$(git rev-parse --short HEAD)`).

### B4. The schema is column names, not a schema [IR B1, CB M10, ST M1]
No types, nullability, defaults, primary/foreign keys or `ON DELETE` rules
(e.g. `progress`, `credentials.hash`, `payload` for binary `.torrent`, PKs of
`readers` and `fetch_attempts`). `user_items.source_id` points at
`item_sources`, which has no `id`; no `items.handle` though the Handle is
"stored on the item"; no fetcher column though the registry is keyed by
fetcher; `source_kind` values undefined (loop kinds vs key kinds vs
fetchers).
**Fix:** full `0001_items.sql` DDL or a column/type/null/default/FK table;
add `items.handle bytea`, `items.fetcher text`, `item_sources.id bigserial`,
`payload bytea` + `payload_type`, `CHECK` on `source_kind`; a fetcher →
loop-kind map (qbittorrent→torrent, realdebrid/torbox/hoster→http,
usenet→usenet).

### B5. The race-fix locking isn't written down [IR B2]
The acceptance tests (block racing add; two adds start one download) have no
mechanism. Following section 7 literally fails: Add inserts in an
uncommitted transaction, Block can't see it, inserts only the blocklist key
and commits, Add commits a `queued` item. `ON CONFLICT` on `item_sources`
either leaves orphan `items` rows or needs an undescribed retry.
`pg_advisory_xact_lock(user_id)` only serialises one user's adds.
**Fix:** Add and Block both take
`pg_advisory_xact_lock(hashtextextended(kind||':'||key, 0))` per source key
first; Add then takes the user lock (fixed order: key locks, then user
lock), checks the blocklist, `SELECT … FOR UPDATE` on `item_sources` join
`items`, inserts item then source if absent; Block for a key with no item
inserts only the blocklist row, which Add then sees. Name the statements and
lock order.

### B6. The REST and `/play` contract is undefined [IR B3, CB M13, ST M11]
No request/response JSON for `POST /v1/items`, `GET /v1/items[/{id}]`,
`/v1/admin/block` (item id, key, or both?), `/v1/admin/limits`; no
`.torrent` upload format; no error → status mapping (451, 429, not ready,
evicted-and-requeued). Today's `GET /v1/me`, `POST /v1/files/{id}/link`,
`POST /v1/admin/blocklist` (server.go:67-73) and `/d/{token}/{name}`
(stream/server.go:44, Caddyfile:4) aren't kept, renamed or removed; the
diagram says `/dl`. `/play/<file id>` has no place for a credential and
Stremio can't send headers.
**Fix:** a route table (method, path, credential kind and how it's carried,
body, response, status codes); confirm `/d/` stays; Stremio play as
`/stremio/<credential>/play/<file id>` or `?k=`.

### B7. Purge runs where the credentials aren't [IR B4, CB M5, ST M7]
Entry 27 cancels running fetches "through the registry" from `scheduler`;
only `ingest` holds credentials (design 278, 645). Today purge is in ingest
(worker.go:80-101), woken by NATS `SubjectBlocked`, with no listed
replacement.
**Fix:** either an ingest job woken on a new `torza_blocked` channel calls
`Cancel(h, true)`, `Store.Remove`, sets `purged_at`; or scheduler deletes
only `store/<id>` and each ingest loop cancels `blocked` items it holds. Add
the channel to entry 9 and section 1.

### B8. The smoke test can't pass twice in a row, and doesn't match stage 2 [IR B5, ST B2]
"Blocks a test item": if it's the named torrent, it's blocked for good
(blocked is final) and run 2 fails at step 1. The torrent is never named;
"no second download started" has no observable; stage 2 requires an
eviction check with a 20 GB limit that the script doesn't have, and no
cache-limit setting exists in entry 33. The script targets the stage 1 API
but is written in stage 0; it restarts containers (needs host access and
the compose path); tools (curl, jq) aren't listed.
**Fix:** name the torrent (magnet, size); block a never-added throwaway
hash each run and check 451 on add and play; prove one download via
`fetch_attempts` count or `started_by`; add an eviction step with
`CACHE_MAX_BYTES` set small; list dependencies; write the script at the end
of stage 1.

### B9. "The six steps" are never defined [ST B1]
Line 1097 and 492 refer to them; they exist only in concepts_brief.md.
**Fix:** list them in section 4: add magnet/hash; wait for metadata
(`waiting_remote`); select all files; wait for the remote download; get a
direct link per file; download each with `httpdl`.

## 🟡 Major

* **M1. Existing behaviour dropped** [CB M3, M2, M6; IR M7]:
  * `worker.go`'s size cap (176), metadata timeout (172), `error`/
    `missingFiles` → fail (168), busy-at-100% (187-189), unsafe paths (201)
    and re-adding a lost torrent (149-156) aren't placed. Entry 18's Handle
    (hash + save path) can't re-add a lost torrent. Fix: entry 18 maps
    `error`/`missingFiles` → Permanent, keeps busy states unfinished,
    re-adds from the Job payload (magnet or `.torrent` bytes); entry 22/24
    applies `started_by`'s `max_item_bytes` once `BytesTotal` is known.
  * `create-user` (cmd/api/main.go:42-48,69-89; README.md:85) writes
    `users.api_key_hash`, which disappears. Fix: entry 35 keeps
    `api create-user -email -admin`, inserting `users`, an `api` credential
    and a `user_limits` row with stated defaults.
  * `Store` interface drops `IncomingDir` (qBittorrent save path,
    worker.go:110) and `EnsureDirs` (cmd/ingest/main.go:25); `Open` loses
    modtime used for ETag and `ServeContent` (stream/server.go:106,108);
    `free_unix.go:8` has a method on the struct and won't compile. Fix: add
    both methods, `Open` returns `(io.ReadSeekCloser, fs.FileInfo, error)`,
    list `free_unix.go` (Modify) and the scheduler `Disk` interface
    (evict.go:24-27).
* **M2. Handler-test fakes have no replacement** [CB M7]. Today's tests use
  interface fakes (api/server_test.go:28-107, ingest/worker_test.go:24-127,
  stream/server_test.go:22-36, scheduler/evict_test.go:52-75). With a
  concrete pgx-backed service, they become CI-only DB tests unless an
  interface is defined. Fix: a small `Service` interface per package, keep
  fake-based unit tests; name tests to keep (`TestStillBusyAtFullProgress`,
  `TestReaddsLostTorrent`, `TestPurgeBlocked`).
* **M3. Service constructor and stream's access check** [CB M8, IR M9].
  `service` takes "db pool, store, signer", but scheduler mustn't hold the
  signing key and `api` has no `/data` volume (compose 64-66). `stream`
  calling `service.Play` re-queues on every stale link and signs links it
  doesn't need. Fix: signer and store optional; add `service.CanRead(user,
  file) (File, Item, error)` for stream and WebDAV.
* **M4. Source types and registry not defined** [IR M2, ST M8]: `Job`
  fields, `Handle` type, `Report`/`Manifest` field types, whether `ErrClass`
  is a field or a typed error, `Register`/`Get` signatures, when the worker
  calls `Start` vs `Poll`, which fetcher a loop picks. Paste the
  definitions.
* **M5. State table holes** [IR M3]: no exit for a failed `Finalize`; retry
  count and backoff for `Retry`; whether `Permanent` while `active` deletes
  partial data.
* **M6. Limits undefined** [IR M4, ST M10]: defaults with no `user_limits`
  row (today `MAX_ACTIVE_PER_USER=3`); priority scale and RSS "lowest";
  the active-status set (queued + active + finalizing?); who fails an item
  over the starter's `max_item_bytes` once metadata arrives; whether
  `MAX_TORRENT_BYTES` and `METADATA_TIMEOUT` survive.
* **M7. Disk reservation not implementable as sketched** [IR M5]:
  `reserved_fits` in a set-based `LIMIT $n` claim can't see other rows in
  the same statement; no placeholder value; no update when the real size
  arrives; no overflow rule. Fix: claim one row per statement under an
  advisory lock, give the placeholder, replace with `BytesTotal ×
  multiplier` on the first sized `Poll`. (Stage 8 recommends removing
  reservations from stage 1 entirely.)
* **M8. Config** [IR M6, CB M11, ST M3, M4]: no defaults or units for
  `LEASE_DURATION`, `POLL_INTERVAL`, `SIGNING_KEY_ID`, claim batch N,
  worker ID; existing variables (`MAX_ACTIVE_PER_USER`, `MAX_TORRENT_BYTES`,
  `METADATA_TIMEOUT`, `CACHE_MAX_BYTES`, `CACHE_MIN_FREE_BYTES`,
  `EVICT_INTERVAL`, `LINK_TTL`, `PUBLIC_URL`, `NATS_URL`) not marked kept or
  removed; `-sources` vs `INGEST_SOURCES` (pick `INGEST_SOURCES`);
  `.env.example` contents. Fix: a variable / program / type / default /
  kept-new-removed table.
* **M9. Reader tracking and bytes served undesigned** [IR M8, CB M9, ST M5,
  M12]: `reader_id`, heartbeat interval, whether "50 MB or 2 minutes" is per
  request or across range requests, cleanup of old rows; no storage for
  bytes per user per day, and a `stream` in-memory counter can't show on
  `api`'s `/metrics`.
* **M10. `/metrics` exposed publicly** [IR M10]: Caddy's catch-all
  (Caddyfile:8) sends `/metrics` to `api`, leaking per-user bytes and queue
  state. Fix: an internal unpublished port, or admin auth; state each
  program's port.
* **M11. `.torrent` support incomplete** [IR M11, CB M4]: `qbit.Client.Add`
  (client.go:71-97) only sends `urls`; no entry modifies it; payload storage
  undecided; fake must accept both. Fix: entry for `client.go`
  `AddFile(ctx, torrentBytes, savePath)` (multipart `torrents`).
* **M12. Bencode library left open** [IR M12, CB M12, ST nit]: jackpal
  decodes into maps, so the raw `info` bytes for SHA-1 need re-encoding.
  Fix: a hand-written stdlib scanner returning the `info` byte span, or
  `github.com/anacrolix/torrent/bencode` with its raw type.
* **M13. LISTEN/NOTIFY details** [IR M13]: payload format, every channel's
  sender and listener (who sends `torza_item_ready`), reconnect backoff and
  "full check after reconnect".
* **M14. No stage 2 runbook** [IR M14, ST M18, CB N5]: no registry, so
  build on the server; VPS prerequisites, `.env` generation, qBittorrent's
  first-login password, backup directory and retention, `docker compose
  exec -T postgres pg_dump …` (no `pg_dump` on the host), an actual rollback
  command.
* **M15. Stage 0 tests stage 1 behaviour** [CB M1, IR M1]: entry 2's
  "unknown migration name fails" needs entry 9; stage 0 ends red. Move the
  check to stage 1.
* **M16. Undefined values** [ST M5, M6]: magnet placeholder size; claim
  batch N; smoke-test ready timeout; reader refresh interval; `7zz` timeout
  and priority; Usenet container CPU/memory; guarded client size/time
  limits; RSS numeric priority and where the byte cap lives; what
  "unhealthy" means (`/readyz` reports `torrent: down`, other loops run).
* **M17. Contract suite rules don't match section 4** [ST M2]: entry 17
  replaces rule 4 (`waiting_remote`) with "error classes are set". Use
  section 4's four rules plus "every error has an `ErrClass`".
* **M18. qBittorrent state map missing** [ST M9]: the fake's "metadata →
  downloading → done" are neither qBittorrent states nor phases. Add the map
  (`metaDL` → `waiting_remote`; `downloading`/`stalledDL` → `downloading`;
  `uploading`/`stalledUP`/`pausedUP` → finalize; `error`/`missingFiles` →
  error class) and use those strings in the fake.
* **M19. Image packaging contradicts itself** [ST M13, CB N6]: "the
  scheduler image" doesn't exist (one image, entry 37; a `usenet` target,
  entry 60). Name the Alpine packages (`7zip`, `rclone`) and the targets.
* **M20. Verification plan vs stage gate** [ST M19]: `go test ./...` (682)
  vs `go test -race ./...` (1294); "files 12, 13, 19, 23 and 25" should be
  entries 12, 13, 17, 23, 25; "every row" vs "every (status, event) pair".
* **M21. RSS and file-set wording** [ST M16, M17]: "nobody has opened"
  conflicts with the use definition — use "only `added_via='rss'` rows and
  `last_used_at` null"; file-set match = same `(path, size_bytes)` set as
  the metainfo.
* **M22. Stages 3–13 are outlines** [IR, ST M15]: each needs a detailed
  pass before it starts. Open decisions: stage 3 NZB storage and submission,
  provider credentials before `provider_accounts`, checkpoint format; stage 4
  manifest fields and credential route body; stage 5 six steps → endpoints,
  poll cadence, file-set rule, `secret_ref`, quota enforcement, import body;
  stage 6 which hosters, folder links changing `Add`'s return type; stage 7
  layout, collisions, Basic-auth username, use tracking in `Read`; stage 8
  invite table and kind, session lifetime, CSRF; stage 9 `7zz` in the image,
  low priority in a container; stage 10 table columns, poll interval, filter
  syntax, byte-cap value; stage 11 the metadata provider and key; stage 12
  command syntax; stage 13 encryption algorithm and key variable, slot
  meaning, remote input format.

## 🟢 Nit

* Entry 1: "named after the test" vs `t_<random>`; the real reason for
  per-test schemas is data isolation, not the advisory lock [IR, CB N9, ST].
* Entry 33 "adds" `EVICT_IDLE_FOR` and `POLL_INTERVAL`, which exist
  (config.go:60,63); list `NATS_URL` as removed [CB N1].
* Entry 22's "lazy connection" is already true (`qbit.New` never connects);
  the change is that a failing `Info` marks the loop unhealthy [CB N2].
* Entries 38 and 71 add Caddy routes the catch-all already covers; only
  `/dav/*` needs a block [CB N3, IR].
* Compose details: `depends_on: {migrate: {condition:
  service_completed_successfully}}`, remove `nats` from the anchor and
  ingest, replace `torza:local`, add `TORZA_TAG` to `.env.example` [CB N4].
* Key prefixes: `tza_api_` leaves 4 random characters in
  `DisplayPrefix` (keys.go:33-38) — raise to 16; `session`/`telegram` have
  no prefix [CB N7, ST, IR].
* `fakesource` never registered in `cmd/ingest`; `INGEST_SOURCES` rejects
  `fake` [CB N8].
* Signed-link token format and `SIGNING_KEY_ID` format [IR].
* Priority never lowered — state it's intended [IR].
* `/v1/admin/import` before stage 5 returns 501 [ST].
* `POST /v1/credentials` should create `stremio` and `webdav` kinds [ST].
* Entry 49a numbering; `feed_backlog` columns and section 2 entry; "owner"
  = `users.is_admin`; one placeholder style (`<item id>`, `<file id>`);
  "the VPS" vs "the server"; vague verbs ("wire", "wired into"), units
  (MB vs MiB), named lock keys, CORS header value [ST].
