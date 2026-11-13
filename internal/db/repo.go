package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"torza/internal/magnet"
)

// Repo runs Torza's database queries.
type Repo struct{ pool *pgxpool.Pool }

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// ── Users ───────────────────────────────────────────────────────────────────

func (r *Repo) CreateUser(ctx context.Context, email string, isAdmin bool, keyHash []byte, keyPrefix string) (User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `
		INSERT INTO users (email, api_key_hash, api_key_prefix, is_admin)
		VALUES ($1, $2, $3, $4)
		RETURNING id, email, is_admin, created_at`,
		email, keyHash, keyPrefix, isAdmin).Scan(&u.ID, &u.Email, &u.IsAdmin, &u.CreatedAt)
	return u, err
}

func (r *Repo) UserByKeyHash(ctx context.Context, hash []byte) (User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `SELECT id, email, is_admin, created_at FROM users WHERE api_key_hash = $1`, hash).
		Scan(&u.ID, &u.Email, &u.IsAdmin, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// ── A user's library (api) ──────────────────────────────────────────────────

// AddTorrentForUser adds a torrent to a user's library. If it isn't cached and
// isn't already downloading, it's queued for download, and that counts toward
// this user's limit of active downloads. queued reports whether a new download
// was started.
func (r *Repo) AddTorrentForUser(ctx context.Context, userID int64, m magnet.Magnet, maxActive int) (t Torrent, queued bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Torrent{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // does nothing once committed

	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocklist WHERE infohash = $1)`, m.InfoHash).Scan(&blocked); err != nil {
		return Torrent{}, false, err
	}
	if blocked {
		return Torrent{}, false, ErrBlocked
	}

	t, err = scanTorrent(tx.QueryRow(ctx, `SELECT `+torrentCols("")+` FROM torrents WHERE infohash = $1 FOR UPDATE`, m.InfoHash))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Torrent{}, false, err
	}
	exists := err == nil

	switch {
	case exists && t.Status == StatusBlocked:
		return Torrent{}, false, ErrBlocked
	case exists && (t.Status == StatusReady || t.Status == StatusQueued || t.Status == StatusDownloading):
		// Cached or already on its way: just add it to this user's library.
	default:
		// Not cached (new, evicted, or failed before): download it.
		if maxActive > 0 {
			var active int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM torrents WHERE requested_by = $1 AND status IN ('queued', 'downloading')`, userID).Scan(&active); err != nil {
				return Torrent{}, false, err
			}
			if active >= int64(maxActive) {
				return Torrent{}, false, ErrTooManyActive
			}
		}
		t, err = scanTorrent(tx.QueryRow(ctx, `
			INSERT INTO torrents (infohash, name, magnet, status, requested_by)
			VALUES ($1, $2, $3, 'queued', $4)
			ON CONFLICT (infohash) DO UPDATE SET
				name         = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE torrents.name END,
				magnet       = EXCLUDED.magnet,
				status       = 'queued',
				size_bytes   = 0,
				progress     = 0,
				error        = '',
				requested_by = EXCLUDED.requested_by,
				completed_at = NULL,
				purged_at    = NULL,
				updated_at   = now()
			RETURNING `+torrentCols(""), m.InfoHash, m.Name, m.URI(), userID))
		if err != nil {
			return Torrent{}, false, err
		}
		queued = true
	}

	if _, err := tx.Exec(ctx, `INSERT INTO user_torrents (user_id, infohash) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, m.InfoHash); err != nil {
		return Torrent{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Torrent{}, false, err
	}
	return t, queued, nil
}

func (r *Repo) ListUserTorrents(ctx context.Context, userID int64) ([]Torrent, error) {
	return collectTorrents(r.pool.Query(ctx, `
		SELECT `+torrentCols("t.")+`
		FROM torrents t
		JOIN user_torrents ut ON ut.infohash = t.infohash
		WHERE ut.user_id = $1
		ORDER BY ut.added_at DESC`, userID))
}

// UserTorrent returns one torrent from a user's library, with its files.
func (r *Repo) UserTorrent(ctx context.Context, userID int64, infohash string) (Torrent, []File, error) {
	t, err := scanTorrent(r.pool.QueryRow(ctx, `
		SELECT `+torrentCols("t.")+`
		FROM torrents t
		JOIN user_torrents ut ON ut.infohash = t.infohash
		WHERE ut.user_id = $1 AND t.infohash = $2`, userID, infohash))
	if err != nil {
		return Torrent{}, nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT id, infohash, path, size_bytes FROM torrent_files WHERE infohash = $1 ORDER BY path`, infohash)
	if err != nil {
		return Torrent{}, nil, err
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.InfoHash, &f.Path, &f.SizeBytes); err != nil {
			return Torrent{}, nil, err
		}
		files = append(files, f)
	}
	return t, files, rows.Err()
}

