// Package api is Torza's public REST API: who you are, your torrents, and
// download links. Every /v1 request is authenticated with an API key sent as
// "Authorization: Bearer <key>".
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"torza/internal/auth"
	"torza/internal/db"
	"torza/internal/links"
	"torza/internal/magnet"
)

// Repo is the part of the database the API needs.
type Repo interface {
	UserByKeyHash(ctx context.Context, hash []byte) (db.User, error)
	AddTorrentForUser(ctx context.Context, userID int64, m magnet.Magnet, maxActive int) (db.Torrent, bool, error)
	ListUserTorrents(ctx context.Context, userID int64) ([]db.Torrent, error)
	UserTorrent(ctx context.Context, userID int64, infohash string) (db.Torrent, []db.File, error)
	RemoveUserTorrent(ctx context.Context, userID int64, infohash string) error
	FileForUser(ctx context.Context, userID, fileID int64) (db.File, db.Torrent, error)
	Block(ctx context.Context, infohash, reason, reference string) error
}

// Notifier wakes other services when there's new work.
type Notifier interface {
	TorrentQueued(infohash string) error
	TorrentBlocked(infohash string) error
}

type Config struct {
	PublicURL        string // where the stream service is reached, e.g. https://torza.example
	LinkTTL          time.Duration
	MaxActivePerUser int
}

type Server struct {
	repo   Repo
	notify Notifier
	signer *links.Signer
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

func New(repo Repo, notify Notifier, signer *links.Signer, cfg Config, log *slog.Logger) *Server {
	return &Server{repo: repo, notify: notify, signer: signer, cfg: cfg, log: log, now: time.Now}
}

// userHandler is a handler that runs after the API key has been checked.
type userHandler func(w http.ResponseWriter, r *http.Request, u db.User)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /v1/me", s.withUser(s.me))
	mux.Handle("POST /v1/torrents", s.withUser(s.addTorrent))
	mux.Handle("GET /v1/torrents", s.withUser(s.listTorrents))
	mux.Handle("GET /v1/torrents/{infohash}", s.withUser(s.getTorrent))
	mux.Handle("DELETE /v1/torrents/{infohash}", s.withUser(s.removeTorrent))
	mux.Handle("POST /v1/files/{id}/link", s.withUser(s.createLink))
	mux.Handle("POST /v1/admin/blocklist", s.withUser(adminOnly(s.blockTorrent)))
	return mux
}

func (s *Server) withUser(next userHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			writeError(w, http.StatusUnauthorized, "send your API key as: Authorization: Bearer <key>")
			return
		}
		u, err := s.repo.UserByKeyHash(r.Context(), auth.HashKey(key))
		if errors.Is(err, db.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		if err != nil {
			s.internalError(w, "look up API key", err)
			return
		}
		next(w, r, u)
	})
}

func adminOnly(next userHandler) userHandler {
	return func(w http.ResponseWriter, r *http.Request, u db.User) {
		if !u.IsAdmin {
			writeError(w, http.StatusForbidden, "admin only")
			return
		}
		next(w, r, u)
	}
}

// ── Responses ───────────────────────────────────────────────────────────────

