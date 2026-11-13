package db

import (
	"strings"
	"testing"
)

// These checks guard the parts of the queries that can be tested without a
// running Postgres. The queries themselves need an integration test against a
// real database before the first deploy.

func TestTorrentColumnsMatchScanTargets(t *testing.T) {
	var tr Torrent
	if got, want := len(torrentDest(&tr)), len(torrentFields); got != want {
		t.Fatalf("torrentDest has %d targets but torrentFields has %d columns", got, want)
	}
}

func TestTorrentColsPrefix(t *testing.T) {
	got := torrentCols("t.")
	if !strings.HasPrefix(got, "t.infohash, t.name") || strings.Contains(got, ", infohash") {
		t.Fatalf("torrentCols(\"t.\") = %q", got)
	}
}

func TestMigrationsEmbedded(t *testing.T) {
	b, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"users", "torrents", "torrent_files", "user_torrents", "blocklist"} {
		if !strings.Contains(string(b), "CREATE TABLE "+table+" (") {
			t.Errorf("migration doesn't create %s", table)
		}
	}
}
