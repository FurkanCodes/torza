# Stage 8 — Intermediate Zoom Out Review

* **Target:** `docs/designs/torza.md` (full file, 1303 lines; not edited since stages 5–7)
* **Inputs:** zoom_out_report.md (stage 5), comprehension_report.md (stage 6), critic_report.md (stage 7)
* **Reviewer:** egm-zoom-out (fresh context), standard: zoom-out-standard
* **Date:** 2026-11-13

**Verdict:** Needs changes before stage 1; no new blockers. The ~60
findings across stages 5–7 come from **six root causes**, and each has a
fix that makes the design *smaller*. The main risk is applying stages 6
and 7 by *adding* mechanisms (reconciler, two-phase reservations, reader
heartbeats, more tables, contracts for every stage) when cutting them from
stage 1 is simpler. Where stage 5 says "cut" and stages 6/7 say "specify",
side with cut-and-defer. Stage 7 M9 is mostly wrong on the facts.

## 1. Root causes

**R1. No single "who fills this item" field.** (S5 M2, S6 B3, M11, S7 B1,
M10, part of S6 M1.) Fix: `items.fetcher` and `items.handle bytea`; loops,
the flag (rename `-sources` → `-fetchers`), claim filter and wake-up
channels keyed by fetcher; drop "loop kind" (a container runs a list of
fetchers; `ingest-usenet` runs `usenet`); `Kind()` returns the fetcher.
Fallback is one transaction (Cancel qBittorrent, close attempt, set
fetcher, clear handle, `status=queued`, notify) and one state event
(`Switched`: active→queued). `rd_fallback_after` and the old global
`METADATA_TIMEOUT` (worker.go:172) become two rows of one decision table in
`policy.go`.

**R2. Nobody owns an item while a worker holds it.** (S5 M3, S6 B6, M16,
S7 M2.) Rule: an item with a non-null `handle` belongs to `ingest`. Worker
writes guarded by `WHERE status IN ('active','finalizing') AND fetcher=…`;
on guard failure the worker calls `Cancel(deleteData=true)` and
`Store.Remove` itself (as ingest already does, worker.go:80-101);
`scheduler` purges only handle-less items. Add and Block both take
`pg_advisory_xact_lock` on a hash of each source key, serialising the CUJ 3
race.

**R3. The manifest isn't stored before the source releases its files.**
(S6 B5, S7 M3, M4, S5 M4, the lease-reaper nit.) Fix: call `Finalize` for
the manifest, then in one transaction upsert `item_files` (state
`missing`) and set `finalizing`; only then the source releases files.
Recovery reads the database: `store/<id>` exists → ready; only
`incoming/<id>` → rename, ready; neither → `queued`. Adds
finalizing→queued and finalizing→failed (Permanent) only. No Source rule 5,
no `.manifest.json`.

**R4. Disk reservation runs before the size is known.** (S5 M6, S6 M2, S7
B2.) Magnets have no size at claim time — the cause of the placeholder
question, the size check that can't run at Add, and the claim/eviction
deadlock. Simpler version in section 3.

**R5. One start path assumed, four exist.** (S6 M4, M5, S7 M6, B2a.) Play
re-queues, RSS and Telegram add, fallback restarts. Fix: one
`service.Enqueue(user, item, via)` for every move to `queued`: counts
`max_active` under the user lock, sets `started_by` and `priority` from the
enqueuing user (RSS uses 0). Size check stays in ingest after metadata,
against `started_by`'s `max_item_bytes` (worker.go:176). Priority: set when
queued; a later add while queued raises it with `GREATEST`; never lowered.

**R6. Stage 1 carries later-stage scope.** (S5 M1, N2, N3, N6, S6 B2
invites, M6–M9, M14, S7 M5, M8.) Most stage 1 "missing contract" findings
are for features stage 1 doesn't need: session/telegram credentials,
invites, `/play`, `readers`, per-user bandwidth, signing key ID,
`/v1/admin/import`. Remove them; each returns with its stage and a forward
migration (284-287).

## 2. Conflicts between reports

