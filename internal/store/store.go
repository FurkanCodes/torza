// Package store manages the cache on disk.
//
// Layout under the data directory:
//
//	incoming/<infohash>/...  torrents qBittorrent is still downloading
//	store/<infohash>/...     finished torrents, served to users
//
// Moving a finished torrent from incoming/ to store/ is a single rename on the
// same disk, so it's instant and a half-moved torrent can never be served.
package store

import (
	"errors"
	"os"
	"path/filepath"

	"torza/internal/magnet"
)

// ErrBadPath means a path would escape its torrent's folder, or the info hash
// isn't valid.
var ErrBadPath = errors.New("unsafe file path")

// Store is the data directory.
type Store struct{ Root string }

func New(root string) Store { return Store{Root: root} }

// EnsureDirs creates incoming/ and store/ if they don't exist.
func (s Store) EnsureDirs() error {
	for _, d := range []string{s.incomingRoot(), s.storeRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// IncomingDir is where qBittorrent saves a torrent while it downloads.
func (s Store) IncomingDir(infohash string) string {
	return filepath.Join(s.incomingRoot(), infohash)
}

// TorrentDir is where a finished torrent lives.
func (s Store) TorrentDir(infohash string) string {
	return filepath.Join(s.storeRoot(), infohash)
}

// FilePath returns the on-disk path of a file inside a finished torrent.
// File names come from torrents, which anyone can create, so it refuses paths
// that would escape the torrent's folder, like "../../etc/passwd".
func (s Store) FilePath(infohash, rel string) (string, error) {
	if !magnet.ValidHash(infohash) {
		return "", ErrBadPath
	}
	rel = filepath.FromSlash(rel)
	if !filepath.IsLocal(rel) {
		return "", ErrBadPath
	}
	return filepath.Join(s.TorrentDir(infohash), rel), nil
}

// Publish moves a finished download from incoming/ into the served cache.
func (s Store) Publish(infohash string) error {
	if !magnet.ValidHash(infohash) {
		return ErrBadPath
	}
	dst := s.TorrentDir(infohash)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(s.IncomingDir(infohash), dst)
}

// Remove deletes everything stored for a torrent. Missing folders are fine.
func (s Store) Remove(infohash string) error {
	if !magnet.ValidHash(infohash) {
		return ErrBadPath
	}
	return errors.Join(os.RemoveAll(s.IncomingDir(infohash)), os.RemoveAll(s.TorrentDir(infohash)))
}

func (s Store) incomingRoot() string { return filepath.Join(s.Root, "incoming") }
func (s Store) storeRoot() string    { return filepath.Join(s.Root, "store") }
