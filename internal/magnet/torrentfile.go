package magnet

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strconv"
)

// ErrTorrentFile means the upload isn't a .torrent file we can read.
var ErrTorrentFile = errors.New("not a valid .torrent file")

// ErrV2Only means a torrent has only a BitTorrent v2 info hash, which Torza
// doesn't support yet.
var ErrV2Only = errors.New("BitTorrent v2-only torrents aren't supported yet; use a v1 or hybrid torrent")

const maxTorrentFile = 10 << 20

// ParseTorrentFile reads a .torrent file. The info hash is the SHA-1 of the
// raw bytes of its info dictionary, exactly as they appear in the file.
func ParseTorrentFile(b []byte) (Magnet, error) {
	if len(b) == 0 || len(b) > maxTorrentFile || b[0] != 'd' {
		return Magnet{}, ErrTorrentFile
	}
	top, end, err := parseDict(b, 0)
	if err != nil || end != len(b) {
		return Magnet{}, ErrTorrentFile
	}
	info, ok := top["info"]
	if !ok || len(info.raw) == 0 || info.raw[0] != 'd' {
		return Magnet{}, ErrTorrentFile
	}
	if _, hasPieces := info.dict["pieces"]; !hasPieces {
		if _, v2 := info.dict["file tree"]; v2 {
			return Magnet{}, ErrV2Only
		}
		return Magnet{}, ErrTorrentFile
	}
	sum := sha1.Sum(info.raw)
	m := Magnet{InfoHash: hex.EncodeToString(sum[:]), Name: string(info.dict["name"].str)}
	add := func(tr []byte) {
		if len(tr) > 0 && len(m.Trackers) < maxTrackers && !containsStr(m.Trackers, string(tr)) {
			m.Trackers = append(m.Trackers, string(tr))
		}
	}
	add(top["announce"].str)
	for _, tier := range top["announce-list"].list {
		for _, tr := range tier.list {
			add(tr.str)
		}
	}
	return m, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// value is a decoded bencode value, keeping its raw bytes.
type value struct {
	raw  []byte
	str  []byte
	list []value
	dict map[string]value
}

const maxDepth = 64

func parseValue(b []byte, i, depth int) (value, int, error) {
	if i >= len(b) || depth > maxDepth {
		return value{}, 0, ErrTorrentFile
	}
	start := i
	switch c := b[i]; {
	case c == 'i':
		end := bytes.IndexByte(b[i:], 'e')
		if end < 0 {
			return value{}, 0, ErrTorrentFile
		}
		if _, err := strconv.ParseInt(string(b[i+1:i+end]), 10, 64); err != nil {
			return value{}, 0, ErrTorrentFile
		}
		i += end + 1
		return value{raw: b[start:i]}, i, nil
	case c == 'l':
		var v value
		i++
		for i < len(b) && b[i] != 'e' {
			e, next, err := parseValue(b, i, depth+1)
			if err != nil {
				return value{}, 0, err
			}
			v.list = append(v.list, e)
			i = next
		}
		if i >= len(b) {
			return value{}, 0, ErrTorrentFile
		}
		i++
		v.raw = b[start:i]
		return v, i, nil
	case c == 'd':
		d, next, err := parseDictDepth(b, i, depth)
		if err != nil {
			return value{}, 0, err
		}
		return value{raw: b[start:next], dict: d}, next, nil
	case c >= '0' && c <= '9':
		colon := bytes.IndexByte(b[i:], ':')
		if colon < 0 {
			return value{}, 0, ErrTorrentFile
		}
		n, err := strconv.Atoi(string(b[i : i+colon]))
		i += colon + 1
		if err != nil || n < 0 || n > len(b)-i {
			return value{}, 0, ErrTorrentFile
		}
		return value{raw: b[start : i+n], str: b[i : i+n]}, i + n, nil
	}
	return value{}, 0, ErrTorrentFile
}

func parseDict(b []byte, i int) (map[string]value, int, error) { return parseDictDepth(b, i, 0) }

func parseDictDepth(b []byte, i, depth int) (map[string]value, int, error) {
	if i >= len(b) || b[i] != 'd' {
		return nil, 0, ErrTorrentFile
	}
	d := map[string]value{}
	i++
	for i < len(b) && b[i] != 'e' {
		k, next, err := parseValue(b, i, depth+1)
		if err != nil || k.str == nil && len(k.raw) == 0 {
			return nil, 0, ErrTorrentFile
		}
		if k.list != nil || k.dict != nil || len(k.raw) == 0 || k.raw[0] < '0' || k.raw[0] > '9' {
			return nil, 0, ErrTorrentFile
		}
		v, after, err := parseValue(b, next, depth+1)
		if err != nil {
			return nil, 0, err
		}
		d[string(k.str)] = v
		i = after
	}
	if i >= len(b) {
		return nil, 0, ErrTorrentFile
	}
	return d, i + 1, nil
}