| Conflict | Sides | Call |
|---|---|---|
| Expiring leases | S5 M4 removes; S6 B5, S7 B1/M2/reaper extend | **S5.** One container per fetcher; startup recovery (own `active` → `queued`, keep handle) + R3 + status-guarded writes cover every crash. Drops `lease_until`, renewal, `ReleaseExpired`, `LEASE_DURATION`, lease metric, stale-lease test. Keep the atomic claim and `SKIP LOCKED` (free) so bug 4's disjoint-claims test holds. |
| Live readers | S5 M5 drops `readers`; S6 M9, S7 M8 define it further | **S5, with S7's pause point.** Reuse `TouchTorrent` (repo.go:181-187, throttled to once a minute) and `EvictIdleFor` (evict.go:32,82); fire from stream's write path after the first N MB, never on HEAD/PROPFIND; raise `EVICT_IDLE_FOR` default from 15 min to a few hours. |
| RSS safeguards | S5 M7 drops backlog, byte cap, evict-RSS-first; S7 M9 says "dropped without record" | **S5.** S7 M9 is wrong: the cap is at 1208, evict-RSS-first at 1223. Remove both; record one line as Alternative N ("evict RSS first", rejected for the global LRU rule at 152). |
| Signed-link key ID | S5 N2 drops; S6 M7 asks where it goes | **Drop.** Only the current key is accepted (469-472); `v1.` already versions the format. Removes `SIGNING_KEY_ID`; only the `/dl`→`/d` diagram fix remains. |
| Credentials, invites in stage 1 | S5 M1 api-only; S6 B2, M8, S7 M5 want invite/telegram tables | **S5 for scope, S6 B2 for bootstrapping.** `credentials` keeps a `kind` column, only `api` keys in stage 1; bootstrap via existing `api create-user` (cmd/api/main.go:42) writing a credential row. Invites in stage 8, telegram links in stage 12. |
| Per-user bytes served | S6 M14, S7 M5 add `bandwidth_daily`; S5 M1 defers | **Defer.** The VPS dashboard shows monthly traffic; if kept, one upsert when a response closes. |
| `/play` route | S6 M6 wants auth for `/play/<file id>` in stage 1 (entry 31) | **Neither.** Keep `POST /v1/files/{id}/link` in stage 1; add play in stage 4 as `/stremio/<credential>/play/<file id>`. |
| Store interface | S5 N1 concrete; brief §15 decided interface | **Concrete `store.Local`** (nit); keeping the interface costs little. |
| `migrate` program | S5 N3 `api migrate`; S7 nits accept five programs | **`api migrate`** (nit). |
| Risks section | S7 M1 full capacity + risk tables | Required; keep to ~8 one-line rows plus three numbers (disk, cache limit, monthly traffic). |

## 3. Fixes heavier than needed

* **S7 B2 (two-phase reservation, lock, demand eviction, starvation):**
  stage 1 has no `reserved_bytes`, multipliers or `reserved_fits`.
  Eviction counts "used" as ready bytes plus known sizes of `active` items,
  so claims and eviction use the same number and can't deadlock. Ingest
  stops claiming while `statfs` free space is below the minimum
  (backstop). NFR (653-655) becomes "best effort". Multipliers and one
  advisory lock arrive in stage 9.
* **S7 M3 periodic reconciler:** R3 recovery + startup recovery + one
  scheduler sweep deleting `store/` and `incoming/` folders whose item
  isn't active, finalizing or ready. At startup, `qbitsource` removes
  torrents in Torza's category with no active item. Rollback = restore the
  dump, the sweep runs.
* **S7 M3 kill-and-restart goal:** agreed; test the three R3 disk cases
  plus a mid-`active` restart, nothing more.