type torrentView struct {
	InfoHash  string     `json:"infohash"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Progress  float64    `json:"progress"`
	SizeBytes int64      `json:"size_bytes"`
	Error     string     `json:"error,omitempty"`
	Files     []fileView `json:"files,omitempty"`
}

type fileView struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

func newTorrentView(t db.Torrent, files []db.File) torrentView {
	v := torrentView{InfoHash: t.InfoHash, Name: t.Name, Status: t.Status, Progress: t.Progress, SizeBytes: t.SizeBytes, Error: t.Error}
	for _, f := range files {
		v.Files = append(v.Files, fileView{ID: f.ID, Path: f.Path, SizeBytes: f.SizeBytes})
	}
	return v
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) me(w http.ResponseWriter, r *http.Request, u db.User) {
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "email": u.Email, "is_admin": u.IsAdmin})
}

// addTorrent: POST /v1/torrents {"magnet": "magnet:?xt=..." or an info hash}
func (s *Server) addTorrent(w http.ResponseWriter, r *http.Request, u db.User) {
	var req struct {
		Magnet string `json:"magnet"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be JSON like {"magnet": "magnet:?xt=urn:btih:..."}`)
		return
	}
	m, err := magnet.Parse(req.Magnet)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	t, queued, err := s.repo.AddTorrentForUser(r.Context(), u.ID, m, s.cfg.MaxActivePerUser)
	switch {
	case errors.Is(err, db.ErrBlocked):
		writeError(w, http.StatusUnavailableForLegalReasons, "this torrent was removed after a takedown request")
		return
	case errors.Is(err, db.ErrTooManyActive):
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("you already have %d downloads running; wait for one to finish", s.cfg.MaxActivePerUser))
		return
	case err != nil:
		s.internalError(w, "add torrent", err)
		return
	}
	if queued {
		// If this fails, ingest still finds the torrent on its next check.
		if err := s.notify.TorrentQueued(t.InfoHash); err != nil {
			s.log.Warn("notify queued", "infohash", t.InfoHash, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, newTorrentView(t, nil))
}

func (s *Server) listTorrents(w http.ResponseWriter, r *http.Request, u db.User) {
	ts, err := s.repo.ListUserTorrents(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, "list torrents", err)
		return
	}
	out := make([]torrentView, 0, len(ts))
	for _, t := range ts {
		out = append(out, newTorrentView(t, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"torrents": out})
}

func (s *Server) getTorrent(w http.ResponseWriter, r *http.Request, u db.User) {
	ih, ok := infohashParam(w, r)
	if !ok {
		return
	}
	t, files, err := s.repo.UserTorrent(r.Context(), u.ID, ih)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "torrent is not in your library")
		return
	}
	if err != nil {
		s.internalError(w, "get torrent", err)
		return
	}
	if t.Status != db.StatusReady {
		files = nil
	}
	writeJSON(w, http.StatusOK, newTorrentView(t, files))
}

func (s *Server) removeTorrent(w http.ResponseWriter, r *http.Request, u db.User) {
	ih, ok := infohashParam(w, r)
	if !ok {
		return
	}
	err := s.repo.RemoveUserTorrent(r.Context(), u.ID, ih)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "torrent is not in your library")
		return
	}
	if err != nil {
		s.internalError(w, "remove torrent", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createLink: POST /v1/files/{id}/link → {"url": ..., "expires_at": ...}
func (s *Server) createLink(w http.ResponseWriter, r *http.Request, u db.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "file id must be a positive number")
		return
	}
	f, t, err := s.repo.FileForUser(r.Context(), u.ID, id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file is not in your library")
		return
	}
	if err != nil {
		s.internalError(w, "find file", err)
		return
	}
	if t.Status != db.StatusReady {
		writeError(w, http.StatusConflict, "torrent is not ready (status: "+t.Status+")")
		return
	}
	expires := s.now().Add(s.cfg.LinkTTL).Truncate(time.Second)
	token := s.signer.Sign(links.Claims{FileID: f.ID, UserID: u.ID, Expires: expires})
	link := s.cfg.PublicURL + "/d/" + token + "/" + url.PathEscape(path.Base(f.Path))
	writeJSON(w, http.StatusOK, map[string]any{"url": link, "expires_at": expires.UTC()})
}

// blockTorrent: POST /v1/admin/blocklist {"torrent": magnet or hash, "reason": ..., "reference": ...}
func (s *Server) blockTorrent(w http.ResponseWriter, r *http.Request, u db.User) {
	var req struct {
		Torrent   string `json:"torrent"`
		Reason    string `json:"reason"`
		Reference string `json:"reference"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be JSON like {"torrent": "<magnet or info hash>", "reason": "...", "reference": "..."}`)
		return
	}
	m, err := magnet.Parse(req.Torrent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "torrent: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if err := s.repo.Block(r.Context(), m.InfoHash, req.Reason, req.Reference); err != nil {
		s.internalError(w, "block torrent", err)
		return
	}
	if err := s.notify.TorrentBlocked(m.InfoHash); err != nil {
		s.log.Warn("notify blocked", "infohash", m.InfoHash, "err", err)
	}
	s.log.Info("torrent blocked", "infohash", m.InfoHash, "admin", u.ID, "reference", req.Reference)
	writeJSON(w, http.StatusOK, map[string]string{"blocked": m.InfoHash})
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func infohashParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	ih := strings.ToLower(r.PathValue("infohash"))
	if !magnet.ValidHash(ih) {
		writeError(w, http.StatusBadRequest, "infohash must be 40 hex characters")
		return "", false
	}
	return ih, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
