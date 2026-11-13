package stream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torza/internal/db"
	"torza/internal/dbtest"
	"torza/internal/links"
	"torza/internal/service"
	"torza/internal/store"
)

var signingKey = []byte("0123456789abcdef0123456789abcdef")

type env struct {
	h      http.Handler
	repo   *db.Repo
	svc    *service.Service
	st     store.Store
	user   db.User
	itemID int64
	fileID int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	repo := dbtest.Repo(t)
	st := store.New(t.TempDir())
	if err := st.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	u, err := repo.CreateUser(ctx, "u@example.test", false, []byte("k"), "p")
	if err != nil {
		t.Fatal(err)
	}
	k := db.Key{Kind: db.KindTorrent, Value: strings.Repeat("a", 40)}
	res, err := repo.Add(ctx, u.ID, k, "m", "", "qbittorrent", db.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := repo.Claim(ctx, "qbittorrent", 1, 0)
	body := strings.Repeat("0123456789", 1000)
	dir := st.IncomingDir(jobs[0].ItemID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginFinalize(ctx, res.Item.ID, "qbittorrent", "Movie", []db.ManifestFile{{Path: "movie.mkv", SizeBytes: int64(len(body))}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(dir, res.Item.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkReady(ctx, res.Item.ID, "qbittorrent"); err != nil {
		t.Fatal(err)
	}
	_, files, _ := repo.LibraryItem(ctx, u.ID, res.Item.ID)
	signer := links.NewSigner(signingKey)
	svc := service.New(repo, signer, service.Config{PublicURL: "http://dl.test", LinkTTL: time.Hour})
	h := New(svc, signer, st, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	return &env{h: h, repo: repo, svc: svc, st: st, user: u, itemID: res.Item.ID, fileID: files[0].ID}
}

func (e *env) get(t *testing.T, method, url, rng string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, strings.TrimPrefix(url, "http://dl.test"), nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) link(t *testing.T) string {
	t.Helper()
	l, _, err := e.svc.FileLink(context.Background(), e.user.ID, e.fileID)
	if err != nil {
		t.Fatal(err)
	}
	return l.URL
}

func TestServesRanges(t *testing.T) {
	e := newEnv(t)
	url := e.link(t)
	rec := e.get(t, "GET", url, "bytes=10-19")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123456789" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	if rec := e.get(t, "GET", url, ""); rec.Code != 200 || rec.Body.Len() != 10000 {
		t.Fatalf("whole file: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	url := e.link(t)
	if rec := e.get(t, "GET", "/d/bogus/movie.mkv", ""); rec.Code != http.StatusForbidden {
		t.Errorf("bad token: %d", rec.Code)
	}
	expired := links.NewSigner(signingKey).Sign(links.Claims{FileID: e.fileID, UserID: e.user.ID, Expires: time.Now().Add(-time.Minute)})
	if rec := e.get(t, "GET", "/d/"+expired+"/movie.mkv", ""); rec.Code != http.StatusGone {
		t.Errorf("expired: %d", rec.Code)
	}
	if err := e.repo.RemoveFromLibrary(ctx, e.user.ID, e.itemID); err != nil {
		t.Fatal(err)
	}
	if rec := e.get(t, "GET", url, ""); rec.Code != http.StatusNotFound {
		t.Errorf("after removal from library: %d", rec.Code)
	}
}

func TestBlockedIs451(t *testing.T) {
	e := newEnv(t)
	url := e.link(t)
	if _, err := e.repo.Block(context.Background(), db.Key{Kind: db.KindTorrent, Value: strings.Repeat("a", 40)}, "x", "", e.user.ID); err != nil {
		t.Fatal(err)
	}
	if rec := e.get(t, "GET", url, ""); rec.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("blocked: %d", rec.Code)
	}
}

func TestOnlyRealReadsCountAsUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	lastUsed := func() time.Time {
		it, _, _ := e.repo.LibraryItem(ctx, e.user.ID, e.itemID)
		return it.LastUsedAt
	}
	if _, err := e.repo.Pool().Exec(ctx, `UPDATE items SET last_used_at = now() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	url := e.link(t)
	before := lastUsed()
	e.get(t, "HEAD", url, "")
	e.get(t, "GET", url, "bytes=0-9")
	if !lastUsed().Equal(before) {
		t.Fatal("HEAD or a 10-byte range counted as use")
	}
	e.get(t, "GET", url, "") // the whole (small) file
	if !lastUsed().After(before) {
		t.Fatal("reading the whole file didn't count as use")
	}
}
