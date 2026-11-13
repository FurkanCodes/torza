# Stage 7 — Critic Review

* **Target:** `docs/designs/torza.md`, FeatureRange lines 1–678
* **Reviewers (fresh context, in parallel):** egm-design (feature-spec-standard), egm-structural (feature-spec-standard), egm-style (persona rules)
* **Date:** 2026-11-13

## Verdicts

* **Design:** needs revision, not ready to approve. Two blockers in the
  core of stage 1: moving an item between fetchers and loops (CUJ 5 can't
  be built), and disk reservation with eviction (a full cache can stall
  forever). No Risks & Mitigations section.
* **Structural:** revise before approval, no blockers. Alternatives rule
  met (A–M); Risks & Mitigations section missing; data model misses state
  the text relies on; terms mean different things in different sections.
* **Style:** approve with changes. Prose is plain; terms drift between
  sections, a few promises are absolute where the mechanism can't keep them.

Findings below are merged and deduplicated; the reviewers that raised each
one are in brackets.

## 🔴 Blocker

### B1. "Kind", fetcher, loop and Handle are mixed; the fetch policy can't be built [design, structural]
Three lists are all "kind": loop kinds `torrent,http,usenet` (235),
key kinds `torrent|nzb|hoster` (304-313), fetchers `qbittorrent|realdebrid|
torbox|hoster|usenet` (298, 364). `Source.Kind()` (339) and
`items.source_kind` ("kind of the current attempt", 294; claim filter 386;
channel 241) could mean any. An RD import of a torrent item has key kind
`torrent` but runs on the http downloader — which loop claims it, and who
rewrites `source_kind`? `items` has no `handle` or current-fetcher column,
though the Handle is "stored on the item" (346, 394-395); a restarted
worker can't tell which Source to Poll.
**Fix:** one glossary (key kind, fetcher, loop kind) with a fetcher → loop
kind table; `Kind()` returns the fetcher; add `items.fetcher` and
`items.handle bytea` (or keep them on the current `fetch_attempts` row via
`items.current_attempt_id`); write the fallback hand-over as one
transaction (cancel qBittorrent → close attempt → set fetcher and loop
kind, clear handle, `status=queued` → notify); add the `active→queued`
(fetcher switch) event to the state table.

### B2. Disk reservation doesn't work for magnets, and claims can deadlock against eviction [design; style 7]
(a) A magnet has no size until metadata arrives, after the claim (and
never, in CUJ 5); `max_item_bytes` "when an add starts" (119-121) can't be
checked at Add time — today it's checked in ingest after metadata
(internal/ingest/worker.go:176). (b) Claims are refused when
ready+reserved+new passes the limit (455-456) but eviction runs only "when
over the limit" (458): a cache at 95% with a queued 10% item stalls on
every tick. (c) `SKIP LOCKED` locks rows, not the byte budget; the torrent
loop and the Usenet container can reserve the same free bytes, so the NFR
"never fills the disk past the minimum free space" (653-655) has nothing
enforcing it. (d) On Linux, deleting a file a reader still holds open
doesn't free its space (raised in the brief §7, not addressed).
**Fix:** two-phase reservation (provisional on claim; real once
`BytesTotal` is known, then the `max_item_bytes` check → `failed` if too
large, back to `queued` with reservation released if no room); serialize
reservation changes under one advisory lock; eviction targets
ready+reserved+head-of-queue demand; trigger on database-tracked bytes,
`statfs` only as a backstop; state whether a large head-of-queue item
blocks smaller ones and how starvation is avoided. Reword the NFR so it
doesn't promise more than estimates allow.

## 🟡 Major

### M1. No Risks & Mitigations section; no capacity or cost numbers [design, structural]
The standard requires it. Risks scattered or missing: owner's RD/TorBox/
hoster accounts banned or out of quota (datacenter IP, use for others —
check provider terms before stage 5); fakes built from docs not matching
real services; PAR2 using all CPU for hours; VPS bandwidth; VPS provider
suspending the server over torrent traffic or abuse reports; qBittorrent's
open port; single node, single disk, no cache backup; Postgres as a single
point of failure; signing-key compromise; the takedown race; crash safety.
No numbers for disk, cache limit, largest item, monthly traffic, CPU/RAM
for PAR2 and 7zz beside Postgres and qBittorrent, or monthly cost.
**Fix:** a `## Risks & Limits` section after Alternatives: a capacity
table and a Risk / Impact / Mitigation / Stage table pointing at the
section that handles each.

### M2. Purge conflicts with credential isolation, and blocked items keep being worked on [design]
Purge runs in `scheduler` (236) but only `ingest` holds source credentials
(278); deleting `incoming/<id>` under qBittorrent causes `missingFiles` or
recreated files. `active→blocked` doesn't stop the worker: writes are
guarded only by `claimed_by` (392-393), so it can keep writing progress or
finalize a blocked item. The brief (§10) said blocked content is deleted
through each source's own code.
**Fix:** ingest purges blocked items through `Cancel(h, deleteData=true)`
then `Store.Remove`; scheduler purges only items with no Handle (ready,
evicted); guard every worker write with `AND status IN
('active','finalizing') AND claimed_by=$worker`; Block wakes the right loop.

