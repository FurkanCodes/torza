package magnet

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"testing"
)

func TestParseTorrentFile(t *testing.T) {
	info := "d6:lengthi5e4:name8:demo.txt12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae"
	file := "d8:announce19:udp://tr.example:8013:announce-listll19:udp://tr.example:80el20:udp://tr2.example:80ee4:info" + info + "e"
	m, err := ParseTorrentFile([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(info))
	if m.InfoHash != hex.EncodeToString(sum[:]) || m.Name != "demo.txt" || len(m.Trackers) != 2 {
		t.Fatalf("got %+v", m)
	}
	v2 := "d4:infod9:file treede4:name1:x12:meta versioni2eee"
	if _, err := ParseTorrentFile([]byte(v2)); !errors.Is(err, ErrV2Only) {
		t.Errorf("v2-only: %v", err)
	}
	for _, bad := range []string{"", "x", "d4:info", "d4:infoi1ee", "l4:infoe", "d3:abc999999:xe", file + "x"} {
		if _, err := ParseTorrentFile([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
