# Stage 6 — Comprehension Test

* **Target:** `docs/designs/torza.md` — FeatureRange 1–678, DetailedRange 679–1303
* **Reviewer:** egm-comprehension (fresh context), standard: re-implementation-standard
* **Context read:** the design, concepts_brief.md, ARCHITECTURE.md, README.md, the current Go code, migration, deploy files, Dockerfile, go.mod, CI workflow. Not readable: `~/reports/Debrid service competitive teardown.md` (outside the repo).
* **Date:** 2026-11-13

## Verdict

**No — a reader with no other context could not rebuild the system from
this document.** The feature range (1–678) holds together: items and
source keys, leases, the finish order, the service layer and the store
interface are motivated. The detailed plan (679–1303) is mostly a file
list with one-line intents and leaves out most behavioural contracts:
REST and Stremio request/response shapes and status codes, how users and
credentials are created, what "kind" means, where the Handle and current
fetcher are stored, retry and failure rules, and the placeholder size for
disk reservations. Four internal contradictions would make two readers
build different things (RD file-set check, purge credentials, recovery
from `finalizing`, smoke test vs. stage 2 done). Stages 4–13 are outlines.

## Blocker

### B1. No contract for the replacement REST API
Entry 31 (957–961) names routes (`/v1/items`, `/v1/items/{id}`,
`/v1/admin/block`, `/v1/admin/limits`, `/v1/admin/import`,
`/play/{file id}`; `POST /v1/credentials` at 1083) but no request bodies,
response JSON or status codes. Unanswered: block by item ID, source key or
raw input? `.torrent` upload as multipart or base64? What `/play` returns
for queued/evicted items (202, 409, redirect)? Size-limit status code? Do
`GET /v1/me` and `POST /v1/files/{id}/link` survive
(internal/api/server.go:67,72)? Is there a DELETE for library removal?
Today's contract is written down (README.md:46-66, ARCHITECTURE.md:50-84);
the design drops it, and `scripts/smoke.sh` (724) depends on it.
**Change:** a route table — method, path, auth kind, body, response,
every status code (at least 400, 401, 403, 404, 409, 429, 451, size limit).

### B2. No way to create the first user or any credential
`users` loses `api_key_hash` (291), which today's `api create-user`
writes (cmd/api/main.go:42-48, internal/db/repo.go:22-30); nothing
replaces it. "Invite link" (479, 1156) has no table, route, expiry or
single-use rule. `POST /v1/credentials` only appears in stage 4, yet the
stage 2 smoke test needs two users' API keys (724).
**Change:** a bootstrapping section: CLI subcommand (program, flags,
output), invite table and flow, which credential kinds a user can mint and
through which route.

### B3. "Kind" has three vocabularies; dispatch fields missing from `items`
Ingest kinds (torrent/http/usenet: 235, 881), source-key kinds
(torrent/nzb/hoster: 306-313) and fetcher names (qbittorrent, realdebrid,
…: 298, 364; registry key 842) are all "kind". `Source.Kind()` (339)
doesn't say which. `items.source_kind` = "kind of the current attempt"
(294) drives the claim query (386) and `torza_queued_<kind>` (241). `items`
has no fetcher column and no Handle column, though the Handle is "stored on
the item" (346). On RD fallback (371), does `source_kind` change so the
http loop claims it? Who sets it in `service.Add`?
**Change:** one glossary mapping key kind, ingest kind and fetcher; add
`fetcher` and `handle bytea` to `items`; `Kind()` returns the fetcher;
spell out the fallback steps (cancel, set fetcher and ingest kind, clear
Handle, re-queue or keep claim, notify).

