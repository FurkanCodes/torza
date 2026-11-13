package ingest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"torza/internal/db"
	"torza/internal/magnet"
	"torza/internal/qbit"
	"torza/internal/store"
)

var testNow = time.Unix(1_700_000_000, 0)

// fakeRepo keeps torrents in memory and applies the same status rules as the
// real queries.
type fakeRepo struct {
	torrents map[string]*db.Torrent
	files    map[string][]db.FileInput
	purged   map[string]bool
}

func (r *fakeRepo) TorrentsByStatus(_ context.Context, statuses ...string) ([]db.Torrent, error) {
	var out []db.Torrent
	for _, t := range r.torrents {
		if slices.Contains(statuses, t.Status) {
			out = append(out, *t)
		}
	}
	slices.SortFunc(out, func(a, b db.Torrent) int { return strings.Compare(a.InfoHash, b.InfoHash) })
	return out, nil
}

func (r *fakeRepo) BlockedUnpurged(context.Context) ([]db.Torrent, error) {
	var out []db.Torrent
	for _, t := range r.torrents {
		if t.Status == db.StatusBlocked && !r.purged[t.InfoHash] {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (r *fakeRepo) MarkDownloading(_ context.Context, ih string) error {
	if t := r.torrents[ih]; t.Status == db.StatusQueued {
		t.Status = db.StatusDownloading
	}
	return nil
}

func (r *fakeRepo) UpdateProgress(_ context.Context, ih, name string, size int64, progress float64) error {
	if t := r.torrents[ih]; t.Status == db.StatusDownloading {
		t.Name, t.SizeBytes, t.Progress = name, size, progress
	}
	return nil
}

func (r *fakeRepo) MarkReady(_ context.Context, ih, name string, size int64, files []db.FileInput) error {
	t := r.torrents[ih]
	if t.Status != db.StatusDownloading {
		return db.ErrConflict
	}
	t.Status, t.Name, t.SizeBytes, t.Progress = db.StatusReady, name, size, 1
	r.files[ih] = files
	return nil
}

func (r *fakeRepo) MarkFailed(_ context.Context, ih, reason string) error {
	if t := r.torrents[ih]; t.Status == db.StatusQueued || t.Status == db.StatusDownloading {
		t.Status, t.Error = db.StatusFailed, reason
	}
	return nil
}

func (r *fakeRepo) MarkPurged(_ context.Context, ih string) error {
	r.purged[ih] = true
	return nil
}

// fakeEngine imitates qBittorrent.
type fakeEngine struct {
	torrents  map[string]qbit.Torrent
	files     map[string][]qbit.File
	added     []string
	savePaths map[string]string
	deleted   map[string]bool // info hash → deleteFiles
}

func (e *fakeEngine) Add(_ context.Context, uri, savePath string) error {
	m, err := magnet.Parse(uri)
	if err != nil {
		return err
	}
	e.added = append(e.added, m.InfoHash)
	e.savePaths[m.InfoHash] = savePath
	if _, ok := e.torrents[m.InfoHash]; !ok {
		e.torrents[m.InfoHash] = qbit.Torrent{Hash: m.InfoHash, State: "metaDL", AddedOn: testNow.Unix()}
	}
	return nil
}

func (e *fakeEngine) Info(_ context.Context, hashes []string) ([]qbit.Torrent, error) {
	var out []qbit.Torrent
	for _, h := range hashes {
		if t, ok := e.torrents[h]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (e *fakeEngine) Files(_ context.Context, h string) ([]qbit.File, error) { return e.files[h], nil }

func (e *fakeEngine) Delete(_ context.Context, hashes []string, deleteFiles bool) error {
	for _, h := range hashes {
		delete(e.torrents, h)
		e.deleted[h] = deleteFiles
	}
	return nil
}

func setup(t *testing.T, status string) (*Worker, *fakeRepo, *fakeEngine, store.Store, string) {
	t.Helper()
	st := store.New(t.TempDir())
	if err := st.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ih := strings.Repeat("a", 40)
	repo := &fakeRepo{
		torrents: map[string]*db.Torrent{ih: {InfoHash: ih, Magnet: "magnet:?xt=urn:btih:" + ih, Status: status}},
		files:    map[string][]db.FileInput{},
		purged:   map[string]bool{},
	}
	eng := &fakeEngine{torrents: map[string]qbit.Torrent{}, files: map[string][]qbit.File{}, savePaths: map[string]string{}, deleted: map[string]bool{}}
	w := New(repo, eng, st, Config{MaxTorrentBytes: 1000, MetadataTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return testNow }
	return w, repo, eng, st, ih
}

func tick(t *testing.T, w *Worker) {
	t.Helper()
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func TestDownloadLifecycle(t *testing.T) {
	w, repo, eng, st, ih := setup(t, db.StatusQueued)

	// 1. Queued: handed to qBittorrent with its own save folder.
	tick(t, w)
	if got := repo.torrents[ih].Status; got != db.StatusDownloading {
		t.Fatalf("status = %s, want downloading", got)
	}
	if len(eng.added) != 1 || eng.savePaths[ih] != st.IncomingDir(ih) {
		t.Fatalf("added %v to %q", eng.added, eng.savePaths[ih])
	}
	if _, err := os.Stat(st.IncomingDir(ih)); err != nil {
		t.Fatalf("save folder wasn't created: %v", err)
	}

	// 2. Downloading: progress is recorded.
	eng.torrents[ih] = qbit.Torrent{Hash: ih, Name: "Movie", State: "downloading", Progress: 0.5, TotalSize: 100, AddedOn: testNow.Unix()}
	tick(t, w)
	if tr := repo.torrents[ih]; tr.Progress != 0.5 || tr.SizeBytes != 100 || tr.Name != "Movie" {
		t.Fatalf("progress not recorded: %+v", tr)
	}

	// 3. Finished: moved into the cache, file list recorded, removed from qBittorrent.
	src := filepath.Join(st.IncomingDir(ih), "Movie", "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng.torrents[ih] = qbit.Torrent{Hash: ih, Name: "Movie", State: "stalledUP", Progress: 1, TotalSize: 100, AddedOn: testNow.Unix()}
	eng.files[ih] = []qbit.File{{Name: "Movie/movie.mkv", Size: 100, Progress: 1}}
	tick(t, w)

	if got := repo.torrents[ih].Status; got != db.StatusReady {
		t.Fatalf("status = %s, want ready", got)
	}
	if deleteFiles, ok := eng.deleted[ih]; !ok || deleteFiles {
		t.Fatalf("want removed from qBittorrent keeping files; removed=%v deleteFiles=%v", ok, deleteFiles)
	}
	p, _ := st.FilePath(ih, "Movie/movie.mkv")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file isn't in the cache: %v", err)
	}
	if got := repo.files[ih]; len(got) != 1 || got[0].Path != "Movie/movie.mkv" || got[0].SizeBytes != 100 {
		t.Fatalf("files = %+v", got)
	}
}

func TestStillBusyAtFullProgress(t *testing.T) {
	w, repo, eng, _, ih := setup(t, db.StatusDownloading)
	eng.torrents[ih] = qbit.Torrent{Hash: ih, State: "checkingUP", Progress: 1, TotalSize: 100, AddedOn: testNow.Unix()}
	tick(t, w)
	if got := repo.torrents[ih].Status; got != db.StatusDownloading {
		t.Fatalf("finished while qBittorrent was still checking: %s", got)
	}
}

func TestFailures(t *testing.T) {
	cases := []struct {
		name    string
		torrent qbit.Torrent
		files   []qbit.File
		reason  string
	}{
		{
			name:    "too large",
			torrent: qbit.Torrent{State: "downloading", Progress: 0.1, TotalSize: 5000, AddedOn: testNow.Unix()},
			reason:  "limit",
		},
		{
			name:    "no peers",
			torrent: qbit.Torrent{State: "metaDL", TotalSize: 0, AddedOn: testNow.Add(-2 * time.Hour).Unix()},
			reason:  "no peers",
		},
		{
			name:    "engine error",
			torrent: qbit.Torrent{State: "error", TotalSize: 100, AddedOn: testNow.Unix()},
			reason:  "error",
		},
		{
			name:    "unsafe path",
			torrent: qbit.Torrent{State: "stalledUP", Progress: 1, TotalSize: 100, AddedOn: testNow.Unix()},
			files:   []qbit.File{{Name: "../../etc/evil", Size: 100}},
			reason:  "unsafe",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, repo, eng, _, ih := setup(t, db.StatusDownloading)
			c.torrent.Hash = ih
			eng.torrents[ih] = c.torrent
			eng.files[ih] = c.files
			tick(t, w)
			tr := repo.torrents[ih]
			if tr.Status != db.StatusFailed || !strings.Contains(tr.Error, c.reason) {
				t.Fatalf("got status %s error %q, want failed with %q", tr.Status, tr.Error, c.reason)
			}
			if deleteFiles, ok := eng.deleted[ih]; !ok || !deleteFiles {
				t.Fatalf("partial files weren't deleted from qBittorrent")
			}
		})
	}
}

func TestPurgeBlocked(t *testing.T) {
	w, repo, eng, st, ih := setup(t, db.StatusBlocked)
	eng.torrents[ih] = qbit.Torrent{Hash: ih, State: "downloading"}
	if err := os.MkdirAll(st.TorrentDir(ih), 0o755); err != nil {
		t.Fatal(err)
	}
	tick(t, w)
	if !repo.purged[ih] {
		t.Fatal("blocked torrent wasn't marked purged")
	}
	if deleteFiles, ok := eng.deleted[ih]; !ok || !deleteFiles {
		t.Fatal("blocked torrent wasn't removed from qBittorrent with its files")
	}
	if _, err := os.Stat(st.TorrentDir(ih)); !os.IsNotExist(err) {
		t.Fatal("blocked torrent's cached files still exist")
	}
}

func TestReaddsLostTorrent(t *testing.T) {
	w, _, eng, _, ih := setup(t, db.StatusDownloading)
	tick(t, w)
	if len(eng.added) != 1 || eng.added[0] != ih {
		t.Fatalf("lost torrent wasn't re-added: %v", eng.added)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{999: "999 B", 1500: "1.5 kB", 100e9: "100.0 GB", 2_500_000_000_000: "2.5 TB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
