// Package api is Torza's public REST API and test web page. Every /v1
// request is authenticated with an API key sent as
// "Authorization: Bearer <key>".
//
// It works like a debrid service's API: check whether content is cached,
// add it (a magnet, an info hash or a .torrent file), watch it download,
// get direct links for its files, and remove it.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"torza/internal/auth"
	"torza/internal/db"
	"torza/internal/magnet"
	"torza/internal/service"
	"torza/internal/stremio"
)

// MaxCheck is the most inputs one availability check takes.
const MaxCheck = 100

type Config struct {
	PublicURL string // where the stream service is reached; the web page may play media from there
}

type Server struct {
	svc *service.Service
	cfg Config
	log *slog.Logger
}

func New(svc *service.Service, cfg Config, log *slog.Logger) *Server {
	return &Server{svc: svc, cfg: cfg, log: log}
}

// userHandler is a handler that runs after the API key has been checked.
type userHandler func(w http.ResponseWriter, r *http.Request, u db.User)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /v1/me", s.withUser(s.me))
	mux.Handle("POST /v1/items/check", s.withUser(s.check))
	mux.Handle("POST /v1/items", s.withUser(s.add))
	mux.Handle("GET /v1/items", s.withUser(s.list))
	mux.Handle("GET /v1/items/{id}", s.withUser(s.get))
	mux.Handle("DELETE /v1/items/{id}", s.withUser(s.remove))
	mux.Handle("POST /v1/items/{id}/links", s.withUser(s.itemLinks))
	mux.Handle("POST /v1/files/{id}/link", s.withUser(s.fileLink))
	mux.Handle("POST /v1/admin/blocklist", s.withUser(adminOnly(s.block)))
	mux.Handle("PUT /v1/admin/users/{id}/limits", s.withUser(adminOnly(s.setLimits)))
	mux.Handle("POST /v1/stremio", s.withUser(s.newStremio))
	mux.Handle("GET /v1/stremio", s.withUser(s.listStremio))
	mux.Handle("DELETE /v1/stremio/{id}", s.withUser(s.revokeStremio))
	if s.svc != nil {
		stremio.New(s.svc, s.log).Register(mux)
	}
	web := s.webHandler()
	mux.Handle("GET /{$}", web)
	mux.Handle("GET /ui/", web)
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
		u, err := s.svc.Authorize(r.Context(), auth.HashKey(key))
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

type itemView struct {
	ID        int64      `json:"id"`
	Hash      string     `json:"hash,omitempty"` // the torrent info hash this user added it by
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Phase     string     `json:"phase,omitempty"`
	Progress  float64    `json:"progress"`
	SizeBytes int64      `json:"size_bytes"`
	Error     string     `json:"error,omitempty"`
	AddedAt   *time.Time `json:"added_at,omitempty"`
	Files     []fileView `json:"files,omitempty"`
}

type fileView struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

func newItemView(it db.Item, files []db.File) itemView {
	v := itemView{ID: it.ID, Name: it.Name, Status: it.Status, Progress: it.Progress, SizeBytes: it.SizeBytes, Error: it.Error}
	if it.Status == db.StatusActive || it.Status == db.StatusFinalizing {
		v.Phase = it.Phase
	}
	if it.Status == db.StatusBlocked {
		v.Name, v.Error = "", ""
	}
	for _, f := range files {
		v.Files = append(v.Files, fileView{ID: f.ID, Path: f.Path, SizeBytes: f.SizeBytes})
	}
	return v
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) me(w http.ResponseWriter, r *http.Request, u db.User) {
	a, err := s.svc.Account(r.Context(), u)
	if err != nil {
		s.internalError(w, "account", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "email": u.Email, "is_admin": u.IsAdmin,
		"limits": map[string]any{"max_active": a.Limits.MaxActive, "max_item_bytes": a.Limits.MaxItemBytes, "priority": a.Limits.Priority},
		"usage":  map[string]any{"active": a.Usage.Active, "library_items": a.Usage.LibraryItems, "library_bytes": a.Usage.LibraryBytes},
	})
}