### B4. An RD import can't satisfy the file-set match that gates it
An import is ready "only if its file set matches what the torrent lists"
(374, entry 51), but fallback fires exactly when no metadata has arrived
(370-371, CUJ 5), so there is no list to compare. The mismatch outcome is
also missing: no `finalizing → failed` transition (324).
**Change:** say what the import is compared against (uploaded `.torrent`
metainfo, RD's own list, or no check without a list) and add the mismatch
outcome to the state table.

### B5. An item can get stuck in `finalizing`
Step 2 calls `Finalize`, which removes the torrent from qBittorrent
(403-404). A crash before step 3's rename leaves `incoming/<id>` but no
`store/<id>`; the repair at 412 only handles an existing `store/<id>`;
calling `Finalize` again fails because qBittorrent no longer has it;
`finalizing` can only go to `ready` or `blocked` (324); `ReleaseExpired`
only covers `active` (395). The item stays `finalizing` forever,
contradicting 80-82 and CUJ 4.
**Change:** make `Finalize` idempotent and rebuildable from the Handle and
`incoming/` (add to the four rules at 352-362 and the contract suite,
entry 17), or store the manifest in the database before the source
releases files; state what happens when a `finalizing` item's lease
expires.

### B6. Purge runs in `scheduler`, but only `ingest` has source credentials
Entry 27 (934) cancels running fetches "through the registry" from
`scheduler`; lines 278 and 645 say only `ingest` holds credentials. Today
purge runs in ingest (internal/ingest/worker.go:80-101).
**Change:** pick one — e.g. `scheduler` deletes files and `ingest` cancels
claimed items that turn `blocked` — and say how the worker learns of the
block (guard conflict or notify).

## Major

* **M1. Failure, retry and timeout rules missing.** `ErrClass` (349) has
  no retry count, backoff or point of becoming `failed`; entry 24 (907)
  "fails" without conditions; `METADATA_TIMEOUT` (2 h) and
  `MAX_TORRENT_BYTES` (internal/config/config.go:58-59,
  ARCHITECTURE.md:145-151) vanish; whose `max_item_bytes` applies after
  metadata arrives, what happens to other users' rows, what error users
  see. **Change:** a fetch-policy decision table and the replacement
  settings.
* **M2. Disk-reservation values undefined; claims can deadlock with
  eviction.** No placeholder value (882), no update rule when the real
  size arrives, no estimate for magnets or NZBs; cache-limit and min-free
  settings (455, 655) missing from entry 33 (today `CACHE_MAX_BYTES`,
  `CACHE_MIN_FREE_BYTES`). Eviction runs only "when over the limit" (458)
  but claims are refused when ready+reserved+new would pass it (455), so
  the queue can stall with no eviction. **Change:** values, update rule,
  config names, and whether eviction also frees space for queued items.
* **M3. `EVICT_IDLE_FOR` and `POLL_INTERVAL` "added" but already exist**
  (entry 33/977, 38/1011; internal/config/config.go:60,63). With `readers`
  replacing idle protection (464, ARCHITECTURE.md:130), does
  `EVICT_IDLE_FOR` still apply? **Change:** list kept/renamed/removed
  variables and defaults for new ones (`LEASE_DURATION`,
  `INGEST_SOURCES`, `SIGNING_KEY_ID`); settle `INGEST_SOURCES` vs
  `-sources=` (235).
* **M4. Priority semantics contradict.** `items.priority` = highest among
  holders (395-397) vs RSS adds "at the lowest priority" (218, entry 63).
  Type, range and defaults of `priority`; defaults when a user has no
  `user_limits` row; the "active download" predicate (424). **Change:**
  define them.
* **M5. Play, re-queue and limits interact undefined.** Does a Play
  re-queue (430, CUJ 2) pass Add's limit check, and whose `started_by`
  does it take? Does re-queue reset phase, Handle, `fetch_attempts`? On
  library removal (427), does an active item the user started keep
  counting? **Change:** state the rules in section 7.
* **M6. `/play` auth for Stremio unspecified.** Credentials are in the
  path for `/stremio/<credential>/…` (1074), but `/play/<file id>` (1076,
  475) has no credential slot and players can't send headers. **Change:**
  fix the URL form (e.g. `/play/<credential>/<file id>`), accepted kinds,
  and not-ready/blocked responses.
* **M7. Signed-link format and stream route inconsistent.** Diagram says
  `/dl` (254); today it's `/d/{token}/{name}` (internal/stream/server.go:44,
  deploy/Caddyfile:4). Where the key ID goes in
  `v1.<fileID>.<userID>.<exp>.<sig>` (internal/links/links.go:8), `v2`?
  `SIGNING_KEY_ID` format, `LINK_TTL` kept? **Change:** exact token
  grammar and path, diagram consistent.
