package db

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// AddResult is what adding a key to a library did.
type AddResult struct {
	Item    Item
	Files   []File // the item's present files when it is ready
	Started bool   // this add started a new download
}

// Add puts the content a key names into a user's library. If it is cached or
// already on its way, that is all it does, and it is free. Otherwise it
// queues a download for fetcher, which counts against this user's limit.
//
// One transaction holds the user's lock, then the key's lock, so two users
// adding the same new key at once start one download, one user's parallel
// adds can't pass their limit together, and a takedown can't be lost.
func (r *Repo) Add(ctx context.Context, userID int64, k Key, payload, name, fetcher string, def Limits) (AddResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AddResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUser(ctx, tx, userID); err != nil {
		return AddResult{}, err
	}
	if err := lockKey(ctx, tx, k); err != nil {
		return AddResult{}, err
	}
	if blocked, err := keyBlocked(ctx, tx, k); err != nil || blocked {
		return AddResult{}, errors.Join(err, blockedErr(blocked))
	}

	var res AddResult
	var sourceID int64
	err = tx.QueryRow(ctx, `SELECT s.id, `+itemCols("i.")+` FROM item_sources s JOIN items i ON i.id = s.item_id
		WHERE s.kind = $1 AND s.key = $2 FOR UPDATE OF i`, k.Kind, k.Value).
		Scan(append([]any{&sourceID}, itemDest(&res.Item)...)...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if res.Item, err = enqueueNew(ctx, tx, userID, k, payload, name, fetcher, def); err != nil {
			return AddResult{}, err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO item_sources (kind, key, item_id, payload) VALUES ($1, $2, $3, $4) RETURNING id`,
			k.Kind, k.Value, res.Item.ID, payload).Scan(&sourceID); err != nil {
			return AddResult{}, err
		}
		res.Started = true
	case err != nil:
		return AddResult{}, err
	case res.Item.Status == StatusBlocked:
		return AddResult{}, ErrBlocked
	case res.Item.Status == StatusFailed || res.Item.Status == StatusEvicted:
		if res.Item, err = enqueue(ctx, tx, userID, res.Item.ID, fetcher, def); err != nil {
			return AddResult{}, err
		}
		res.Started = true
	default:
		// Cached or on its way. A higher-priority user waiting for it moves it up.
		if res.Item.Status == StatusQueued {
			l, err := limitsFor(ctx, tx, userID, def)
			if err != nil {
				return AddResult{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE items SET priority = GREATEST(priority, $2) WHERE id = $1`, res.Item.ID, l.Priority); err != nil {
				return AddResult{}, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_items (user_id, item_id, source_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		userID, res.Item.ID, sourceID); err != nil {
		return AddResult{}, err
	}
	if res.Item.Status == StatusReady {
		if res.Files, err = collectFiles(tx.Query(ctx, `SELECT `+fileCols+` FROM item_files WHERE item_id = $1 AND state = 'present' ORDER BY path`, res.Item.ID)); err != nil {
			return AddResult{}, err
		}
	}
	return res, tx.Commit(ctx)
}

// Requeue downloads an evicted or failed item in a user's library again, as
// that user. It is how playing an evicted file brings it back. It returns the
// item as it now is; a ready, queued or active item is left as it is.
func (r *Repo) Requeue(ctx context.Context, userID, itemID int64, fetcher string, def Limits) (Item, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Item{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUser(ctx, tx, userID); err != nil {
		return Item{}, err
	}
	keys, err := itemKeys(ctx, tx, itemID)
	if err != nil {
		return Item{}, err
	}
	for _, k := range keys {
		if err := lockKey(ctx, tx, k); err != nil {
			return Item{}, err
		}
	}
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols("i.")+` FROM items i JOIN user_items ui ON ui.item_id = i.id
		WHERE ui.user_id = $1 AND i.id = $2 FOR UPDATE OF i`, userID, itemID))
	if err != nil {
		return Item{}, err
	}
	switch it.Status {
	case StatusBlocked:
		return Item{}, ErrBlocked
	case StatusFailed, StatusEvicted:
		if it, err = enqueue(ctx, tx, userID, it.ID, fetcher, def); err != nil {
			return Item{}, err
		}
	}
	return it, tx.Commit(ctx)
}

// enqueueNew creates an item and queues it. The caller holds the user lock.
func enqueueNew(ctx context.Context, tx pgx.Tx, userID int64, k Key, payload, name, fetcher string, def Limits) (Item, error) {
	l, err := checkActive(ctx, tx, userID, def)
	if err != nil {
		return Item{}, err
	}
	it, err := scanItem(tx.QueryRow(ctx, `
		INSERT INTO items (name, status, fetcher, started_by, priority) VALUES ($1, 'queued', $2, $3, $4)
		RETURNING `+itemCols(""), name, fetcher, userID, l.Priority))
	if err != nil {
		return Item{}, err
	}
	return it, notify(ctx, tx, ChanQueued(fetcher), strconv.FormatInt(it.ID, 10))
}

// enqueue is the one way an existing item moves to queued: it counts against
// the enqueuing user's limit and takes their priority. The caller holds the
// user lock and the item's key locks.
func enqueue(ctx context.Context, tx pgx.Tx, userID, itemID int64, fetcher string, def Limits) (Item, error) {
	l, err := checkActive(ctx, tx, userID, def)
	if err != nil {
		return Item{}, err
	}
	it, err := scanItem(tx.QueryRow(ctx, `
		UPDATE items SET status = 'queued', phase = '', fetcher = $2, handle = NULL, started_by = $3,
			priority = $4, size_bytes = 0, progress = 0, error = '', queued_at = now(), active_since = NULL,
			completed_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('failed', 'evicted')
		RETURNING `+itemCols(""), itemID, fetcher, userID, l.Priority))
	if errors.Is(err, ErrNotFound) {
		return Item{}, ErrConflict
	}
	if err != nil {
		return Item{}, err
	}
	return it, notify(ctx, tx, ChanQueued(fetcher), strconv.FormatInt(it.ID, 10))
}

func checkActive(ctx context.Context, tx pgx.Tx, userID int64, def Limits) (Limits, error) {
	l, err := limitsFor(ctx, tx, userID, def)
	if err != nil || l.MaxActive <= 0 {
		return l, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM items WHERE started_by = $1 AND status IN ('queued', 'active', 'finalizing')`, userID).Scan(&active); err != nil {
		return l, err
	}
	if active >= l.MaxActive {
		return l, ErrTooManyActive
	}
	return l, nil
}

func keyBlocked(ctx context.Context, tx pgx.Tx, k Key) (bool, error) {
	var blocked bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocklist WHERE kind = $1 AND key = $2)`, k.Kind, k.Value).Scan(&blocked)
	return blocked, err
}

func blockedErr(blocked bool) error {
	if blocked {
		return ErrBlocked
	}
	return nil
}

func itemKeys(ctx context.Context, tx pgx.Tx, itemID int64) ([]Key, error) {
	rows, err := tx.Query(ctx, `SELECT kind, key FROM item_sources WHERE item_id = $1 ORDER BY kind, key`, itemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) {
		var k Key
		return k, row.Scan(&k.Kind, &k.Value)
	})
}

