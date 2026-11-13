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
	"os"
	"path"

	"torza/internal/db"
	"torza/internal/links"
	"torza/internal/store"
)

// Repo is the part of the database the stream service needs.
type Repo interface {
	FileForUser(ctx context.Context, userID, fileID int64) (db.File, db.Torrent, error)
	TouchTorrent(ctx context.Context, infohash string) error
}

type Server struct {
	repo   Repo
	signer *links.Signer
	store  store.Store
	log    *slog.Logger
}

func New(repo Repo, signer *links.Signer, st store.Store, log *slog.Logger) *Server {
	return &Server{repo: repo, signer: signer, store: st, log: log}
}

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

	// Re-check access: the user may have removed the torrent, or it may have
	// been evicted or blocked since the link was made.
	file, t, err := s.repo.FileForUser(r.Context(), claims.UserID, claims.FileID)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("find file", "err", err)
		http.Error(w, "Internal error.", http.StatusInternalServerError)
		return
	}
	switch t.Status {
	case db.StatusReady:
	case db.StatusBlocked:
		http.Error(w, "This file was removed after a takedown request.", http.StatusUnavailableForLegalReasons)
		return
	default:
		http.Error(w, "This file is no longer cached. Add the torrent again.", http.StatusNotFound)
		return
	}

	p, err := s.store.FilePath(file.InfoHash, file.Path)
	if err != nil {
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.log.Warn("cached file missing", "infohash", file.InfoHash, "path", file.Path, "err", err)
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}

	if err := s.repo.TouchTorrent(r.Context(), t.InfoHash); err != nil {
		s.log.Warn("touch torrent", "infohash", t.InfoHash, "err", err)
	}

	name := path.Base(file.Path)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", fmt.Sprintf(`"%d-%d-%d"`, file.ID, info.Size(), info.ModTime().Unix()))
	// ServeContent handles Range, If-Range, HEAD and Content-Type for us.
	http.ServeContent(w, r, name, info.ModTime(), f)
}
