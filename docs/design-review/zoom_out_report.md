# Stage 5 — Initial Zoom Out Review

* **Target:** `docs/designs/torza.md` (lines 1–1303)
* **Reviewer:** egm-zoom-out (fresh context), standard: zoom-out-standard
* **Context read:** concepts_brief.md, ARCHITECTURE.md, README.md, the current Go code, deploy files, CI workflow
* **Date:** 2026-11-13

**Verdict:** Needs changes, no blockers. The design attacks the root
problem (the info hash does every job; the code is shaped around
qBittorrent), and its core moves hold up: items plus source keys, stable
file rows, one service layer, no NATS, cloud storage only as an export
target. The main problem is that stage 1 builds parts that only matter for
later stages or for running more than one server. There are also three
internal contradictions: "kind" means three things, the purge job needs
source credentials it isn't allowed to have, and RSS makes an exception to
the "one global eviction rule".

## Major

### M1. Stage 1 builds too much before the first deploy
The brief warned against "too much untested code before anything has run"
(concepts_brief.md:121-126), yet stage 1 is 34 entries. Many serve later
stages: the `stremio`/`webdav`/`session`/`telegram` credential kinds and
prefixes, `user_limits.rd_fallback_after`, `fetch_attempts`, an
`/v1/admin/import` route that does nothing until stage 5, Usenet disk
multipliers, live-reader rows, per-user bandwidth metrics, a five-way
config split, and a separate `migrate` program. Migrations only move
forward after the first deploy (lines 284-287), so adding a column in the
stage that needs it is one small migration, not a rewrite.
**Change:** shrink stage 1 to what a torrent-only deploy needs (items,
source keys, file rows, library, state table, Source interface with
qBittorrent and the fake two-step source, finish order, service layer with
Add/Play/Block/Authorize, API-only credentials, the four bug tests). Move
everything else into the stage that first uses it.
*Evidence:* lines 736-1026, 292-298, 959-960, 878-883, 971-992.

### M2. "Kind" has three meanings, so the fallback path is unclear
Key kinds (torrent, nzb, hoster), loop kinds (torrent, http, usenet — the
`-sources` values, `items.source_kind`, the wake-up channels, the
multipliers) and fetchers (qbittorrent, realdebrid, torbox, hoster,
usenet — the registry keys) are mixed. `Source.Kind()` doesn't say which.
For CUJ 5, "cancel qBittorrent and start an RD fetch" either runs RD in the
torrent loop or needs a `source_kind` change and re-queue that the state
table doesn't describe.
**Change:** use two words only — *key kind* (matching) and *fetcher*
(filling). Replace `source_kind` with `items.fetcher`; key loops,
`-sources`, the claim query, channels and multipliers by fetcher. Fallback
is one state move: end the qbittorrent attempt, set `fetcher=realdebrid`,
re-queue. `Kind()` returns the fetcher name.
*Evidence:* lines 235, 294, 298, 304-313, 339, 364-375, 386, 841-843, 881, 888-889.

### M3. The purge job needs source credentials it isn't allowed to have
`scheduler/purge.go` cancels running fetches "through the registry", so
`scheduler` would need every provider's credentials — contradicting "only
`ingest` holds source credentials" — and cancels outside the process that
holds the item.
**Change:** purge only deletes files and sets `purged_at` for items no
worker holds. When an item becomes `blocked`, the ingest loop's next
guarded write fails and ingest calls `Cancel(deleteData=true)` itself.
*Evidence:* line 933 vs lines 278, 645-646, 431-432.

### M4. Expiring leases solve a multi-server problem the design rules out
With one ingest container per fetcher and "more than one server" a
non-goal, crash recovery needs no expiring leases: on startup a loop moves
its own `active` items back to `queued`, keeping the Handle. Leases add
`lease_until`, renewal on every Poll, `ReleaseExpired`, `LEASE_DURATION`,
a metric and stale-worker tests, and make recovery wait for expiry. The
status guard (`WHERE status='active'`) already stops stale writes after a
block or eviction.
**Change:** keep `FOR UPDATE SKIP LOCKED` claiming; replace expiring leases
with startup recovery plus status-guarded writes; list leases under Future
Improvements next to "More than one server".
*Evidence:* lines 154-155, 377-398, 788-790, 806-808, 976, 982-983; internal/db/repo.go:193-195.