### M3. Crash safety has no disk-to-database reconciliation [design; style 6]
Only finalize has a recovery rule. Eviction marks evicted then deletes
files — a crash between leaves orphan bytes forever; purge and failed-
download cleanup likewise. Restoring a `pg_dump` (513-514, 658-660) leaves
folders with no rows, `ready` rows with missing folders, and unknown
qBittorrent torrents. A crash after `Finalize` released the torrent but
before the rename needs `Finalize` to run again with no qBittorrent state —
rule 1 (354) covers only `Poll`. Line 412 completes a `finalizing` item
"without calling the source again" but doesn't say where the manifest
comes from; walking the folder would pick up `.!qB` part files. The goal
"any process at any moment" (80-82) can't be tested as written.
**Fix:** a reconciler at startup and periodically (delete orphan folders
of items not active/finalizing/ready; mark ready-with-missing-folder as
evicted; remove qBittorrent torrents with no active item), also run after
a rollback; Source rule 5 "`Finalize` is idempotent after the source
released its files"; persist the manifest (e.g.
`incoming/<id>/.manifest.json` or a column) before step 3; reword the goal
as one kill-and-restart test per claim and finish step.

### M4. State table has no exits for failed finalize or mismatched imports [design, structural; style 10]
`finalizing` only goes to `ready` or `blocked` (320-328). Missing:
`Finalize` returns Permanent, the rename fails, neither `incoming/<id>` nor
`store/<id>` exists after a crash, an RD import's files don't match the
torrent (374-375) — and with fallback there is often no torrent list to
compare. RD bytes are never checked against piece hashes, yet the goal
(93-94) promises "the same item a qBittorrent download would".
**Fix:** add `finalizing→failed` and `finalizing→queued`; list the three
on-disk cases (store only, incoming only, neither) with an action each;
define the match source (metainfo RD returns, or fetch it) and the
mismatch outcome; either verify RD content (re-add to qBittorrent over the
data and force a recheck) or drop the "same item" guarantee and mark
imports unverified.

### M5. Data model misses columns and tables the text relies on [structural, style 2]
No `items.handle` (346); no storage for bytes served per user per day
(656); no tables for invite links (479) or Telegram link codes and chat
IDs (480-481); `user_items.source_id` (297) points at `item_sources`,
which has no `id`; `provider_accounts` listed in stage 1 (299) and stage 5
(302).
**Fix:** add `items.handle bytea`, `item_sources.id`,
`bandwidth_daily(user_id, day, bytes)`, `invites`, and `telegram_links`
(or a stated use of `credentials`); pick one stage for
`provider_accounts`; add a "stage added" column to the table list.

### M6. Re-queue paths skip limits; priority rules contradict [design, structural, style]
`Play` re-queues evicted items (429-430) without Add's limit check or
`started_by` — any user can restart someone else's large evicted item past
`max_active`/`max_item_bytes`, undercutting CUJ 7's "unless someone asks".
Goals say limits including priority apply only to starting adds (119-121),
but section 5 raises `items.priority` on non-starting adds; CUJ 7 adds RSS
items "at the lowest priority" while `items.priority` is the highest among
holders (395-397); priority is never lowered when library rows go.
**Fix:** route Play's re-queue through Add's limit branch (same advisory
lock, `started_by` = player, same 429-style refusal); say concurrency and
size limits apply to starting adds and priority is the highest among
holders; define how an RSS add ranks (e.g. `added_via='rss'` counts as 0
until a direct add) and match CUJ 7; state how priority is recomputed.

### M7. Untrusted content: symlinks, path traversal and decompression bombs [design]
Torrents (BEP 47 symlinks) and archives unpacked by `7zz` can create
symlinks or `../` paths. `stream` opens with `os.Open`, which follows
symlinks — a link to `/proc/self/environ` would leak `SIGNING_KEY` and
`DATABASE_URL`, links to other items would leak their content. `7zz`
output isn't bounded by the reservation.
**Fix:** `Store.Open` resolves inside the item root and refuses symlinks
(`os.Root`, or `Lstat` plus `O_NOFOLLOW`), keeping the current
`filepath.IsLocal` check (internal/store/store.go:57); `Finalize` rejects
manifests with symlinks or non-regular files; run `7zz` with symlinks off,
check every output path, stop past the reservation; fuzz-test the NZB,
yEnc and PAR2 parsers.

### M8. Live-reader tracking too vague to protect playback [design; style 9]
Players send many range requests; a paused player has no connection.
`reader_id`, heartbeat interval, and whether "50 MB or 2 minutes" is per
request or session ("or" = whichever first?) are undefined; a pause over
2 minutes drops protection.
**Fix:** a reader is `(user_id, file_id)`, refreshed on each range request
and by a heartbeat during long responses; a longer timeout (e.g. 30 min)
for eviction protection than for "use"; say LRU order is the second
safeguard.

