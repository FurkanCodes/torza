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
	"torza/internal/links"
	"torza/internal/store"
)

var key = []byte("0123456789abcdef0123456789abcdef")

type fakeRepo struct {
	userID  int64
	file    db.File
	torrent db.Torrent
	touched int
}

func (f *fakeRepo) FileForUser(_ context.Context, userID, fileID int64) (db.File, db.Torrent, error) {
	if userID != f.userID || fileID != f.file.ID {
		return db.File{}, db.Torrent{}, db.ErrNotFound
	}
	return f.file, f.torrent, nil
}

func (f *fakeRepo) TouchTorrent(context.Context, string) error { f.touched++; return nil }

func TestDownload(t *testing.T) {
	st := store.New(t.TempDir())
	ih := strings.Repeat("c", 40)
	p := filepath.Join(st.TorrentDir(ih), "Movie", "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{
		userID:  7,
		file:    db.File{ID: 5, InfoHash: ih, Path: "Movie/movie.mkv", SizeBytes: 10},
		torrent: db.Torrent{InfoHash: ih, Status: db.StatusReady},
	}
	signer := links.NewSigner(key)
	h := New(repo, signer, st, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()

	get := func(token, rangeHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/d/"+token+"/movie.mkv", nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	valid := signer.Sign(links.Claims{FileID: 5, UserID: 7, Expires: time.Now().Add(time.Hour)})

	rec := get(valid, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("full download: %d %q", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "movie.mkv") {
		t.Errorf("Content-Disposition = %q", rec.Header().Get("Content-Disposition"))
	}
	if repo.touched == 0 {
		t.Error("download didn't mark the torrent as recently used")
	}

	rec = get(valid, "bytes=2-5")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range: %d %q %q", rec.Code, rec.Body, rec.Header().Get("Content-Range"))
	}

	expired := signer.Sign(links.Claims{FileID: 5, UserID: 7, Expires: time.Now().Add(-time.Minute)})
	if rec := get(expired, ""); rec.Code != http.StatusGone {
		t.Errorf("expired: %d", rec.Code)
	}
	if rec := get(valid[:len(valid)-3]+"abc", ""); rec.Code != http.StatusForbidden {
		t.Errorf("tampered: %d", rec.Code)
	}
	otherUser := signer.Sign(links.Claims{FileID: 5, UserID: 8, Expires: time.Now().Add(time.Hour)})
	if rec := get(otherUser, ""); rec.Code != http.StatusNotFound {
		t.Errorf("other user: %d", rec.Code)
	}

	repo.torrent.Status = db.StatusBlocked
	if rec := get(valid, ""); rec.Code != http.StatusUnavailableForLegalReasons {
		t.Errorf("blocked: %d", rec.Code)
	}
	repo.torrent.Status = db.StatusEvicted
	if rec := get(valid, ""); rec.Code != http.StatusNotFound {
		t.Errorf("evicted: %d", rec.Code)
	}
}
