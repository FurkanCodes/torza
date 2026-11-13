package db

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ingest's queries. Every write to an item a fetcher holds is guarded by its
// status and fetcher, so a write after a takedown does nothing and returns
// ErrConflict; ingest then purges the item like any other blocked one.

const jobCols = `i.id, s.kind, s.key, s.payload, i.handle, i.status, COALESCE(i.active_since, i.updated_at),
	COALESCE(ul.max_item_bytes, $2)`

const jobFrom = ` FROM items i
	JOIN LATERAL (SELECT kind, key, payload FROM item_sources WHERE item_id = i.id ORDER BY id LIMIT 1) s ON true
	LEFT JOIN user_limits ul ON ul.user_id = i.started_by`

func scanJobs(rows pgx.Rows, err error) ([]Job, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Job, error) {
		var j Job
		return j, row.Scan(&j.ItemID, &j.Key.Kind, &j.Key.Value, &j.Payload, &j.Handle, &j.Status, &j.ActiveSince, &j.MaxItemBytes)
	})
}

// Claim moves up to n queued items of a fetcher to active, highest priority
// first, and records an attempt for each. SKIP LOCKED keeps two claimers
// from taking the same item. defaultMaxBytes is the server's item size limit
// for users without their own.
func (r *Repo) Claim(ctx context.Context, fetcher string, n int, defaultMaxBytes int64) ([]Job, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	jobs, err := scanJobs(tx.Query(ctx, `
		WITH claimed AS (
			UPDATE items SET status = 'active', phase = 'starting', active_since = now(), updated_at = now()
			WHERE id IN (
				SELECT id FROM items WHERE status = 'queued' AND fetcher = $1
				ORDER BY priority DESC, queued_at, id
				FOR UPDATE SKIP LOCKED LIMIT $3)
			RETURNING *
		)
		SELECT `+jobCols+` FROM claimed i
		JOIN LATERAL (SELECT kind, key, payload FROM item_sources WHERE item_id = i.id ORDER BY id LIMIT 1) s ON true
		LEFT JOIN user_limits ul ON ul.user_id = i.started_by`, fetcher, defaultMaxBytes, n))
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if _, err := tx.Exec(ctx, `INSERT INTO fetch_attempts (item_id, fetcher) VALUES ($1, $2)`, j.ItemID, fetcher); err != nil {
			return nil, err
		}
	}
	return jobs, tx.Commit(ctx)
}

// Held lists the items a fetcher holds: active and finalizing ones.
func (r *Repo) Held(ctx context.Context, fetcher string, defaultMaxBytes int64) ([]Job, error) {
	return scanJobs(r.pool.Query(ctx, `SELECT `+jobCols+jobFrom+`
		WHERE i.fetcher = $1 AND i.status IN ('active', 'finalizing') ORDER BY i.id`, fetcher, defaultMaxBytes))
}

func guarded(tag int64, err error) error {
	if err == nil && tag == 0 {
		return ErrConflict
	}
	return err
}

// SetHandle stores what a fetcher needs to find its work again.
func (r *Repo) SetHandle(ctx context.Context, itemID int64, fetcher string, handle []byte) error {
	tag, err := r.pool.Exec(ctx, `UPDATE items SET handle = $3, updated_at = now()
		WHERE id = $1 AND fetcher = $2 AND status = 'active'`, itemID, fetcher, handle)
	return guarded(tag.RowsAffected(), err)
}

// Progress records how far a download is.
func (r *Repo) Progress(ctx context.Context, itemID int64, fetcher, phase, name string, size int64, progress float64) error {
	tag, err := r.pool.Exec(ctx, `UPDATE items SET phase = $3, name = CASE WHEN $4 <> '' THEN $4 ELSE name END,
			size_bytes = $5, progress = $6, updated_at = now()
		WHERE id = $1 AND fetcher = $2 AND status = 'active'`, itemID, fetcher, phase, name, size, progress)
	return guarded(tag.RowsAffected(), err)
}

