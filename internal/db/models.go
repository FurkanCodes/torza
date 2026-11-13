// Package db is Torza's Postgres layer: connecting, migrations and queries.
package db

import (
	"errors"
	"time"
)

// Torrent statuses. See ARCHITECTURE.md for how they change.
const (
	StatusQueued      = "queued"
	StatusDownloading = "downloading"
	StatusReady       = "ready"
	StatusFailed      = "failed"
	StatusEvicted     = "evicted"
	StatusBlocked     = "blocked"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrBlocked       = errors.New("torrent is blocked")
	ErrTooManyActive = errors.New("too many active downloads")
	ErrConflict      = errors.New("torrent changed status meanwhile")
)

type User struct {
	ID        int64
	Email     string
	IsAdmin   bool
	CreatedAt time.Time
}

type Torrent struct {
	InfoHash       string
	Name           string
	Magnet         string
	Status         string
	SizeBytes      int64
	Progress       float64
	Error          string
	RequestedBy    *int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	LastAccessedAt time.Time
}

type File struct {
	ID        int64
	InfoHash  string
	Path      string // relative to the torrent's folder, "/"-separated
	SizeBytes int64
}

// FileInput describes a file when a torrent finishes.
type FileInput struct {
	Path      string
	SizeBytes int64
}
