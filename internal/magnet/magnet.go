// Package magnet turns user input (a magnet link or a bare info hash) into a
// normalised torrent identity.
//
// The info hash is a torrent's fingerprint. Two users adding the same torrent
// produce the same hash, which is what lets the cache keep one copy for
// everyone.
package magnet

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

// ErrInvalid means the input isn't a magnet link or info hash we understand.
var ErrInvalid = errors.New("not a valid magnet link or info hash")

const maxTrackers = 50

// Magnet is a parsed magnet link.
type Magnet struct {
	InfoHash string   // 40 lowercase hex characters (BitTorrent v1)
	Name     string   // display name from dn=, may be empty
	Trackers []string // tracker URLs from tr=
}

// Parse accepts a magnet link ("magnet:?xt=urn:btih:..."), or a bare info hash
// in hex (40 characters) or base32 (32 characters).
func Parse(input string) (Magnet, error) {
	s := strings.TrimSpace(input)
	if h, ok := parseHash(s); ok {
		return Magnet{InfoHash: h}, nil
	}
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") {
		return Magnet{}, ErrInvalid
	}
	q := u.Query()
	var m Magnet
	const prefix = "urn:btih:"
	for _, xt := range q["xt"] {
		if len(xt) > len(prefix) && strings.EqualFold(xt[:len(prefix)], prefix) {
			if h, ok := parseHash(xt[len(prefix):]); ok {
				m.InfoHash = h
				break
			}
		}
	}
	if m.InfoHash == "" {
		return Magnet{}, ErrInvalid
	}
	m.Name = q.Get("dn")
	for _, tr := range q["tr"] {
		if tr != "" && len(m.Trackers) < maxTrackers {
			m.Trackers = append(m.Trackers, tr)
		}
	}
	return m, nil
}

// URI rebuilds a clean magnet link to hand to the torrent engine.
func (m Magnet) URI() string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(m.InfoHash)
	if m.Name != "" {
		b.WriteString("&dn=")
		b.WriteString(url.QueryEscape(m.Name))
	}
	for _, tr := range m.Trackers {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(tr))
	}
	return b.String()
}

// ValidHash reports whether s is a normalised info hash: 40 lowercase hex
// characters.
func ValidHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func parseHash(s string) (string, bool) {
	switch len(s) {
	case 40:
		b, err := hex.DecodeString(s)
		if err != nil {
			return "", false
		}
		return hex.EncodeToString(b), true
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s))
		if err != nil || len(b) != 20 {
			return "", false
		}
		return hex.EncodeToString(b), true
	}
	return "", false
}