// BeginFinalize stores a finished download's manifest before the fetcher
// lets go of the files: every file of the item is set missing, the
// manifest's files are upserted as pending (keeping their IDs), and the item
// becomes finalizing. After a crash, the database alone says what to do.
func (r *Repo) BeginFinalize(ctx context.Context, itemID int64, fetcher, name string, files []ManifestFile) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var size int64
	for _, f := range files {
		size += f.SizeBytes
	}
	tag, err := tx.Exec(ctx, `UPDATE items SET status = 'finalizing', phase = 'publishing',
			name = CASE WHEN $3 <> '' THEN $3 ELSE name END, size_bytes = $4, progress = 1, updated_at = now()
		WHERE id = $1 AND fetcher = $2 AND status = 'active'`, itemID, fetcher, name, size)
	if err := guarded(tag.RowsAffected(), err); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE item_files SET state = 'missing' WHERE item_id = $1`, itemID); err != nil {
		return err
	}
	paths := make([]string, len(files))
	sizes := make([]int64, len(files))
	for i, f := range files {
		paths[i], sizes[i] = f.Path, f.SizeBytes
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO item_files (item_id, path, size_bytes, state)
		SELECT $1, p, s, 'pending' FROM unnest($2::text[], $3::bigint[]) AS m(p, s)
		ON CONFLICT (item_id, path) DO UPDATE SET size_bytes = EXCLUDED.size_bytes, state = 'pending'`,
		itemID, paths, sizes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkReady finishes a download once its files are in store/<id>: pending