* **M8. Credential details missing for three kinds.** Prefixes only for
  api/stremio/webdav (945); `session` and `telegram` (292) not covered;
  Telegram lookup by chat ID — hashed? what's in `hash`/`prefix`? Session
  lifetime, cookie name, CSRF mechanism (479, 1157). **Change:** a table
  per kind: generation, storage, presentation, lifetime.
* **M9. `readers` and use tracking vague.** "50 MB or 2 minutes" (463,
  969) per request, reader or cumulative? `reader_id`, table key, write
  cadence, whether PROPFIND/HEAD write rows. **Change:** define the reader
  lifecycle.
* **M10. NZB storage undefined.** `item_sources.payload` holds "an NZB
  file reference" (295) — stored where, size limit, survives eviction?
  `Payload` (825) and `Job` (340) never defined. **Change:** define both
  and storage per payload kind.
* **M11. RD vs TorBox selection and TorBox Usenet/web items.** "an RD (or
  TorBox) fetch" (371) without a rule; `sourcekey.Parse` (825) can't tell
  "fetch NZB with TorBox" from "with own Usenet client" (312). **Change:**
  a selection rule and how the owner picks a fetcher (import route body).
* **M12. "Six steps" and hoster list undefined here.** 492 and 1097 refer
  to six steps listed only in concepts_brief.md:260; stage 6 names no
  hosts (1132). **Change:** inline the steps; list supported hosters and
  URL forms.
* **M13. Smoke test vs stage 2 done.** Stage 2 requires an eviction check
  at 20 GB (1032); `smoke.sh` steps (724-728) have none. Which key is
  admin for the block step, which test torrent, which health endpoint?
  **Change:** add the eviction step and name them.
* **M14. Bytes served per user per day has no storage** (656, entries
  32, 34); an in-memory counter resets on restart. `ingest` and
  `scheduler` have no HTTP listener today, yet entry 35 mounts `/metrics`
  and `/readyz` on all four. **Change:** storage and listener config.
* **M15. Stages 4, 7, 10, 11, 13 lack contracts.** Stremio manifest,
  catalog type, meta/video shape, not-ready display; WebDAV layout and name
  collisions; RSS filter schema, poll interval, byte cap with unknown
  sizes, `feed_backlog` missing from section 2 (302, 1213), detecting
  "unopened" RSS items (1223); catalog API never named (114; brief says
  TMDB) and key config; export encryption algorithm and key variable, and
  which limit it counts against (1269). **Change:** a short contract per
  stage, or defer each to its own design.
* **M16. Block-vs-add race prevention unexplained.** CUJ 3 and the test at
  920 require it; section 7 says only "one transaction"; Add locks
  `user_id` (424), which doesn't serialise against Block. What Block does
  for a not-yet-existing item (which key, name, payload); how an `active`
  worker notices. **Change:** describe the locking or unique-constraint
  ordering; give Block's input type.

## Nit

* **N1.** `provider_accounts` listed in the main table (299) and as stage 5 (302).
* **N2.** Diagram (254) omits `/play` and `/telegram` for `api`; `/ui` never defined.
* **N3.** `ingest-usenet` (495) is a container, not a program; two-image deploy and tagging not described (entry 60).
* **N4.** `Store.Publish` when both `incoming/<id>` and `store/<id>` exist (405, entry 20).
* **N5.** `Store.Open` returns no mod time; ETag and `ServeContent` use it (internal/stream/server.go:106-108).
* **N6.** dbtest rationale (695) muddled — the advisory lock serialises, it doesn't conflict; `search_path` must be set in pool connection config, not `SET`.
* **N7.** LISTEN/NOTIFY payload format and the sender of `torza_item_ready` undefined.
* **N8.** `deploy.sh` "pulls or builds" (1018) with no registry named.
* **N9.** `/healthz` vs `/readyz` (992; today internal/api/server.go:66).
* **N10.** Undefined: "release type", "TryAnotherFetcher", `reserved_fits` (placeholder only), qBittorrent state → phase mapping (entry 18), "test user" vs "owner" (= `is_admin`?).
* **N11.** `~/reports/...` (666) is outside the repo; inline what matters or drop it.
* **N12.** "The existing 52 tests" (522) can't be checked from the document.
