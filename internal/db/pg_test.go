package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"torza/internal/db"
	"torza/internal/dbtest"
)

var def = db.Limits{}

const fetcher = "qbittorrent"

func user(t *testing.T, r *db.Repo, n int) db.User {
	t.Helper()
	u, err := r.CreateUser(context.Background(), fmt.Sprintf("u%d@example.test", n), false, []byte(fmt.Sprintf("key-%d", n)), "p")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func key(n int) db.Key { return db.Key{Kind: db.KindTorrent, Value: fmt.Sprintf("%040x", n+1)} }

func add(t *testing.T, r *db.Repo, u db.User, k db.Key) db.AddResult {
	t.Helper()
	res, err := r.Add(context.Background(), u.ID, k, "magnet:?xt=urn:btih:"+k.Value, "n", fetcher, def)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// download takes an item from queued to ready with the given files.
func download(t *testing.T, r *db.Repo, files ...string) {
	t.Helper()
	ctx := context.Background()
	jobs, err := r.Claim(ctx, fetcher, 10, 0)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("claim: %v, %d jobs", err, len(jobs))
	}
	for _, j := range jobs {
		var m []db.ManifestFile
		for _, f := range files {
			m = append(m, db.ManifestFile{Path: f, SizeBytes: 10})
		}
		if err := r.BeginFinalize(ctx, j.ItemID, fetcher, "name", m); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkReady(ctx, j.ItemID, fetcher); err != nil {
			t.Fatal(err)
		}
	}
}

// Many users adding the same new key at once start one download, and every
// one of them ends up with it in their library.
func TestConcurrentAddsStartOneDownload(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	us := make([]db.User, 12)
	for i := range us {
		us[i] = user(t, r, i)
	}
	for round := 0; round < 15; round++ {
		k := key(round)
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			started int
			start   = make(chan struct{})
		)
		for _, u := range us {
			wg.Go(func() {
				<-start
				res, err := r.Add(ctx, u.ID, k, "m", "race", fetcher, def)
				if err != nil {
					t.Errorf("add: %v", err)
					return
				}
				if res.Started {
					mu.Lock()
					started++
					mu.Unlock()
				}
			})
		}
		close(start)
		wg.Wait()
		if started != 1 {
			t.Fatalf("round %d: %d adds started a download, want 1", round, started)
		}
		for _, u := range us {
			lib, err := r.Library(ctx, u.ID)
			if err != nil || len(lib) != round+1 {
				t.Fatalf("round %d: user %d has %d items (%v)", round, u.ID, len(lib), err)
			}
		}
	}
}

// One user firing many adds at once can't get past their active limit.
func TestConcurrentAddsRespectActiveLimit(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	u := user(t, r, 1)
	const limit = 3
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		start    = make(chan struct{})
	)
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			<-start
			_, err := r.Add(ctx, u.ID, key(100+i), "m", "", fetcher, db.Limits{MaxActive: limit})
			switch {
			case err == nil:
				mu.Lock()
				accepted++
				mu.Unlock()
			case !errors.Is(err, db.ErrTooManyActive):
				t.Errorf("add: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if accepted != limit {
		t.Fatalf("%d downloads accepted, want exactly the limit of %d", accepted, limit)
	}
	// Adding something already cached is always free.
	other := user(t, r, 2)
	add(t, r, other, key(999))
	download(t, r, "f")
	if _, err := r.Add(ctx, u.ID, key(999), "m", "", fetcher, db.Limits{MaxActive: limit}); err != nil {
		t.Fatalf("adding a cached item at the limit: %v", err)
	}
}

// A block waits for an add holding the key's lock, then wins: the item ends
// blocked, its files missing, and adding it again is refused. Both orders.
func TestBlockAndAddSerialise(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	u := user(t, r, 1)
	for round, blockFirst := range []bool{false, true} {
		k := key(500 + round)
		if blockFirst {
			if _, err := r.Block(ctx, k, "test", "", u.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Add(ctx, u.ID, k, "m", "", fetcher, def); !errors.Is(err, db.ErrBlocked) {
				t.Fatalf("add after block: %v", err)
			}
			continue
		}
		res := add(t, r, u, k)
		download(t, r, "a.mkv")
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Go(func() { _, _ = r.Add(ctx, u.ID, k, "m", "", fetcher, def) })
		}
		wg.Go(func() {
			if _, err := r.Block(ctx, k, "test", "", u.ID); err != nil {
				t.Errorf("block: %v", err)
			}
		})
		wg.Wait()
		it, files, err := r.LibraryItem(ctx, u.ID, res.Item.ID)
		if err != nil || it.Status != db.StatusBlocked || len(files) != 0 {
			t.Fatalf("after block: status %q, %d files, %v", it.Status, len(files), err)
		}
		if _, err := r.Add(ctx, u.ID, k, "m", "", fetcher, def); !errors.Is(err, db.ErrBlocked) {
			t.Fatalf("add after block: %v", err)
		}
	}
}

// File IDs survive eviction and a new download; a file missing from the new
// manifest stays missing.
func TestFileIDsStableAcrossEviction(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	u := user(t, r, 1)
	res := add(t, r, u, key(1))
	download(t, r, "a.mkv", "b.srt")
	_, before, _ := r.LibraryItem(ctx, u.ID, res.Item.ID)
	if err := r.MarkEvicted(ctx, res.Item.ID); err != nil {
		t.Fatal(err)
	}
	again, err := r.Requeue(ctx, u.ID, res.Item.ID, fetcher, def)
	if err != nil || again.Status != db.StatusQueued {
		t.Fatalf("requeue: %v %q", err, again.Status)
	}
	download(t, r, "a.mkv")
	_, after, _ := r.LibraryItem(ctx, u.ID, res.Item.ID)
	if len(before) != 2 || len(after) != 1 || after[0].ID != before[0].ID || after[0].Path != "a.mkv" {
		t.Fatalf("before %+v, after %+v", before, after)
	}
}

