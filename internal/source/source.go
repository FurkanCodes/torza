// Package source is the interface every way of filling the cache fits:
// qBittorrent today; Real-Debrid, TorBox, hosters and Usenet in later stages.
//
// A source works on one item at a time per Job. Everything it needs to pick
// its work up again after a crash lives in the Handle, which ingest stores on
// the item, and in the item's incoming folder.
package source

import (
	"context"
	"errors"
	"fmt"

	"torza/internal/db"
)

// Job is one item a source is asked to fill.
type Job struct {
	ItemID  int64
	Key     db.Key
	Payload string // what the key came with: a magnet link
	Handle  []byte // nil before Start
	Dir     string // absolute incoming/<item id> folder to download into
}

// Report is how far a job is.
type Report struct {
	Phase      string // starting, waiting_remote, downloading, verifying, ...
	Name       string // the content's name, once known
	BytesDone  int64
	BytesTotal int64 // 0 while unknown (a magnet waiting for metadata)
	Done       bool  // every file is complete and won't change
}

// Manifest is the final list of files of a finished job, relative to Root.
type Manifest struct {
	Root  string // absolute folder holding the files
	Name  string
	Files []db.ManifestFile
}

// Source is one fetcher.
//
// Rules every source follows:
//  1. Poll survives restarts: a new instance rebuilds its state from the Handle
//     and the files on disk.
//  2. File rows come only from Finalize. Earlier file lists are estimates.
//  3. Release and Cancel can be called again after succeeding.
//  4. Errors say what to do next with an *Error of a Class.
type Source interface {
	// Name is the fetcher name stored on items: "qbittorrent", ...
	Name() string
	// Start begins the work and returns its Handle. Called again with the
	// same Job after a crash before the Handle was stored, it must not start
	// a second copy.
	Start(ctx context.Context, job Job) ([]byte, error)
	Poll(ctx context.Context, job Job) (Report, error)
	// Finalize lists the finished files. It changes nothing.
	Finalize(ctx context.Context, job Job) (Manifest, error)
	// Root is the folder the job's files are in, from the Handle alone. It
	// works after Release, which Finalize may not.
	Root(job Job) (string, error)
	// Release lets go of finished files without deleting them, so they can be
	// moved (for qBittorrent: remove the torrent, keep the data).
	Release(ctx context.Context, job Job) error
	// Cancel stops the work; with deleteData it deletes what it downloaded.
	Cancel(ctx context.Context, job Job, deleteData bool) error
}

// Class says what ingest should do after an error.
type Class int

const (
	Retry             Class = iota // try again on the next tick
	Permanent                      // give up: the item fails
	TryAnotherFetcher              // this fetcher can't do it; the policy may switch
)

// Error is a source error with a class and a message users may see.
type Error struct {
	Class Class
	Msg   string
	Err   error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// Fail makes an error of a class.
func Fail(c Class, msg string) error { return &Error{Class: c, Msg: msg} }

// ClassOf returns an error's class; plain errors are Retry.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return Retry
}

// Message is the part of an error a user may see.
func Message(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return "download error"
}
