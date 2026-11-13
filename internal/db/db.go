package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Connect opens a connection pool and waits up to a minute for the database,
// which may still be starting when the services start in Docker.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Minute)
	for {
		err = pool.Ping(ctx)
		if err == nil {
			return pool, nil
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database not reachable: %w", err)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// migrationLock is an arbitrary number every service uses for the same
// Postgres advisory lock, so only one service runs migrations at a time.
const migrationLock = 7_243_001

// Migrate applies migrations that haven't run yet, in file-name order. Every
// service calls it at startup; the first one to get the lock does the work.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLock) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}

	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	if err := refuseUnknown(ctx, conn, names); err != nil {
		return err
	}
	for _, name := range names {
		version := strings.TrimSuffix(path.Base(name), ".sql")
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrationFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// refuseUnknown stops a program whose code doesn't know a migration the
// database already ran: an older build against a newer database, or a renamed
// migration file.
func refuseUnknown(ctx context.Context, conn *pgxpool.Conn, names []string) error {
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[strings.TrimSuffix(path.Base(n), ".sql")] = true
	}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, v := range versions {
		if !known[v] {
			return fmt.Errorf("the database ran migration %q, which this build doesn't have; run a newer build", v)
		}
	}
	return nil
}

// ── Wake-ups ────────────────────────────────────────────────────────────────

// Channels programs wake each other on. Notifications are sent inside the
// transaction that creates the work, so they arrive only after it commits.
const (
	ChanBlocked = "torza_blocked"
	ChanReady   = "torza_item_ready"
)

// ChanQueued is the channel a fetcher's loop listens on for new work.
func ChanQueued(fetcher string) string { return "torza_queued_" + fetcher }

// notify queues a notification that Postgres delivers when tx commits.
func notify(ctx context.Context, tx pgx.Tx, channel, payload string) error {
	_, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
	return err
}

// Listen turns notifications on the given channels into signals on the
// returned channel until ctx ends. It reconnects after errors. Signals never
// pile up: one waiting signal stands for any number of notifications, since
// one check handles all pending work. A lost notification only delays work
// until the listener's next timed check.
func Listen(ctx context.Context, pool *pgxpool.Pool, log interface{ Warn(string, ...any) }, channels ...string) <-chan struct{} {
	ch := make(chan struct{}, 1)
	signal := func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	go func() {
		for ctx.Err() == nil {
			err := listenOnce(ctx, pool, channels, signal)
			if ctx.Err() != nil {
				return
			}
			log.Warn("listen for wake-ups", "err", err)
			signal() // something may have been missed while reconnecting
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return ch
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool, channels []string, signal func()) error {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// A listening connection never goes back to the pool.
	conn := pc.Hijack()
	defer conn.Close(context.Background())
	for _, c := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			return err
		}
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		signal()
	}
}
