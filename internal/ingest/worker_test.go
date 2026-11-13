package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"torza/internal/db"
	"torza/internal/dbtest"
	"torza/internal/source"
	"torza/internal/store"
)

// fakeSource is a two-step fetcher: Start writes the files at once, and the
// test says when Poll reports them done. Its only state outside the test's
// control is the files on disk, like a real source after a restart.
type fakeSource struct {
	mu        sync.Mutex
	files     map[string]string // what Start writes, by path
	size      int64             // what Poll reports as the total
	done      bool
	cancelled []int64
	released  []int64
	symlink   bool // write a symbolic link instead of a file
}

func (f *fakeSource) Name() string { return "qbittorrent" }

func (f *fakeSource) Start(_ context.Context, j source.Job) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p, body := range f.files {
		full := filepath.Join(j.Dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if f.symlink {
			if err := os.Symlink("/etc/hosts", full); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	return []byte(`{"dir":"` + j.Dir + `"}`), nil
}

func (f *fakeSource) Poll(context.Context, source.Job) (source.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return source.Report{Phase: "downloading", Name: "Demo", BytesTotal: f.size, BytesDone: f.size / 2, Done: f.done}, nil
}

func (f *fakeSource) Finalize(_ context.Context, j source.Job) (source.Manifest, error) {
	m := source.Manifest{Root: j.Dir, Name: "Demo"}
	for p, body := range f.files {
		m.Files = append(m.Files, db.ManifestFile{Path: p, SizeBytes: int64(len(body))})
	}
	return m, nil
}

func (f *fakeSource) Root(j source.Job) (string, error) { return j.Dir, nil }

func (f *fakeSource) Release(_ context.Context, j source.Job) error {
	f.released = append(f.released, j.ItemID)
	return nil
}

func (f *fakeSource) Cancel(_ context.Context, j source.Job, deleteData bool) error {
	f.cancelled = append(f.cancelled, j.ItemID)
	if deleteData {
		return os.RemoveAll(j.Dir)
	}
	return nil
}

type rig struct {
	repo *db.Repo
	src  *fakeSource
	st   store.Store
	w    *Worker
	user db.User
}

func newRig(t *testing.T) *rig {
	t.Helper()
	repo := dbtest.Repo(t)
	st := store.New(t.TempDir())
	if err := st.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{files: map[string]string{"Demo/demo.mkv": "video", "Demo/demo.srt": "subs"}, size: 10}
	u, err := repo.CreateUser(context.Background(), "u@example.test", false, []byte("k"), "p")
	if err != nil {
		t.Fatal(err)
	}
	w := New(repo, src, st, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &rig{repo: repo, src: src, st: st, w: w, user: u}
}

func (r *rig) add(t *testing.T, n int) db.AddResult {
	t.Helper()
	k := db.Key{Kind: db.KindTorrent, Value: fmt.Sprintf("%040x", n)}
	res, err := r.repo.Add(context.Background(), r.user.ID, k, "magnet:?xt=urn:btih:"+k.Value, "", "qbittorrent", db.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (r *rig) tick(t *testing.T) {
	t.Helper()
	if err := r.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) item(t *testing.T, id int64) (db.Item, []db.File) {
	t.Helper()
	it, files, err := r.repo.LibraryItem(context.Background(), r.user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return it, files
}

func TestDownloadBecomesReady(t *testing.T) {
	r := newRig(t)
	res := r.add(t, 1)
	r.tick(t)
	if it, _ := r.item(t, res.Item.ID); it.Status != db.StatusActive || it.Progress != .5 {
		t.Fatalf("after first tick: %q %.2f", it.Status, it.Progress)
	}
	r.src.done = true
	r.tick(t)
	it, files := r.item(t, res.Item.ID)
	if it.Status != db.StatusReady || len(files) != 2 || it.SizeBytes != 9 {
		t.Fatalf("status %q, %d files, %d bytes", it.Status, len(files), it.SizeBytes)
	}
	f, err := r.st.Open(it.ID, "Demo/demo.mkv")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if len(r.src.released) != 1 {
		t.Fatal("the source wasn't told to release the files")
	}
}

// A crash after the manifest was stored, at each point of the finish order.
func TestRecoverFinalizing(t *testing.T) {
	for _, c := range []struct {
		name      string
		published bool // the rename into store/ happened
		vanished  bool // the download folder is gone too
		want      string
	}{
		{"renamed, not marked ready", true, false, db.StatusReady},
		{"not renamed yet", false, false, db.StatusReady},
		{"files gone", false, true, db.StatusQueued},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			ctx := context.Background()
			res := r.add(t, 1)
			r.tick(t)
			jobs, err := r.repo.Held(ctx, "qbittorrent", 0)
			if err != nil || len(jobs) != 1 {
				t.Fatal(err)
			}
			m, _ := r.src.Finalize(ctx, r.w.job(jobs[0]))
			if err := r.repo.BeginFinalize(ctx, res.Item.ID, "qbittorrent", "Demo", m.Files); err != nil {
				t.Fatal(err)
			}
			switch {
			case c.published:
				if err := r.st.Publish(r.st.IncomingDir(res.Item.ID), res.Item.ID); err != nil {
					t.Fatal(err)
				}
			case c.vanished:
				_ = os.RemoveAll(r.st.IncomingDir(res.Item.ID))
			}
			r.tick(t) // "restart": the next tick recovers from the database and disk
			it, files := r.item(t, res.Item.ID)
			if it.Status != c.want && !(c.want == db.StatusQueued && it.Status == db.StatusActive) {
				t.Fatalf("status %q, want %q", it.Status, c.want)
			}
			if c.want == db.StatusReady && (len(files) != 2 || !r.st.Published(it.ID)) {
				t.Fatalf("%d files, published %v", len(files), r.st.Published(it.ID))
			}
		})
	}
}

func TestBlockedDownloadIsStoppedAndDeleted(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	res := r.add(t, 1)
	r.tick(t)
	k := db.Key{Kind: db.KindTorrent, Value: fmt.Sprintf("%040x", 1)}
	if _, err := r.repo.Block(ctx, k, "takedown", "", r.user.ID); err != nil {
		t.Fatal(err)
	}
	r.src.done = true
	r.tick(t)
	if len(r.src.cancelled) != 1 || store.Exists(r.st.IncomingDir(res.Item.ID)) || r.st.Published(res.Item.ID) {
		t.Fatalf("cancelled %v; incoming exists %v", r.src.cancelled, store.Exists(r.st.IncomingDir(res.Item.ID)))
	}
	held, _ := r.repo.BlockedHeld(ctx, "qbittorrent")
	if len(held) != 0 {
		t.Fatal("blocked item still has a handle after purge")
	}
}

func TestOverSizeLimitFails(t *testing.T) {
	r := newRig(t)
	r.w.cfg.MaxItemBytes = 5
	res := r.add(t, 1)
	r.tick(t)
	it, _ := r.item(t, res.Item.ID)
	if it.Status != db.StatusFailed || it.Error == "" || store.Exists(r.st.IncomingDir(res.Item.ID)) {
		t.Fatalf("status %q, error %q", it.Status, it.Error)
	}
}

func TestUnsafeFilesFail(t *testing.T) {
	r := newRig(t)
	r.src.symlink, r.src.done = true, true
	res := r.add(t, 1)
	r.tick(t)
	it, _ := r.item(t, res.Item.ID)
	if it.Status != db.StatusFailed || r.st.Published(res.Item.ID) {
		t.Fatalf("status %q, published %v", it.Status, r.st.Published(res.Item.ID))
	}
}