### M9. Safeguards from the brief dropped without a record [structural]
Missing from the design and not recorded as rejected: daily byte cap per
RSS feed; "evict RSS-added items first" (conflicts with one global rule,
152); `.torrent` support in RSS; the Telegram 50 MB / one-copy note;
"from the docs, not yet checked" marking for fakes.
**Fix:** add each where it belongs, or record the rejection (e.g.
Alternative N: "Evict RSS-added items first", rejected for one LRU rule).

### M10. Terms drift between sections [style]
* "two-step" (83, 360) vs "six-step" (492): say once in section 4 that a
  two-step source takes six API calls for RD.
* "test user" (122-124, 196, 547, 576) never defined vs "invited users"
  (24): define once in the Overview (owner is not one).
* "RD (or TorBox)" (371) with no selection rule, and "Hoster and RD/TorBox
  sources can be added only by the owner" (373) reads as contradicting
  automatic imports for test users: reword 369-374 to separate pasting
  links directly (owner only) from automatic fallback (per user).
* "HTTP 1.0" (454) reads as a protocol version: write "a multiplier: 1.0
  for torrent and HTTP sources, 2.2 for Usenet".

### M11. Scope and claims [style]
* Overview (29) says 15 stages (0–14) are covered; stage 14 is a non-goal
  (146-149). Say stages 0–13, stage 14 listed as possible later work.
* "Large debrid services don't offer this either" (558) is unsourced: cite
  the teardown or delete it.

## 🟢 Nit

* **Routes and diagram** [design, structural, style]: diagram (248-276)
  shows `/dl` but the Caddyfile uses `/d/*`; `/play` and
  `/telegram/<secret>` missing, and which program serves `/play` isn't
  said; caption cut off ("then are renamed in" → "into /data/store"); "RSS"
  label on the scheduler → store arrow is meaningless. Add a full route
  table.
* **Headings** [structural]: `### Additional Resources` and `### Future
  Improvements` (662, 669) belong at `##`.
* **Open Questions holds decisions** (157-160) [structural, style]: write
  "None." and move the decisions to sections 1, 10, 12.
* **Container name** [structural, style]: "a second `ingest` container"
  (244) vs `ingest-usenet` (495) vs one `ingest` box in the diagram; pick
  `ingest-usenet` everywhere.
* **Program count** (227-237) [structural, style]: text says four, table
  has five; say `migrate` is new and replaces migrate-on-start.
* **State table events** (294, 330-333) [structural]: list the events
  `Next()` takes, so the table is the one that's tested.
* **TorBox Usenet/web keys** (312-313) [structural]: say they use the
  `nzb`/`hoster` key rules.
* **Test count** (522) [structural]: "52 tests" vs 32 top-level `func
  Test`; re-count or drop.
* **Links** (664-667) [structural, style]: bare paths; `~/reports/…` is
  local-only — use repo-relative links and mark it private.
* **Line wrap** (515) [structural, style]: reflow; add bytes served per
  user per day to the metrics list.
* **Migration rename** (284-287) [design]: the new filename only fails by
  accident (`CREATE TABLE users` collides); add a real startup check for
  `0001_init` in `schema_migrations`, or drop the claim.
* **Expired-lease reaper** (392-395) [design]: name the job that moves
  expired `active` rows back to `queued`.
* **BitTorrent v2/hybrid** (305-307) [design]: register both hashes of a
  hybrid torrent as keys of the same item.
* **Telegram** [design]: say whether the bot sends a short signed link or a
  credential-bound `/play` URL (107, 475-481); use the `secret_token` header
  rather than only a path secret (498).
* **Service layer gaps** [design]: whether a test user may add an
  already-cached hoster/RD key; user and invite creation; rate-limiting
  login and credential checks (373, 478-479).
* **Missing alternative** [design]: an embedded Go torrent library (e.g.
  `anacrolix/torrent`) instead of qBittorrent, with a reason.
* **Wording** [style]: "identity model" (30), "simply" (43), "proven"
  (64), "cheap VPS" (90), CUJ 5 title → "metadata doesn't arrive" (196),
  "unless someone asks" (218), "REST now"/"usenet 3/9" (233, 235), "next
  check" interval (244), "fails loudly" (286), "timestamps" (294),
  "`status` is small" (317), "pure-Go" (331), "gets a conflict" (392-394),
  unnamed limit settings (456), six-hour link lifetime first mentioned at
  470-472, "named legal test torrent" (511-512), "Mature" (533), "The own
  client" (538), "teaches nothing" (549), "slow torrents stop mattering"
  (575), "error-prone" (602), "very different Usenet path" (627), "richer
  interface" (634), pins do need a table (560-561), lease wording (154-155
  vs 674).
