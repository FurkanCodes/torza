// Package stream serves cached files to whoever holds a valid signed link.
//
// It supports HTTP range requests ("send me bytes 1000000-2000000"), which is
// how video players jump around a file without downloading all of it, and how
// download managers resume.
package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"

	"torza/internal/db"
	"torza/internal/links"
	"torza/internal/service"
	"torza/internal/store"
)

// UseAfter is how many bytes a response must send before the read counts as
// use for eviction. Opening a file, HEAD requests and media scanners reading
// headers don't keep content cached.
const UseAfter = 8 << 20

// Access is the part of the service layer the stream service needs.
type Access interface {
	Readable(ctx context.Context, userID, fileID int64) (db.File, db.Item, error)
	Touch(ctx context.Context, itemID int64) error
}

type Server struct {
	access Access
	signer *links.Signer
	store  store.Store
	log    *slog.Logger
}

func New(access Access, signer *links.Signer, st store.Store, log *slog.Logger) *Server {
	return &Server{access: access, signer: signer, store: st, log: log}
}

var _ Access = (*service.Service)(nil)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// GET patterns also match HEAD requests.
	mux.HandleFunc("GET /d/{token}/{name}", s.download)
	return mux
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	claims, err := s.signer.Verify(r.PathValue("token"))
	if errors.Is(err, links.ErrExpired) {
		http.Error(w, "This link has expired. Create a new one.", http.StatusGone)
		return
	}
	if err != nil {
		http.Error(w, "Invalid link.", http.StatusForbidden)
		return
	}

	// Re-check access: the user may have removed the item, or it may have
	// been evicted or blocked since the link was made.
	file, it, err := s.access.Readable(r.Context(), claims.UserID, claims.FileID)
	switch {
	case err == nil:
	case errors.Is(err, db.ErrBlocked):
		http.Error(w, "This file was removed after a takedown request.", http.StatusUnavailableForLegalReasons)
		return
	case errors.Is(err, db.ErrNotFound):
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	case errors.Is(err, service.ErrNotReady), errors.Is(err, service.ErrGone):
		http.Error(w, "This file is no longer cached. Get a new link; Torza downloads it again.", http.StatusNotFound)
		return
	default:
		s.log.Error("check access", "err", err)
		http.Error(w, "Internal error.", http.StatusInternalServerError)
		return
	}

	f, err := s.store.Open(it.ID, file.Path)
	if err != nil {
		s.log.Warn("cached file missing", "item", it.ID, "path", file.Path, "err", err)
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}

	name := path.Base(file.Path)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", fmt.Sprintf(`"%d-%d-%d"`, file.ID, info.Size(), info.ModTime().Unix()))
	// A small file read in full counts too.
	cw := &countingWriter{ResponseWriter: w, after: max(1, min(UseAfter, info.Size())), onUse: func() {
		if err := s.access.Touch(context.WithoutCancel(r.Context()), it.ID); err != nil {
			s.log.Warn("record use", "item", it.ID, "err", err)
		}
	}}
	// ServeContent handles Range, If-Range, HEAD and Content-Type for us.
	http.ServeContent(cw, r, name, info.ModTime(), f)
}

// countingWriter calls onUse once, when the response body passes after bytes.
type countingWriter struct {
	http.ResponseWriter
	n     int64
	after int64
	onUse func()
	used  bool
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	if !c.used && c.n >= c.after {
		c.used = true
		c.onUse()
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