// RemoveUserTorrent removes a torrent from one user's library. The cached
// copy stays for other users until eviction.
func (r *Repo) RemoveUserTorrent(ctx context.Context, userID int64, infohash string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM user_torrents WHERE user_id = $1 AND infohash = $2`, userID, infohash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FileForUser returns a file and its torrent, but only if the torrent is in
// the user's library.
func (r *Repo) FileForUser(ctx context.Context, userID, fileID int64) (File, Torrent, error) {
	var f File
	var t Torrent
	dest := append([]any{&f.ID, &f.InfoHash, &f.Path, &f.SizeBytes}, torrentDest(&t)...)
	err := r.pool.QueryRow(ctx, `
		SELECT f.id, f.infohash, f.path, f.size_bytes, `+torrentCols("t.")+`
		FROM torrent_files f
		JOIN torrents t ON t.infohash = f.infohash
		JOIN user_torrents ut ON ut.infohash = f.infohash AND ut.user_id = $1
		WHERE f.id = $2`, userID, fileID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, Torrent{}, ErrNotFound
	}
	return f, t, err
}

// TouchTorrent records that a torrent was just used, so eviction keeps it. It
// writes at most once a minute per torrent, because video players send many
// small range requests.
func (r *Repo) TouchTorrent(ctx context.Context, infohash string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE torrents SET last_accessed_at = now()
		WHERE infohash = $1 AND last_accessed_at < now() - interval '1 minute'`, infohash)
	return err
}

// ── Downloads (ingest) ──────────────────────────────────────────────────────

func (r *Repo) TorrentsByStatus(ctx context.Context, statuses ...string) ([]Torrent, error) {
	return collectTorrents(r.pool.Query(ctx, `SELECT `+torrentCols("")+` FROM torrents WHERE status = ANY($1) ORDER BY updated_at`, statuses))
}

// BlockedUnpurged returns blocked torrents whose files haven't been deleted yet.
func (r *Repo) BlockedUnpurged(ctx context.Context) ([]Torrent, error) {
	return collectTorrents(r.pool.Query(ctx, `SELECT `+torrentCols("")+` FROM torrents WHERE status = 'blocked' AND purged_at IS NULL`))
}

func (r *Repo) MarkDownloading(ctx context.Context, infohash string) error {
	_, err := r.pool.Exec(ctx, `UPDATE torrents SET status = 'downloading', updated_at = now() WHERE infohash = $1 AND status = 'queued'`, infohash)
	return err
}