// check: POST /v1/items/check {"items": ["magnet:?...", "<hash>", ...]}
// → for each input: cached, downloading, unknown, blocked or invalid.
func (s *Server) check(w http.ResponseWriter, r *http.Request, u db.User) {
	var req struct {
		Items []string `json:"items"`
	}
	if err := decodeJSON(w, r, &req); err != nil || len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, `body must be JSON like {"items": ["magnet:?xt=urn:btih:...", "<info hash>"]}`)
		return
	}
	if len(req.Items) > MaxCheck {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("check at most %d items at once", MaxCheck))
		return
	}
	res, err := s.svc.Check(r.Context(), req.Items)
	if err != nil {
		s.internalError(w, "check", err)
		return
	}
	out := make([]map[string]any, len(res))
	for i, c := range res {
		v := map[string]any{"input": c.Input}
		switch {
		case !c.Valid:
			v["availability"] = "invalid"
		case c.Status == db.StatusBlocked:
			v["availability"] = "blocked"
		case c.Status == db.StatusReady:
			v["availability"] = "cached"
		case c.Status == db.StatusQueued || c.Status == db.StatusActive || c.Status == db.StatusFinalizing:
			v["availability"] = "downloading"
		default: // unknown, evicted or failed: adding it starts a download
			v["availability"] = "not_cached"
		}
		if c.Valid {
			v["hash"] = c.Key.Value
		}
		if c.Status == db.StatusReady {
			v["name"], v["size_bytes"] = c.Name, c.Size
			files := make([]fileView, 0, len(c.Files))
			for _, f := range c.Files {
				files = append(files, fileView{ID: f.ID, Path: f.Path, SizeBytes: f.SizeBytes})
			}
			v["files"] = files
		}
		out[i] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// add: POST /v1/items with JSON {"magnet": "<magnet link or info hash>"},
// or a multipart form with the .torrent file in a field named "torrent".
func (s *Server) add(w http.ResponseWriter, r *http.Request, u db.User) {
	m, err := s.parseAdd(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.svc.Add(r.Context(), u.ID, m)
	switch {
	case errors.Is(err, db.ErrBlocked):
		writeError(w, http.StatusUnavailableForLegalReasons, "this content was removed after a takedown request")
		return
	case errors.Is(err, db.ErrTooManyActive):
		writeError(w, http.StatusTooManyRequests, "you already have as many downloads running as your limit allows; wait for one to finish")
		return
	case err != nil:
		s.internalError(w, "add", err)
		return
	}
	v := newItemView(res.Item, res.Files)
	v.Hash = m.InfoHash
	status := http.StatusOK
	if res.Started {
		status = http.StatusAccepted
	}
	writeJSON(w, status, v)
}

const maxUpload = 10 << 20

func (s *Server) parseAdd(w http.ResponseWriter, r *http.Request) (magnet.Magnet, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload+64<<10)
		f, _, err := r.FormFile("torrent")
		if err != nil {
			return magnet.Magnet{}, errors.New(`send the .torrent file in a form field named "torrent"`)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
		if err != nil {
			return magnet.Magnet{}, err
		}
		return magnet.ParseTorrentFile(b)
	}
	var req struct {
		Magnet string `json:"magnet"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return magnet.Magnet{}, errors.New(`body must be JSON like {"magnet": "magnet:?xt=urn:btih:..."}, or a .torrent file upload`)
	}
	return magnet.Parse(req.Magnet)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, u db.User) {
	items, err := s.svc.Library(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, "list", err)
		return
	}
	out := make([]itemView, 0, len(items))
	for _, li := range items {
		v := newItemView(li.Item, nil)
		if li.Key.Kind == db.KindTorrent {
			v.Hash = li.Key.Value
		}
		added := li.AddedAt
		v.AddedAt = &added
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, u db.User) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	it, files, err := s.svc.Item(r.Context(), u.ID, id)
	if s.libraryError(w, err, "get item") {
		return
	}
	writeJSON(w, http.StatusOK, newItemView(it, files))
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, u db.User) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if s.libraryError(w, s.svc.Remove(r.Context(), u.ID, id), "remove item") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// itemLinks: POST /v1/items/{id}/links → {"links": [...]} for every file.
func (s *Server) itemLinks(w http.ResponseWriter, r *http.Request, u db.User) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	ls, it, err := s.svc.ItemLinks(r.Context(), u.ID, id)
	if s.linkError(w, err, it) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": ls})
}

// fileLink: POST /v1/files/{id}/link → {"url": ..., "expires_at": ...}
func (s *Server) fileLink(w http.ResponseWriter, r *http.Request, u db.User) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	l, it, err := s.svc.FileLink(r.Context(), u.ID, id)
	if s.linkError(w, err, it) {
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// linkError writes the response for a link that couldn't be made. An item
// that was evicted is downloading again: 202 with its status, so a client
// can retry when it is ready.
func (s *Server) linkError(w http.ResponseWriter, err error, it db.Item) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, service.ErrNotReady):
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": it.Status, "progress": it.Progress,
			"message": "not cached yet; it is downloading. Ask again when it is ready.",
		})
	case errors.Is(err, service.ErrGone):
		writeError(w, http.StatusGone, "this file isn't part of the item's current download")
	case errors.Is(err, db.ErrTooManyActive):
		writeError(w, http.StatusTooManyRequests, "this was removed from the cache, and you already have as many downloads running as your limit allows")
	default:
		return s.libraryError(w, err, "make link")
	}
	return true
}

func (s *Server) libraryError(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "not in your library")
	case errors.Is(err, db.ErrBlocked):
		writeError(w, http.StatusUnavailableForLegalReasons, "this content was removed after a takedown request")
	default:
		s.internalError(w, what, err)
	}
	return true
}

// block: POST /v1/admin/blocklist {"torrent": magnet or hash, "reason": ..., "reference": ...}
func (s *Server) block(w http.ResponseWriter, r *http.Request, u db.User) {
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
	itemID, err := s.svc.Block(r.Context(), u, m, req.Reason, req.Reference)
	if err != nil {
		s.internalError(w, "block", err)
		return
	}
	s.log.Info("blocked", "hash", m.InfoHash, "item", itemID, "admin", u.ID, "reference", req.Reference)
	writeJSON(w, http.StatusOK, map[string]any{"blocked": m.InfoHash, "item_id": itemID})
}

// setLimits: PUT /v1/admin/users/{id}/limits {"max_active": 3, "max_item_bytes": null, "priority": 0}
// A null or missing limit means "use the server default".
func (s *Server) setLimits(w http.ResponseWriter, r *http.Request, _ db.User) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var req struct {
		MaxActive    *int   `json:"max_active"`
		MaxItemBytes *int64 `json:"max_item_bytes"`
		Priority     int    `json:"priority"`
	}
	if err := decodeJSON(w, r, &req); err != nil ||
		(req.MaxActive != nil && *req.MaxActive < 0) || (req.MaxItemBytes != nil && *req.MaxItemBytes < 0) {
		writeError(w, http.StatusBadRequest, `body must be JSON like {"max_active": 3, "max_item_bytes": 50000000000, "priority": 0}`)
		return
	}
	err := s.svc.SetLimits(r.Context(), id, req.MaxActive, req.MaxItemBytes, req.Priority)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such user")
		return
	}
	if err != nil {
		s.internalError(w, "set limits", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Stremio add-on links ────────────────────────────────────────────────────

// newStremio: POST /v1/stremio {"label": "Living room TV"} → the add-on's
// install URLs. The secret in them is shown once.
func (s *Server) newStremio(w http.ResponseWriter, r *http.Request, u db.User) {
	var req struct {
		Label string `json:"label"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, `body must be empty or JSON like {"label": "Living room TV"}`)
			return
		}
	}
	if len(req.Label) > 100 {
		writeError(w, http.StatusBadRequest, "label is too long (100 characters at most)")
		return
	}
	key, c, err := s.svc.NewStremioKey(r.Context(), u.ID, strings.TrimSpace(req.Label))
	if err != nil {
		s.internalError(w, "new stremio key", err)
		return
	}
	manifest := requestBase(r) + "/stremio/" + key + "/manifest.json"
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           c.ID,
		"label":        c.Label,
		"manifest_url": manifest,
		// Opening this link on a device with Stremio installs the add-on.
		"install_url": "stremio://" + strings.TrimPrefix(strings.TrimPrefix(manifest, "https://"), "http://"),
	})
}

func (s *Server) listStremio(w http.ResponseWriter, r *http.Request, u db.User) {
	cs, err := s.svc.StremioKeys(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, "list stremio keys", err)
		return
	}
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"id": c.ID, "label": c.Label, "prefix": c.Prefix, "created_at": c.CreatedAt, "last_used_at": c.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"addons": out})
}

func (s *Server) revokeStremio(w http.ResponseWriter, r *http.Request, u db.User) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if s.libraryError(w, s.svc.RevokeStremioKey(r.Context(), u.ID, id), "revoke stremio key") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requestBase is where this request reached the API.
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive number")
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(v)
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
