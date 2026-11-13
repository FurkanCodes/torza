// Package dbtest gives tests a real, empty, migrated Postgres database of
// their own: a fresh schema in the database TEST_DATABASE_URL names, dropped
// when the test ends. Without the variable the test is skipped, except in CI,
// where a skipped database test would hide the bugs these tests exist for.
//
// Locally:
//
//	docker run -d --rm --name torza-testpg -e POSTGRES_USER=torza -e POSTGRES_PASSWORD=torza \
//	  -e POSTGRES_DB=torza_test -p 127.0.0.1:55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL='postgres://torza:torza@127.0.0.1:55432/torza_test?sslmode=disable' go test ./...
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"torza/internal/db"
)

// Pool returns a pool on a fresh schema with no migrations run.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is not set in CI")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 40
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Repo returns a repo on a fresh, migrated schema.
func Repo(t *testing.T) *db.Repo {
	t.Helper()
	pool := Pool(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return db.NewRepo(pool)
}
