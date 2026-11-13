package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilePath(t *testing.T) {
	s := New(t.TempDir())
	ih := strings.Repeat("a", 40)

	for _, ok := range []string{"file.iso", "Movie/movie.mkv", "a/b/c.txt"} {
		p, err := s.FilePath(ih, ok)
		if err != nil {
			t.Errorf("FilePath(%q): %v", ok, err)
			continue
		}
		if !strings.HasPrefix(p, s.TorrentDir(ih)+string(filepath.Separator)) {
			t.Errorf("FilePath(%q) = %q, outside the torrent folder", ok, p)
		}
	}
	for _, bad := range []string{"", "../x", "/etc/passwd", "a/../../x", ".."} {
		if _, err := s.FilePath(ih, bad); !errors.Is(err, ErrBadPath) {
			t.Errorf("FilePath(%q) err = %v, want ErrBadPath", bad, err)
		}
	}
	if _, err := s.FilePath("../"+strings.Repeat("a", 37), "file"); !errors.Is(err, ErrBadPath) {
		t.Errorf("accepted an invalid info hash")
	}
}

func TestPublishAndRemove(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ih := strings.Repeat("b", 40)
	src := filepath.Join(s.IncomingDir(ih), "Folder", "file.bin")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Publish(ih); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	p, err := s.FilePath(ih, "Folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "hello" {
		t.Fatalf("published file: %q, %v", b, err)
	}
	if _, err := os.Stat(s.IncomingDir(ih)); !os.IsNotExist(err) {
		t.Fatalf("incoming folder still exists after Publish (err=%v)", err)
	}

	if err := s.Remove(ih); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(s.TorrentDir(ih)); !os.IsNotExist(err) {
		t.Fatalf("torrent folder still exists after Remove (err=%v)", err)
	}
	if err := s.Remove(ih); err != nil {
		t.Fatalf("Remove of a missing torrent should succeed: %v", err)
	}
	if err := s.Publish("not-a-hash"); !errors.Is(err, ErrBadPath) {
		t.Fatalf("Publish(invalid hash) err = %v", err)
	}
}

func TestFreeBytes(t *testing.T) {
	n, err := New(t.TempDir()).FreeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("FreeBytes = %d, want > 0", n)
	}
}
