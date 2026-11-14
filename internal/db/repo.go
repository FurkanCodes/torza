package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo runs Torza's queries. Each program uses the methods for its own job:
// users and libraries (api, stream), work (ingest), cache (scheduler).
type Repo struct{ pool *pgxpool.Pool }

func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Pool is the underlying connection pool, for wake-up listeners.
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// ── Users and credentials ───────────────────────────────────────────────────

// CreateUser creates a user with one API credential.
func (r *Repo) CreateUser(ctx context.Context, email string, isAdmin bool, keyHash []byte, keyPrefix string) (User, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var u User
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email, is_admin) VALUES ($1, $2) RETURNING id, email, is_admin, created_at`,
		email, isAdmin).Scan(&u.ID, &u.Email, &u.IsAdmin, &u.CreatedAt); err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO credentials (user_id, kind, hash, prefix) VALUES ($1, 'api', $2, $3)`,
		u.ID, keyHash, keyPrefix); err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

// UserByKeyHash finds the user holding an unrevoked API key.
func (r *Repo) UserByKeyHash(ctx context.Context, hash []byte) (User, error) {
	return r.UserByCredential(ctx, CredAPI, hash)
}

// UserByCredential finds the user holding an unrevoked credential of a kind,
// and notes it was used (at most once a minute, to keep writes down).
func (r *Repo) UserByCredential(ctx context.Context, kind string, hash []byte) (User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `
		WITH c AS (
			UPDATE credentials SET last_used_at = now()
			WHERE hash = $1 AND kind = $2 AND revoked_at IS NULL
			  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')
			RETURNING user_id
		)
		SELECT u.id, u.email, u.is_admin, u.created_at FROM users u
		WHERE u.id = COALESCE((SELECT user_id FROM c),
			(SELECT user_id FROM credentials WHERE hash = $1 AND kind = $2 AND revoked_at IS NULL))`,
		hash, kind).Scan(&u.ID, &u.Email, &u.IsAdmin, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// CreateCredential adds a credential of a kind for a user.
func (r *Repo) CreateCredential(ctx context.Context, userID int64, kind, label string, hash []byte, prefix string) (Credential, error) {
	var c Credential
	err := r.pool.QueryRow(ctx, `INSERT INTO credentials (user_id, kind, label, hash, prefix) VALUES ($1, $2, $3, $4, $5)
		RETURNING id, kind, label, prefix, created_at, last_used_at`, userID, kind, label, hash, prefix).
		Scan(&c.ID, &c.Kind, &c.Label, &c.Prefix, &c.CreatedAt, &c.LastUsedAt)
	return c, err
}

// Credentials lists a user's unrevoked credentials of a kind.
func (r *Repo) Credentials(ctx context.Context, userID int64, kind string) ([]Credential, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, kind, label, prefix, created_at, last_used_at FROM credentials
		WHERE user_id = $1 AND kind = $2 AND revoked_at IS NULL ORDER BY id`, userID, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Credential, error) {
		var c Credential
		return c, row.Scan(&c.ID, &c.Kind, &c.Label, &c.Prefix, &c.CreatedAt, &c.LastUsedAt)
	})
}

// RevokeCredential revokes one of a user's credentials of a kind.
func (r *Repo) RevokeCredential(ctx context.Context, userID, id int64, kind string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE credentials SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND kind = $3 AND revoked_at IS NULL`, id, userID, kind)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// LimitsFor returns a user's limits, falling back to the defaults for any
// the owner hasn't set.
func (r *Repo) LimitsFor(ctx context.Context, userID int64, def Limits) (Limits, error) {
	return limitsFor(ctx, r.pool, userID, def)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func limitsFor(ctx context.Context, q querier, userID int64, def Limits) (Limits, error) {
	var maxActive *int32
	var maxBytes *int64
	var prio *int32
	err := q.QueryRow(ctx, `SELECT max_active, max_item_bytes, priority FROM user_limits WHERE user_id = $1`, userID).
		Scan(&maxActive, &maxBytes, &prio)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return Limits{}, err
	}
	l := def
	if maxActive != nil {
		l.MaxActive = int(*maxActive)
	}
	if maxBytes != nil {
		l.MaxItemBytes = *maxBytes
	}
	if prio != nil {
		l.Priority = int(*prio)
	}
	return l, nil
}

// SetLimits stores a user's own limits. A nil field means "server default".
func (r *Repo) SetLimits(ctx context.Context, userID int64, maxActive *int, maxItemBytes *int64, priority int) error {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO user_limits (user_id, max_active, max_item_bytes, priority)
		SELECT id, $2, $3, $4 FROM users WHERE id = $1
		ON CONFLICT (user_id) DO UPDATE SET
			max_active = EXCLUDED.max_active, max_item_bytes = EXCLUDED.max_item_bytes, priority = EXCLUDED.priority`,
		userID, maxActive, maxItemBytes, priority)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ── Locks ───────────────────────────────────────────────────────────────────

// Advisory lock classes, the first half of each two-number lock key. A
// transaction that needs both takes the user lock first, then the key lock.
const (
	lockClassUser = 1
	lockClassKey  = 2
	lockClassJob  = 3 // one scheduler job of a kind at a time
)

func lockUser(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, ($2::bigint % 2147483647)::int)`, lockClassUser, userID)
	return err
}

// lockKey serialises adds and takedowns of one source key.
func lockKey(ctx context.Context, tx pgx.Tx, k Key) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, lockClassKey, k.Kind+":"+k.Value)
	return err
}

// ── Column lists ────────────────────────────────────────────────────────────

var itemFields = []string{
	"id", "name", "status", "phase", "fetcher", "size_bytes", "progress", "error",
	"started_by", "priority", "created_at", "updated_at", "completed_at", "last_used_at",
}

func itemCols(prefix string) string {
	cols := make([]string, len(itemFields))
	for i, f := range itemFields {
		cols[i] = prefix + f
	}
	return strings.Join(cols, ", ")
}

func itemDest(it *Item) []any {
	return []any{&it.ID, &it.Name, &it.Status, &it.Phase, &it.Fetcher, &it.SizeBytes, &it.Progress, &it.Error,
		&it.StartedBy, &it.Priority, &it.CreatedAt, &it.UpdatedAt, &it.CompletedAt, &it.LastUsedAt}
}

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	err := row.Scan(itemDest(&it)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return it, err
}

func collectFiles(rows pgx.Rows, err error) ([]File, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (File, error) {
		var f File
		return f, row.Scan(&f.ID, &f.ItemID, &f.Path, &f.SizeBytes, &f.State)
	})
}

const fileCols = "id, item_id, path, size_bytes, state"

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}
