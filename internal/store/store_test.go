package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torza/internal/db"
)

func newStore(t *testing.T) Store {
	t.Helper()
	s := New(t.TempDir())
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return s
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPublishOpenRemove(t *testing.T) {
	s := newStore(t)
	src := s.IncomingDir(7)
	write(t, filepath.Join(src, "Movie", "movie.mkv"), "video")
	files := []db.ManifestFile{{Path: "Movie/movie.mkv", SizeBytes: 5}}
	if err := CheckManifest(src, files); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(src, 7); err != nil {
		t.Fatal(err)
	}
	// A second publish after a restart finds the move done.
	if err := s.Publish(src, 7); err != nil {
		t.Fatalf("publish again: %v", err)
	}
	f, err := s.Open(7, "Movie/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := s.Remove(7); err != nil {
		t.Fatal(err)
	}
	if s.Published(7) {
		t.Fatal("store/7 still exists after Remove")
	}
}

func TestOpenRefusesEscapes(t *testing.T) {
	s := newStore(t)
	write(t, filepath.Join(s.ItemDir(1), "ok.txt"), "x")
	write(t, filepath.Join(s.Root, "secret.txt"), "secret")
	if err := os.Symlink(filepath.Join(s.Root, "secret.txt"), filepath.Join(s.ItemDir(1), "link.txt")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../x", "/etc/passwd", "a/../../x", "..", "link.txt"} {
		if f, err := s.Open(1, bad); err == nil {
			f.Close()
			t.Errorf("Open(%q) succeeded", bad)
		}
	}
}

func TestCheckManifestRefusesLinksAndEscapes(t *testing.T) {
	s := newStore(t)
	src := s.IncomingDir(2)
	write(t, filepath.Join(src, "a.txt"), "a")
	if err := os.Symlink("/etc/hosts", filepath.Join(src, "l.txt")); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]db.ManifestFile{
		{{Path: "l.txt"}}, {{Path: "../x"}}, {{Path: "missing.txt"}}, nil,
	} {
		if err := CheckManifest(src, files); err == nil {
			t.Errorf("CheckManifest(%v) accepted", files)
		}
	}
	if err := s.Publish(filepath.Join(s.Root, "store", "9"), 3); !errors.Is(err, ErrBadPath) {
		t.Errorf("Publish from outside incoming/: %v", err)
	}
}

func TestSweepAndAdoptLegacy(t *testing.T) {
	s := newStore(t)
	hash := strings.Repeat("c", 40)
	write(t, filepath.Join(s.Root, "store", hash, "f"), "x")
	if err := s.AdoptLegacy(5, hash); err != nil || !s.Published(5) {
		t.Fatalf("adopt: %v", err)
	}
	write(t, filepath.Join(s.IncomingDir(8), "f"), "x")
	write(t, filepath.Join(s.ItemDir(9), "f"), "x")
	write(t, filepath.Join(s.Root, "store", "notes", "f"), "x")
	removed, err := s.Sweep(map[int64]bool{5: true}, time.Hour, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || !s.Published(5) || !Exists(filepath.Join(s.Root, "store", "notes")) {
		t.Fatalf("removed %v", removed)
	}
	// Folders younger than minAge stay.
	write(t, filepath.Join(s.IncomingDir(10), "f"), "x")
	if removed, _ := s.Sweep(nil, time.Hour, time.Now()); len(removed) != 0 {
		t.Fatalf("swept a new folder: %v", removed)
	}
}