* **S7 M4 RD verification by qBittorrent recheck:** stage 5 work. Drop the
  file-set match (S6 B4 shows it can't run during fallback), record the
  fetcher in `fetch_attempts`, and change the goal (93-94) to "same item
  key", not "same bytes"; recheck optional in stage 5.
* **S7 M7 (`os.Root`, `O_NOFOLLOW`, `7zz` sandbox, fuzzing):** stage 1 only
  opens through `os.Root` (go 1.26 in go.mod), keeps `filepath.IsLocal`
  (store.go:57), and `Finalize` rejects non-regular files. `7zz` and
  fuzzing go to stages 3 and 9, one line each.
* **S6 B1 full route table:** only stage 1's routes, starting from
  README.md:46-66 with `/v1/items` paths; keep `/v1/me`, `DELETE`,
  `POST /v1/files/{id}/link`, 429/451/410. About 8 rows; `smoke.sh` needs
  exactly this.
* **S6 M15 contracts for stages 4, 7, 10, 11, 13:** mark those entries
  "provisional; each stage gets a short design at its start" (with S5 N9).
* **S6 M8 per-kind credential table:** one sentence for `api`; others in
  their stages.
* **S7 B1 `items.current_attempt_id`:** use two plain columns on `items`
  instead (no join, no foreign-key cycle).
* **S7 M6 recompute priority:** never lower it (R5).
* **S6 M3 full config list:** apply the cuts first; `LEASE_DURATION` and
  `SIGNING_KEY_ID` disappear, `EVICT_IDLE_FOR` and `POLL_INTERVAL` exist,
  `-fetchers` replaces `-sources` and `INGEST_SOURCES` — about 3 new names.
* **S5 M5 follow-up for scanners:** one threshold ("touch after the first
  N MB written in a response"), not "50 MB or 2 minutes"; no touch on HEAD
  or PROPFIND.

## 4. Ordered must-fix list before stage 1

1. **Fetcher model (R1):** `items.fetcher`, `items.handle`, `-fetchers`,
   `Switched` event, fallback decision table including the metadata
   timeout. (S5 M2, S6 B3/M1/M11, S7 B1)
2. **Finish order and recovery (R3) instead of leases (S5 M4):** manifest
   before release, three disk cases, finalizing→queued/failed, startup
   recovery, orphan sweep. (S6 B5, S7 M3/M4, S5 M4)
3. **Item ownership and block race (R2):** status-guarded writes, ingest
   purges anything with a handle, per-source-key advisory lock in Add and
   Block. (S5 M3, S6 B6/M16, S7 M2)
4. **Disk (R4):** no reservations in stage 1, size check after metadata,
   eviction counts active bytes, `statfs` backstop, NFR reworded. (S5 M6,
   S6 M2, S7 B2)
5. **One `Enqueue` path (R5)** with the one-sentence priority rule. (S6
   M4/M5, S7 M6)
6. **Stage 1 route table and bootstrapping** (S6 B1/B2), small as above.
7. **Cut stage 1 scope:** no `readers`, `/play`, `/v1/admin/import`, key
   ID, per-user bandwidth, `provider_accounts`, `migrate` program or
   five-way config split (two structs: HTTP programs, workers); only `api`
   credentials.
8. **`os.Root` in `Store.Open`**, `Finalize` rejects non-regular files
   (S7 M7, stage 1 part).
9. **Smoke test matches stage 2 done** (S6 M13): eviction check with a
   small `CACHE_MAX_BYTES` and two small legal torrents, not a 20 GB fill.
10. **Short Risks & Limits section** (S7 M1), ~8 rows.

## 5. New observations

* **S7 M9 is mostly wrong on the facts:** byte cap at 1208, evict-RSS-first
  at 1223, the "from the docs, not yet checked" marking at 712-713 and
  1110-1111, the Telegram play-links note at 107. Only `.torrent` support
  in RSS is missing. Don't re-add what is there.
* **S7's migration-rename nit is already handled:** entry 9 (766-768)
  refuses a database that recorded an unknown migration.
* **Existing recovery the design silently loses:** worker.go:151-156
  re-adds a torrent qBittorrent lost, and qBittorrent rechecks the data on
  disk — the natural way to meet Source rule 1 for `qbitsource`; name it in
  entry 18. It's also what turns S6 B5 into a re-download if `Finalize` runs
  before the manifest is stored — another reason for R3's ordering.
* **The block-vs-add "run 100 times" test (920-921) is flaky by design.**
  With R2's per-key lock it can be deterministic: hold an Add transaction,
  run Block on a second connection, assert it waits, commit, assert
  `blocked`; repeat with the order swapped.
* **`claimed_by` is probably unnecessary once leases go:** status plus
  fetcher identifies the owner; keep it only as a debugging label, not in
  write guards.
* **BitTorrent v2/hybrid:** stage 1 accepts v1 info hashes only and rejects
  v2-only magnets with a 400; registering both hashes of hybrids can wait.
* **Telegram `notify.go` (1254)** is the only consumer of
  `torza_item_ready`; send that NOTIFY in R3's "mark ready" transaction in
  stage 1 so it needs no retrofit.