### M5. Two mechanisms do one job in eviction
A `readers` table with heartbeats plus `last_used_at` updated after 50 MB
or 2 minutes; eviction checks both. One rule covers it for ~10 users:
while bytes flow, `stream` refreshes `last_used_at` once a minute after the
threshold, and eviction skips items used within `EVICT_IDLE_FOR` (already
in internal/scheduler/evict.go:32,82). Neither protects a paused player.
**Change:** drop `readers`; "in use" means `last_used_at` refreshed while
bytes flow; export refreshes it while copying.
*Evidence:* lines 301, 458-465, 926-928, 966-969, 1142, 1268-1269, 515-516.

### M6. Disk-reservation multipliers arrive before the source that needs them
Stage 1 adds per-kind multipliers (Usenet 2.2), a no-metadata placeholder
size, `reserved_bytes`, `usage.go` and `reserved_fits(...)` in the claim
SQL. Only Usenet (stage 9) needs the multiplier, and the in-SQL check —
summing reservations across rows claimed in one `LIMIT $n SKIP LOCKED`
statement — is left unspecified (line 398).
**Change:** in stage 1, check free space plus the item's known size in Go
before claiming one item; add `reserved_bytes` and multipliers in stage 9.
*Evidence:* lines 386, 398, 453-456, 653-655, 801, 878-883, 1179-1180.

### M7. RSS guards overlap and break the stated eviction rule
RSS adds a `feed_backlog` table, a daily byte cap per feed, and "RSS-added
items nobody has opened go first" in eviction. The last contradicts "one
global rule". The backlog isn't needed (leave the entry unseen and retry
next poll); the byte cap duplicates user limits; `feed_backlog` is missing
from section 2.
**Change:** keep `feeds` and `feed_entries_seen`; mark an entry seen only
once added; rely on user limits and lowest priority. Drop the backlog, the
byte cap and the eviction exception, or justify the exception next to the
non-goal.
*Evidence:* lines 152-153, 551-561, 1206-1223, 302.

## Nit

* **N1.** `Store` interface has one implementation and no planned second
  (lines 439-451, 869-876, 150-151). Keep a concrete `store.Local`.
* **N2.** The signing key ID does nothing while only the current key is
  accepted (lines 469-472, 950-952, 977); links already carry `v1`
  (internal/links/links.go:27). Drop it until older keys are accepted.
* **N3.** No separate `migrate` program needed: migrations already use an
  advisory lock (internal/db/db.go:45-61) and `api` already has
  subcommands (README.md:84-86). Use `api migrate` (lines 237, 986-992, 1009-1010).
* **N4.** LISTEN/NOTIFY may be unneeded: with a 5 s poll
  (internal/config/config.go:60) and downloads taking minutes, wake-ups save
  seconds but cost a dedicated reconnecting connection and channel naming.
  Consider timer polling only (lines 239-246, 768-770, 888-889, 1254).
* **N5.** A Handle can't hold changing state (Poll returns a Report, not a
  new Handle), yet the RD/TorBox Handle holds "per-file download state".
  Take progress from file sizes under `incoming/<id>/` (lines 341-355, 1096-1097).
* **N6.** Data-model gaps: no table for bytes served per user per day
  (lines 656-657, 969, 984); `feed_backlog` missing; `provider_accounts`
  listed both in stage 1 and stage 5 (lines 299 vs 302, 757); both a
  `-sources` flag and `INGEST_SOURCES` (lines 235, 976).
* **N7.** Owner accounts could be an `ingest` config block instead of a
  `provider_accounts` table with `secret_ref` indirection (lines 299, 1098-1105).
* **N8.** Use existing Go packages for the release-name parser and the
  SSRF-safe HTTP client (lines 1190-1200), as already done for bencode and
  WebDAV.
* **N9.** Mark the file lists for stages 9–13 "provisional; re-plan at
  stage start" (lines 1160-1285).
