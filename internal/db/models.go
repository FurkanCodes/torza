// Package db is Torza's Postgres layer: connecting, migrations and queries.
package db

import (
	"errors"
	"time"
)

// Item statuses. See internal/state for how they change.
const (
	StatusQueued     = "queued"
	StatusActive     = "active"
	StatusFinalizing = "finalizing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	StatusEvicted    = "evicted"
	StatusBlocked    = "blocked"
)

// Source key kinds.
const KindTorrent = "torrent"

// Credential kinds.
const (
	CredAPI     = "api"
	CredStremio = "stremio"
)

// Credential is a key a user holds, without its secret.
type Credential struct {
	ID         int64
	Kind       string
	Label      string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// File states.
const (
	FilePresent = "present"
	FileMissing = "missing"
	FilePending = "pending"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrBlocked       = errors.New("item is blocked")
	ErrTooManyActive = errors.New("too many active downloads")
	// ErrConflict means the item changed status meanwhile (for example it was
	// blocked while a worker held it), so a guarded write did nothing.
	ErrConflict = errors.New("item changed status meanwhile")
)

type User struct {
	ID        int64
	Email     string
	IsAdmin   bool
	CreatedAt time.Time
}

// Limits are a user's effective limits: their own, or the server defaults.
type Limits struct {
	MaxActive    int   // 0 = no limit
	MaxItemBytes int64 // 0 = no limit
	Priority     int
}

// Key is how a request names content: a kind and a normalised value.
type Key struct {
	Kind  string
	Value string
}

type Item struct {
	ID          int64
	Name        string
	Status      string
	Phase       string
	Fetcher     string
	SizeBytes   int64
	Progress    float64
	Error       string
	StartedBy   *int64
	Priority    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
	LastUsedAt  time.Time
}

type File struct {
	ID        int64
	ItemID    int64
	Path      string // relative to the item's folder, "/"-separated
	SizeBytes int64
	State     string
}

// ManifestFile is one file of a finished download.
type ManifestFile struct {
	Path      string
	SizeBytes int64
}

// LibraryItem is an item as one user sees it.
type LibraryItem struct {
	Item
	AddedAt time.Time
	Key     Key // the key this user added it by
}

// Job is an item a fetcher holds, with what it needs to work on it.
type Job struct {
	ItemID       int64
	Key          Key
	Payload      string
	Handle       []byte
	Status       string
	ActiveSince  time.Time
	MaxItemBytes int64 // the starting user's limit; 0 = none
}

// Availability answers "is this cached?" for one key.
type Availability struct {
	Key    Key
	ItemID int64
	Status string // ready, queued, active, finalizing, failed, evicted, blocked, or "" when unknown
	Name   string
	Size   int64
	Files  []File
}

// Usage is what /v1/me reports.
type Usage struct {
	Active       int
	LibraryItems int
	LibraryBytes int64
}