// files become present, the fetcher lets go of the item, and listeners on
// ChanReady hear about it.
func (r *Repo) MarkReady(ctx context.Context, itemID int64, fetcher string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE items SET status = 'ready', phase = '', handle = NULL, error = '',
			completed_at = now(), last_used_at = now(), updated_at = now()
		WHERE id = $1 AND fetcher = $2 AND status = 'finalizing'`, itemID, fetcher)
	if err := guarded(tag.RowsAffected(), err); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE item_files SET state = 'present' WHERE item_id = $1 AND state = 'pending'`, itemID); err != nil {
		return err
	}
	if err := endAttempt(ctx, tx, itemID, fetcher, "ready", ""); err != nil {
		return err
	}
	if err := notify(ctx, tx, ChanReady, strconv.FormatInt(itemID, 10)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Fail gives up on a download. The fetcher must already have deleted its
// partial files; the item lets go of its handle. A later add re-queues it.
func (r *Repo) Fail(ctx context.Context, itemID int64, fetcher, reason string) error {
	return r.release(ctx, itemID, fetcher, StatusFailed, "failed", reason)
}

// RequeueHeld puts a held item back in the queue with no handle, for a fresh
// start: the recovery step when a finalizing item has nothing on disk.
func (r *Repo) RequeueHeld(ctx context.Context, itemID int64, fetcher, reason string) error {
	return r.release(ctx, itemID, fetcher, StatusQueued, "requeued", reason)
}

func (r *Repo) release(ctx context.Context, itemID int64, fetcher, status, result, reason string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE items SET status = $3, phase = '', handle = NULL, error = $4, progress = 0,
			queued_at = CASE WHEN $3 = 'queued' THEN now() ELSE queued_at END, active_since = NULL, updated_at = now()
		WHERE id = $1 AND fetcher = $2 AND status IN ('active', 'finalizing')`, itemID, fetcher, status, reason)
	if err := guarded(tag.RowsAffected(), err); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE item_files SET state = 'missing' WHERE item_id = $1 AND state = 'pending'`, itemID); err != nil {
		return err
	}
	if err := endAttempt(ctx, tx, itemID, fetcher, result, reason); err != nil {
		return err
	}
	if status == StatusQueued {
		if err := notify(ctx, tx, ChanQueued(fetcher), strconv.FormatInt(itemID, 10)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func endAttempt(ctx context.Context, tx pgx.Tx, itemID int64, fetcher, result, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE fetch_attempts SET ended_at = now(), result = $3, error = $4
		WHERE item_id = $1 AND fetcher = $2 AND ended_at IS NULL`, itemID, fetcher, result, reason)
	return err
}

// BlockedHeld lists blocked items a fetcher still holds a handle for. Only
// that fetcher can stop the work and delete what it wrote.
func (r *Repo) BlockedHeld(ctx context.Context, fetcher string) ([]Job, error) {
	return scanJobs(r.pool.Query(ctx, `SELECT `+jobCols+jobFrom+`
		WHERE i.fetcher = $1 AND i.status = 'blocked' AND i.handle IS NOT NULL ORDER BY i.id`, fetcher, int64(0)))
}

// PurgedHeld records that a fetcher stopped a blocked item and deleted its
// files: the handle goes, and the item is purged.
func (r *Repo) PurgedHeld(ctx context.Context, itemID int64, fetcher string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE items SET handle = NULL, fetcher = '', purged_at = now(), updated_at = now()
		WHERE id = $1 AND fetcher = $2 AND status = 'blocked'`, itemID, fetcher); err != nil {
		return err
	}
	if err := endAttempt(ctx, tx, itemID, fetcher, "blocked", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LegacyFolders lists ready items whose files still sit in a folder named
// by the old torrent-only layout.
func (r *Repo) LegacyFolders(ctx context.Context) (map[int64]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT item_id, folder FROM legacy_folders`)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	var id int64
	var folder string
	_, err = pgx.ForEachRow(rows, []any{&id, &folder}, func() error { out[id] = folder; return nil })
	return out, err
}

// LegacyFolderDone forgets a converted folder.
func (r *Repo) LegacyFolderDone(ctx context.Context, itemID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM legacy_folders WHERE item_id = $1`, itemID)
	return err
}

// ── Scheduler ───────────────────────────────────────────────────────────────

// UsedBytes is what the cache holds or will soon hold: ready items plus the
// known sizes of downloads in progress. Eviction uses the same number, so it
// makes room for what is downloading.
func (r *Repo) UsedBytes(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes), 0) FROM items WHERE status IN ('ready', 'active', 'finalizing')`).Scan(&n)
	return n, err
}

// EvictionCandidates returns ready items not used since idleSince, least
// recently used first.
func (r *Repo) EvictionCandidates(ctx context.Context, idleSince time.Time, limit int) ([]Item, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+itemCols("")+` FROM items WHERE status = 'ready' AND last_used_at < $1
		ORDER BY last_used_at, id LIMIT $2`, idleSince, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		var it Item
		return it, row.Scan(itemDest(&it)...)
	})
}

// MarkEvicted marks a ready item evicted and its files missing, before the
// files are deleted, so no new link is made for them.
func (r *Repo) MarkEvicted(ctx context.Context, itemID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE items SET status = 'evicted', fetcher = '', updated_at = now() WHERE id = $1 AND status = 'ready'`, itemID)
	if err := guarded(tag.RowsAffected(), err); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE item_files SET state = 'missing' WHERE item_id = $1`, itemID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BlockedUnheld lists blocked items no fetcher holds whose files may still
// be on disk.
func (r *Repo) BlockedUnheld(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM items WHERE status = 'blocked' AND handle IS NULL AND purged_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// MarkPurged records that a blocked item's files are gone.
func (r *Repo) MarkPurged(ctx context.Context, itemID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE items SET purged_at = now(), updated_at = now() WHERE id = $1 AND status = 'blocked' AND handle IS NULL`, itemID)
	return err
}

// KeepFolders returns the IDs of items whose folders must stay on disk.
func (r *Repo) KeepFolders(ctx context.Context) (map[int64]bool, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM items WHERE status IN ('active', 'finalizing', 'ready')
		OR (status = 'blocked' AND (handle IS NOT NULL OR purged_at IS NULL))`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	keep := make(map[int64]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	return keep, err
}

// Touch records that an item was really used (a reader received data), at
// most once a minute.
func (r *Repo) Touch(ctx context.Context, itemID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE items SET last_used_at = now()
		WHERE id = $1 AND status = 'ready' AND last_used_at < now() - interval '1 minute'`, itemID)
	return err
}

// TryJobLock takes a session-free, transaction-free lock for one scheduler
// job kind; the returned release must be called. ok is false if another copy
// holds it.
func (r *Repo) TryJobLock(ctx context.Context, job int) (release func(), ok bool, err error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, lockClassJob, job).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, lockClassJob, job)
		conn.Release()
	}, true, nil
}
