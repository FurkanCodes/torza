// Package store manages the cache on disk.
//
// Layout under the data directory:
//
//	incoming/<item id>/...  downloads in progress
//	store/<item id>/...     finished items, served to users
//
// Moving a finished download from incoming/ to store/ is a single rename on
// the same disk, so it is instant and a half-moved item is never served.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"torza/internal/db"
	"torza/internal/magnet"
)

// ErrBadPath means a path would escape its item's folder.
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

// IncomingDir is where an item downloads to.
func (s Store) IncomingDir(itemID int64) string {
	return filepath.Join(s.incomingRoot(), strconv.FormatInt(itemID, 10))
}

// ItemDir is where a finished item lives.
func (s Store) ItemDir(itemID int64) string {
	return filepath.Join(s.storeRoot(), strconv.FormatInt(itemID, 10))
}

// Open opens a file of a finished item for reading. File names come from
// torrents, which anyone can make, so it opens through an os.Root: no path,
// "..", or symbolic link can reach outside the item's folder.
func (s Store) Open(itemID int64, rel string) (*os.File, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, ErrBadPath
	}
	root, err := os.OpenRoot(s.ItemDir(itemID))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.FromSlash(rel))
}

// CheckManifest refuses a manifest whose files aren't plain files inside
// src: no escaping paths, links, devices or folders.
func CheckManifest(src string, files []db.ManifestFile) error {
	if len(files) == 0 {
		return errors.New("the download has no files")
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range files {
		rel := filepath.FromSlash(f.Path)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("%w: %q", ErrBadPath, f.Path)
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %q is not a regular file", ErrBadPath, f.Path)
		}
	}
	return nil
}

// Publish moves a finished download from src (a folder under incoming/)
// into store/<id>. It never deletes an existing store/<id>: if that exists,
// the move already happened before a restart, and leftovers in src go.
func (s Store) Publish(src string, itemID int64) error {
	if err := s.underIncoming(src); err != nil {
		return err
	}
	dst := s.ItemDir(itemID)
	if _, err := os.Stat(dst); err == nil {
		return os.RemoveAll(src)
	}
	return os.Rename(src, dst)
}

// Published reports whether store/<id> exists.
func (s Store) Published(itemID int64) bool {
	info, err := os.Stat(s.ItemDir(itemID))
	return err == nil && info.IsDir()
}

// Exists reports whether a folder exists.
func Exists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// Remove deletes everything stored for an item. Missing folders are fine.
func (s Store) Remove(itemID int64) error {
	return errors.Join(os.RemoveAll(s.IncomingDir(itemID)), os.RemoveAll(s.ItemDir(itemID)))
}

// AdoptLegacy renames a finished item's folder from the old layout
// (store/<info hash>) to store/<id>.
func (s Store) AdoptLegacy(itemID int64, folder string) error {
	if !magnet.ValidHash(folder) {
		return ErrBadPath
	}
	old := filepath.Join(s.storeRoot(), folder)
	if !Exists(old) || Exists(s.ItemDir(itemID)) {
		return nil
	}
	return os.Rename(old, s.ItemDir(itemID))
}

// Sweep deletes item folders in incoming/ and store/ whose item isn't kept,
// if they are older than minAge (so a folder made a moment ago by a new
// download is safe). Folders not named by an item ID are left alone.
func (s Store) Sweep(keep map[int64]bool, minAge time.Duration, now time.Time) (removed []string, err error) {
	var errs []error
	for _, base := range []string{s.incomingRoot(), s.storeRoot()} {
		entries, err := os.ReadDir(base)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			id, err := strconv.ParseInt(e.Name(), 10, 64)
			if err != nil || id <= 0 || keep[id] {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < minAge {
				continue
			}
			p := filepath.Join(base, e.Name())
			if err := os.RemoveAll(p); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, p)
		}
	}
	return removed, errors.Join(errs...)
}

func (s Store) underIncoming(dir string) error {
	rel, err := filepath.Rel(s.incomingRoot(), dir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%w: %s is not under incoming/", ErrBadPath, dir)
	}
	return nil
}

func (s Store) incomingRoot() string { return filepath.Join(s.Root, "incoming") }
func (s Store) storeRoot() string    { return filepath.Join(s.Root, "store") }
