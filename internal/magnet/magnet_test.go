package magnet

import (
	"encoding/base32"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

const hexHash = "c9e15763f722f23e98a29decdfae341b98d53056"

func base32Hash(t *testing.T) string {
	t.Helper()
	raw, err := hex.DecodeString(hexHash)
	if err != nil {
		t.Fatal(err)
	}
	return base32.StdEncoding.EncodeToString(raw)
}

func TestParse(t *testing.T) {
	b32 := base32Hash(t)
	cases := []struct {
		name         string
		in           string
		wantHash     string
		wantName     string
		wantTrackers []string
		wantErr      bool
	}{
		{name: "bare hex", in: hexHash, wantHash: hexHash},
		{name: "bare hex uppercase", in: strings.ToUpper(hexHash), wantHash: hexHash},
		{name: "bare base32", in: b32, wantHash: hexHash},
		{name: "surrounding whitespace", in: "  " + hexHash + "\n", wantHash: hexHash},
		{
			name:         "full magnet",
			in:           "magnet:?xt=urn:btih:" + strings.ToUpper(hexHash) + "&dn=Big+Buck+Bunny&tr=udp%3A%2F%2Ftracker.example%3A1337%2Fannounce",
			wantHash:     hexHash,
			wantName:     "Big Buck Bunny",
			wantTrackers: []string{"udp://tracker.example:1337/announce"},
		},
		{name: "magnet with base32 hash", in: "magnet:?xt=urn:btih:" + b32, wantHash: hexHash},
		{name: "uppercase URN prefix", in: "magnet:?xt=URN:BTIH:" + hexHash, wantHash: hexHash},
		{name: "wrong scheme", in: "http://example.com/?xt=urn:btih:" + hexHash, wantErr: true},
		{name: "no btih", in: "magnet:?xt=urn:sha1:abc", wantErr: true},
		{name: "short hash", in: "magnet:?xt=urn:btih:abc", wantErr: true},
		{name: "non-hex 40 chars", in: strings.Repeat("z", 40), wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := Parse(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error", c.in, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.in, err)
			}
			if m.InfoHash != c.wantHash || m.Name != c.wantName || !slices.Equal(m.Trackers, c.wantTrackers) {
				t.Fatalf("Parse(%q) = %+v, want hash %s name %q trackers %v", c.in, m, c.wantHash, c.wantName, c.wantTrackers)
			}
		})
	}
}

func TestURIRoundTrip(t *testing.T) {
	in := Magnet{InfoHash: hexHash, Name: "a b & c", Trackers: []string{"udp://t.example:1/announce", "https://t2.example/announce?k=v"}}
	out, err := Parse(in.URI())
	if err != nil {
		t.Fatal(err)
	}
	if out.InfoHash != in.InfoHash || out.Name != in.Name || !slices.Equal(out.Trackers, in.Trackers) {
		t.Fatalf("round trip changed the magnet: %+v -> %+v", in, out)
	}
}

func TestTrackerLimit(t *testing.T) {
	uri := "magnet:?xt=urn:btih:" + hexHash + strings.Repeat("&tr=udp%3A%2F%2Ft%3A1", maxTrackers+10)
	m, err := Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Trackers) != maxTrackers {
		t.Fatalf("kept %d trackers, want %d", len(m.Trackers), maxTrackers)
	}
}

func TestValidHash(t *testing.T) {
	if !ValidHash(hexHash) {
		t.Error("rejected a valid hash")
	}
	for _, bad := range []string{"", strings.ToUpper(hexHash), hexHash[:39], hexHash + "0", strings.Repeat("g", 40)} {
		if ValidHash(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