// Two claimers never get the same item.
func TestClaimsAreDisjoint(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	u := user(t, r, 1)
	for i := 0; i < 30; i++ {
		add(t, r, u, key(i))
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[int64]int{}
	)
	for c := 0; c < 6; c++ {
		wg.Go(func() {
			jobs, err := r.Claim(ctx, fetcher, 10, 0)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			for _, j := range jobs {
				seen[j.ItemID]++
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(seen) != 30 {
		t.Fatalf("claimed %d distinct items, want 30", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("item %d claimed %d times", id, n)
		}
	}
}

// A guarded write after a takedown does nothing.
func TestWritesAfterBlockConflict(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	u := user(t, r, 1)
	k := key(1)
	add(t, r, u, k)
	jobs, _ := r.Claim(ctx, fetcher, 1, 0)
	if _, err := r.Block(ctx, k, "x", "", u.ID); err != nil {
		t.Fatal(err)
	}
	id := jobs[0].ItemID
	if err := r.Progress(ctx, id, fetcher, "downloading", "", 10, .5); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("progress after block: %v", err)
	}
	if err := r.BeginFinalize(ctx, id, fetcher, "", []db.ManifestFile{{Path: "a", SizeBytes: 1}}); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("finalize after block: %v", err)
	}
}

func TestCheck(t *testing.T) {
	r := dbtest.Repo(t)
	ctx := context.Background()
	u := user(t, r, 1)
	add(t, r, u, key(1))
	download(t, r, "movie.mkv")
	add(t, r, u, key(2))
	if _, err := r.Block(ctx, key(3), "x", "", u.ID); err != nil {
		t.Fatal(err)
	}
	got, err := r.Check(ctx, []db.Key{key(1), key(2), key(3), key(4), key(1)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{db.StatusReady, db.StatusQueued, db.StatusBlocked, "", db.StatusReady}
	for i, a := range got {
		if a.Status != want[i] {
			t.Errorf("key %d: %q, want %q", i, a.Status, want[i])
		}
	}
	if len(got[0].Files) != 1 || len(got[4].Files) != 1 || len(got[1].Files) != 0 {
		t.Errorf("files: %d %d %d", len(got[0].Files), len(got[4].Files), len(got[1].Files))
	}
}

// The conversion from the torrent-only schema keeps users, keys, file IDs
// and libraries.
func TestConvertTorrentSchema(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	old, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	h1, h2 := key(1).Value, key(2).Value
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		string(old),
		`INSERT INTO schema_migrations (version) VALUES ('0001_init')`,
		`INSERT INTO users (id, email, api_key_hash, api_key_prefix) VALUES (1, 'a@x', 'k1', 'p1'), (2, 'b@x', 'k2', 'p2')`,
		`INSERT INTO torrents (infohash, name, magnet, status, size_bytes, progress, requested_by) VALUES
			('` + h1 + `', 'One', 'magnet:?xt=urn:btih:` + h1 + `', 'ready', 10, 1, 1),
			('` + h2 + `', 'Two', 'magnet:?xt=urn:btih:` + h2 + `', 'downloading', 20, .5, 2)`,
		`INSERT INTO torrent_files (id, infohash, path, size_bytes) VALUES (41, '` + h1 + `', 'one.mkv', 10)`,
		`INSERT INTO user_torrents (user_id, infohash) VALUES (1, '` + h1 + `'), (2, '` + h1 + `'), (2, '` + h2 + `')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q[:min(40, len(q))], err)
		}
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	r := db.NewRepo(pool)
	u, err := r.UserByKeyHash(ctx, []byte("k2"))
	if err != nil || u.ID != 2 {
		t.Fatalf("API key after conversion: %v %v", u, err)
	}
	lib, err := r.Library(ctx, 2)
	if err != nil || len(lib) != 2 {
		t.Fatalf("library: %d items, %v", len(lib), err)
	}
	f, it, err := r.FileForUser(ctx, 1, 41)
	if err != nil || f.State != db.FilePresent || it.Status != db.StatusReady || it.Name != "One" {
		t.Fatalf("file 41: %+v %+v %v", f, it, err)
	}
	held, err := r.Held(ctx, fetcher, 0)
	if err != nil || len(held) != 1 || held[0].Key.Value != h2 || len(held[0].Handle) == 0 {
		t.Fatalf("downloading torrent should be held with a handle: %+v %v", held, err)
	}
	folders, err := r.LegacyFolders(ctx)
	if err != nil || folders[it.ID] != h1 || len(folders) != 1 {
		t.Fatalf("legacy folders: %v %v", folders, err)
	}
	// New file IDs continue after the converted ones.
	res, err := r.Add(ctx, 1, key(9), "m", "", fetcher, def)
	if err != nil {
		t.Fatal(err)
	}
	download(t, r, "new.mkv")
	_, files, _ := r.LibraryItem(ctx, 1, res.Item.ID)
	if len(files) != 1 || files[0].ID <= 41 {
		t.Fatalf("new file IDs: %+v", files)
	}
	_ = time.Now
}