func (r *Repo) UpdateProgress(ctx context.Context, infohash, name string, size int64, progress float64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE torrents SET
			name       = CASE WHEN $2::text <> '' THEN $2::text ELSE name END,
			size_bytes = $3,
			progress   = $4,
			updated_at = now()
		WHERE infohash = $1 AND status = 'downloading'`, infohash, name, size, progress)
	return err
}

// MarkReady records a finished torrent and its files. It returns ErrConflict
// if the torrent stopped downloading meanwhile (for example, it was blocked).
func (r *Repo) MarkReady(ctx context.Context, infohash, name string, size int64, files []FileInput) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE torrents SET
			status           = 'ready',
			name             = CASE WHEN $2::text <> '' THEN $2::text ELSE name END,
			size_bytes       = $3,
			progress         = 1,
			error            = '',
			completed_at     = now(),
			last_accessed_at = now(),
			updated_at       = now()
		WHERE infohash = $1 AND status = 'downloading'`, infohash, name, size)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM torrent_files WHERE infohash = $1`, infohash); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(ctx, `INSERT INTO torrent_files (infohash, path, size_bytes) VALUES ($1, $2, $3)`, infohash, f.Path, f.SizeBytes); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *Repo) MarkFailed(ctx context.Context, infohash, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE torrents SET status = 'failed', error = $2, updated_at = now()
		WHERE infohash = $1 AND status IN ('queued', 'downloading')`, infohash, reason)
	return err
}

// MarkPurged records that a blocked torrent's files are gone.
func (r *Repo) MarkPurged(ctx context.Context, infohash string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE torrents SET purged_at = now(), size_bytes = 0, updated_at = now() WHERE infohash = $1`, infohash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM torrent_files WHERE infohash = $1`, infohash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── Cache eviction (scheduler) ──────────────────────────────────────────────

// CacheBytes is the total size of all ready torrents.
func (r *Repo) CacheBytes(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes), 0)::bigint FROM torrents WHERE status = 'ready'`).Scan(&n)
	return n, err
}

// EvictionCandidates returns ready torrents not used since idleSince, least
// recently used first.
func (r *Repo) EvictionCandidates(ctx context.Context, idleSince time.Time, limit int) ([]Torrent, error) {
	return collectTorrents(r.pool.Query(ctx, `
		SELECT `+torrentCols("")+` FROM torrents
		WHERE status = 'ready' AND last_accessed_at < $1
		ORDER BY last_accessed_at
		LIMIT $2`, idleSince, limit))
}

// MarkEvicted marks a torrent as no longer cached. Call it before deleting the
// files, so no new links are made for them.
func (r *Repo) MarkEvicted(ctx context.Context, infohash string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE torrents SET status = 'evicted', updated_at = now() WHERE infohash = $1 AND status = 'ready'`, infohash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM torrent_files WHERE infohash = $1`, infohash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── Takedowns (api, admin) ──────────────────────────────────────────────────

// Block adds a torrent to the blocklist and marks it blocked. ingest then
// deletes its files.
func (r *Repo) Block(ctx context.Context, infohash, reason, reference string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO blocklist (infohash, reason, reference) VALUES ($1, $2, $3)
		ON CONFLICT (infohash) DO UPDATE SET reason = EXCLUDED.reason, reference = EXCLUDED.reference`,
		infohash, reason, reference); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE torrents SET status = 'blocked', purged_at = NULL, updated_at = now() WHERE infohash = $1`, infohash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── Helpers ─────────────────────────────────────────────────────────────────

var torrentFields = []string{
	"infohash", "name", "magnet", "status", "size_bytes", "progress", "error",
	"requested_by", "created_at", "updated_at", "completed_at", "last_accessed_at",
}

// torrentCols lists the torrent columns, optionally prefixed with a table alias.
func torrentCols(prefix string) string {
	cols := make([]string, len(torrentFields))
	for i, f := range torrentFields {
		cols[i] = prefix + f
	}
	return strings.Join(cols, ", ")
}

// torrentDest returns scan targets in the same order as torrentFields.
func torrentDest(t *Torrent) []any {
	return []any{&t.InfoHash, &t.Name, &t.Magnet, &t.Status, &t.SizeBytes, &t.Progress, &t.Error,
		&t.RequestedBy, &t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &t.LastAccessedAt}
}

func scanTorrent(row pgx.Row) (Torrent, error) {
	var t Torrent
	err := row.Scan(torrentDest(&t)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Torrent{}, ErrNotFound
	}
	return t, err
}

func collectTorrents(rows pgx.Rows, err error) ([]Torrent, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Torrent
	for rows.Next() {
		var t Torrent
		if err := rows.Scan(torrentDest(&t)...); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
