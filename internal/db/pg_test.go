package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"torza/internal/magnet"
)

// testRepo gives a test a migrated, empty database of its own: a fresh
// Postgres schema in the database TEST_DATABASE_URL names, dropped when the
// test ends. Without the variable the test is skipped, except in CI, where a
// skipped database test would hide the bugs these tests exist for.
func testRepo(t *testing.T) *Repo {
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
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return NewRepo(pool)
}

func testUser(t *testing.T, r *Repo, n int) User {
	t.Helper()
	key := []byte(fmt.Sprintf("key-%d-%s", n, t.Name()))
	u, err := r.CreateUser(context.Background(), fmt.Sprintf("u%d@example.test", n), false, key, fmt.Sprintf("p%d", n))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func testHash(n int) string { return fmt.Sprintf("%040x", n+1) }

// Many users adding the same new torrent at the same moment must start one
// download, and every one of them must end up with it in their library.
func TestConcurrentAddsStartOneDownload(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	const users, rounds = 12, 15
	us := make([]User, users)
	for i := range us {
		us[i] = testUser(t, r, i)
	}
	for round := 0; round < rounds; round++ {
		m := magnet.Magnet{InfoHash: testHash(round), Name: "race"}
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			started int
			start   = make(chan struct{})
		)
		for _, u := range us {
			wg.Add(1)
			go func(u User) {
				defer wg.Done()
				<-start
				_, queued, err := r.AddTorrentForUser(ctx, u.ID, m, 0)
				if err != nil {
					t.Errorf("add: %v", err)
					return
				}
				if queued {
					mu.Lock()
					started++
					mu.Unlock()
				}
			}(u)
		}
		close(start)
		wg.Wait()
		if started != 1 {
			t.Fatalf("round %d: %d adds reported starting a download, want 1", round, started)
		}
		for _, u := range us {
			if _, _, err := r.UserTorrent(ctx, u.ID, m.InfoHash); err != nil {
				t.Fatalf("round %d: user %d is missing the torrent: %v", round, u.ID, err)
			}
		}
	}
}

// One user firing many adds of different torrents at once must not get past
// their limit of active downloads.
func TestConcurrentAddsRespectActiveLimit(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	u := testUser(t, r, 1)
	const limit, tries = 3, 20
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		start    = make(chan struct{})
	)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := r.AddTorrentForUser(ctx, u.ID, magnet.Magnet{InfoHash: testHash(100 + i)}, limit)
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else if err != ErrTooManyActive {
				t.Errorf("add: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if accepted != limit {
		t.Fatalf("%d downloads accepted, want exactly the limit of %d", accepted, limit)
	}
}

// A takedown racing an add must win: the torrent ends up blocked, never
// queued again behind the block.
func TestBlockRacingAddEndsBlocked(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	u := testUser(t, r, 1)
	for round := 0; round < 20; round++ {
		h := testHash(500 + round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = r.AddTorrentForUser(ctx, u.ID, magnet.Magnet{InfoHash: h}, 0)
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := r.Block(ctx, h, "test", ""); err != nil {
				t.Errorf("block: %v", err)
			}
		}()
		close(start)
		wg.Wait()
		var status string
		err := r.pool.QueryRow(ctx, `SELECT status FROM torrents WHERE infohash = $1`, h).Scan(&status)
		if err == nil && status != StatusBlocked {
			t.Fatalf("round %d: torrent is %q after block, want blocked", round, status)
		}
	}
}