// Check reports, for each key, whether its content is cached, on its way or
// unknown, without starting anything. Blocked keys report "blocked".
func (r *Repo) Check(ctx context.Context, keys []Key) ([]Availability, error) {
	out := make([]Availability, len(keys))
	kinds := make([]string, len(keys))
	values := make([]string, len(keys))
	for i, k := range keys {
		out[i].Key = k
		kinds[i], values[i] = k.Kind, k.Value
	}
	var ready []int64
	readyAt := map[int64][]int{}
	rows, err := r.pool.Query(ctx, `
		SELECT q.n, COALESCE(i.id, 0),
			CASE WHEN b.key IS NOT NULL OR i.status = 'blocked' THEN 'blocked' ELSE COALESCE(i.status, '') END,
			COALESCE(i.name, ''), COALESCE(i.size_bytes, 0)
		FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS q(kind, key, n)
		LEFT JOIN item_sources s ON s.kind = q.kind AND s.key = q.key
		LEFT JOIN items i ON i.id = s.item_id
		LEFT JOIN blocklist b ON b.kind = q.kind AND b.key = q.key`, kinds, values)
	if err != nil {
		return nil, err
	}
	var (
		n      int
		id     int64
		status string
		name   string
		size   int64
	)
	_, err = pgx.ForEachRow(rows, []any{&n, &id, &status, &name, &size}, func() error {
		a := &out[n-1]
		a.ItemID, a.Status = id, status
		if status != StatusBlocked {
			a.Name, a.Size = name, size
		} else {
			a.ItemID = 0
		}
		if status == StatusReady {
			if len(readyAt[id]) == 0 {
				ready = append(ready, id)
			}
			readyAt[id] = append(readyAt[id], n-1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(ready) > 0 {
		files, err := collectFiles(r.pool.Query(ctx, `SELECT `+fileCols+` FROM item_files WHERE item_id = ANY($1) AND state = 'present' ORDER BY item_id, path`, ready))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			for _, i := range readyAt[f.ItemID] {
				out[i].Files = append(out[i].Files, f)
			}
		}
	}
	return out, nil
}

// Library lists a user's items, newest first.
func (r *Repo) Library(ctx context.Context, userID int64) ([]LibraryItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+itemCols("i.")+`, ui.added_at, COALESCE(s.kind, ''), COALESCE(s.key, '')
		FROM items i JOIN user_items ui ON ui.item_id = i.id
		LEFT JOIN item_sources s ON s.id = ui.source_id
		WHERE ui.user_id = $1 ORDER BY ui.added_at DESC, i.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LibraryItem, error) {
		var li LibraryItem
		return li, row.Scan(append(itemDest(&li.Item), &li.AddedAt, &li.Key.Kind, &li.Key.Value)...)
	})
}

// LibraryItem returns one item in a user's library with its present files.
func (r *Repo) LibraryItem(ctx context.Context, userID, itemID int64) (Item, []File, error) {
	it, err := scanItem(r.pool.QueryRow(ctx, `SELECT `+itemCols("i.")+` FROM items i JOIN user_items ui ON ui.item_id = i.id
		WHERE ui.user_id = $1 AND i.id = $2`, userID, itemID))
	if err != nil || it.Status != StatusReady {
		return it, nil, err
	}
	files, err := collectFiles(r.pool.Query(ctx, `SELECT `+fileCols+` FROM item_files WHERE item_id = $1 AND state = 'present' ORDER BY path`, itemID))
	return it, files, err
}

// FilesOf lists the files of items in a user's library, missing ones too,
// so an evicted item still shows what it held. Pending files are left out.
func (r *Repo) FilesOf(ctx context.Context, userID int64, itemIDs []int64) ([]File, error) {
	return collectFiles(r.pool.Query(ctx, `SELECT f.id, f.item_id, f.path, f.size_bytes, f.state FROM item_files f
		JOIN user_items ui ON ui.item_id = f.item_id AND ui.user_id = $1
		WHERE f.item_id = ANY($2) AND f.state <> 'pending' ORDER BY f.item_id, f.path`, userID, itemIDs))
}

// ItemKeys lists the source keys that lead to an item.
func (r *Repo) ItemKeys(ctx context.Context, itemID int64) ([]Key, error) {
	rows, err := r.pool.Query(ctx, `SELECT kind, key FROM item_sources WHERE item_id = $1 ORDER BY kind, key`, itemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) {
		var k Key
		return k, row.Scan(&k.Kind, &k.Value)
	})
}

// RemoveFromLibrary removes an item from one user's library. The cached
// copy stays for everyone else; eviction deletes it once nobody uses it.
func (r *Repo) RemoveFromLibrary(ctx context.Context, userID, itemID int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM user_items WHERE user_id = $1 AND item_id = $2`, userID, itemID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// FileForUser returns a file and its item if the item is in the user's library.
func (r *Repo) FileForUser(ctx context.Context, userID, fileID int64) (File, Item, error) {
	var f File
	var it Item
	err := r.pool.QueryRow(ctx, `SELECT f.id, f.item_id, f.path, f.size_bytes, f.state, `+itemCols("i.")+`
		FROM item_files f JOIN items i ON i.id = f.item_id JOIN user_items ui ON ui.item_id = i.id
		WHERE f.id = $1 AND ui.user_id = $2`, fileID, userID).
		Scan(append([]any{&f.ID, &f.ItemID, &f.Path, &f.SizeBytes, &f.State}, itemDest(&it)...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, Item{}, ErrNotFound
	}
	return f, it, err
}

// UsageFor reports a user's active downloads and library size.
func (r *Repo) UsageFor(ctx context.Context, userID int64) (Usage, error) {
	var u Usage
	err := r.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM items WHERE started_by = $1 AND status IN ('queued', 'active', 'finalizing')),
		(SELECT count(*) FROM user_items WHERE user_id = $1),
		(SELECT COALESCE(sum(i.size_bytes), 0) FROM items i JOIN user_items ui ON ui.item_id = i.id WHERE ui.user_id = $1 AND i.status = 'ready')`,
		userID).Scan(&u.Active, &u.LibraryItems, &u.LibraryBytes)
	return u, err
}

// Block takes content down by one of its keys: every key of the item goes on
// the blocklist and the item is marked blocked, in one transaction under the
// key locks. Ingest (for an item a fetcher holds) or the scheduler then
// deletes the files. Blocking a key nobody has added yet blocks it ahead.
func (r *Repo) Block(ctx context.Context, k Key, reason, reference string, by int64) (itemID int64, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockKey(ctx, tx, k); err != nil {
		return 0, err
	}
	err = tx.QueryRow(ctx, `SELECT item_id FROM item_sources WHERE kind = $1 AND key = $2`, k.Kind, k.Value).Scan(&itemID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	keys := []Key{k}
	if itemID != 0 {
		others, err := itemKeys(ctx, tx, itemID)
		if err != nil {
			return 0, err
		}
		for _, o := range others {
			if o != k {
				if err := lockKey(ctx, tx, o); err != nil {
					return 0, err
				}
				keys = append(keys, o)
			}
		}
	}
	var item *int64
	if itemID != 0 {
		item = &itemID
	}
	for _, bk := range keys {
		if _, err := tx.Exec(ctx, `
			INSERT INTO blocklist (kind, key, item_id, reason, reference, blocked_by) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (kind, key) DO UPDATE SET item_id = EXCLUDED.item_id, reason = EXCLUDED.reason, reference = EXCLUDED.reference`,
			bk.Kind, bk.Value, item, reason, reference, by); err != nil {
			return 0, err
		}
	}
	if itemID != 0 {
		if _, err := tx.Exec(ctx, `UPDATE items SET status = 'blocked', purged_at = NULL, updated_at = now() WHERE id = $1`, itemID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE item_files SET state = 'missing' WHERE item_id = $1`, itemID); err != nil {
			return 0, err
		}
		if err := notify(ctx, tx, ChanBlocked, strconv.FormatInt(itemID, 10)); err != nil {
			return 0, err
		}
	}
	return itemID, tx.Commit(ctx)
}
