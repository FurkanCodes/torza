// Package stremio is Torza's Stremio add-on. It lists a user's library and
// plays it from the cache. It never searches for content: everything it
// shows is something the user added.
//
// A user installs it with a URL holding their own add-on secret:
//
//	<api base>/stremio/<secret>/manifest.json
//
// Items with one video file are movies (id torza:<item>); items with several
// are series whose episodes are the files (id torza:<item>:<file>). Streams
// point at /stremio/<secret>/play/<file>, which redirects to a fresh signed
// link, or starts downloading an evicted item again and asks the player to
// retry in a minute. Play links never expire, because Stremio saves URLs.
package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"torza/internal/db"
	"torza/internal/service"
)

// Version is the add-on's version in its manifest.
const Version = "1.0.0"

// CatalogID names the one catalog, offered for movies and series.
const CatalogID = "torza-library"

const idPrefix = "torza:"

var video = regexp.MustCompile(`(?i)\.(mp4|m4v|mkv|webm|mov|avi|ts|m2ts|wmv|flv|mpg|mpeg)$`)

// Service is the part of the service layer the add-on uses.
type Service interface {
	StremioUser(ctx context.Context, key string) (db.User, error)
	Library(ctx context.Context, userID int64) ([]db.LibraryItem, error)
	FilesOf(ctx context.Context, userID int64, itemIDs []int64) ([]db.File, error)
	FileLink(ctx context.Context, userID, fileID int64) (service.Link, db.Item, error)
	Readable(ctx context.Context, userID, fileID int64) (db.File, db.Item, error)
}

var _ Service = (*service.Service)(nil)

type Handler struct {
	svc  Service
	base string // where the API is reached; "" = the address each request came to
	log  *slog.Logger
}

// New makes the add-on. base is the API's public address (for example the
// Tailscale HTTPS name); empty means play URLs use the address each request
// came to.
func New(svc Service, base string, log *slog.Logger) *Handler {
	return &Handler{svc: svc, base: strings.TrimRight(base, "/"), log: log}
}

// Register mounts the add-on's routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /stremio/{key}/manifest.json", h.withUser(h.manifest))
	mux.HandleFunc("GET /stremio/{key}/catalog/{type}/{rest...}", h.withUser(h.catalog))
	mux.HandleFunc("GET /stremio/{key}/meta/{type}/{id}", h.withUser(h.meta))
	mux.HandleFunc("GET /stremio/{key}/stream/{type}/{id}", h.withUser(h.stream))
	mux.HandleFunc("GET /stremio/{key}/play/{file}/{name}", h.withUser(h.play))
	mux.HandleFunc("OPTIONS /stremio/", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		w.WriteHeader(http.StatusNoContent)
		h.access(r, http.StatusNoContent)
	})
}

type userHandler func(w http.ResponseWriter, r *http.Request, u db.User)

func (h *Handler) withUser(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		w = sw
		defer func() { h.access(r, sw.status) }()
		cors(w)
		u, err := h.svc.StremioUser(r.Context(), r.PathValue("key"))
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this add-on link was revoked or is wrong; make a new one in Torza"})
			return
		}
		if err != nil {
			h.internal(w, "stremio user", err)
			return
		}
		next(w, r, u)
	}
}

// Stremio's web app fetches add-ons from another origin.
func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	// Chrome-based apps (Stremio 5 is one) ask before a web page may call
	// 127.0.0.1 or a home-network address.
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
}

// ── Manifest ────────────────────────────────────────────────────────────────

func (h *Handler) manifest(w http.ResponseWriter, _ *http.Request, _ db.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          "org.torza.library",
		"version":     Version,
		"name":        "Torza",
		"description": "Your Torza library: what you added to Torza, played from its cache. Items removed from the cache download again when you press play.",
		"resources":   []string{"catalog", "meta", "stream"},
		"types":       []string{"movie", "series"},
		"idPrefixes":  []string{idPrefix},
		"catalogs": []map[string]any{
			{"type": "movie", "id": CatalogID, "name": "Torza library"},
			{"type": "series", "id": CatalogID, "name": "Torza library"},
		},
		"behaviorHints": map[string]any{"configurable": false},
	})
}

// ── Library view ────────────────────────────────────────────────────────────

// entry is a library item as the add-on shows it.
type entry struct {
	item   db.LibraryItem
	videos []db.File // video files, sorted by path
}

func (e entry) kind() string {
	if len(e.videos) > 1 {
		return "series"
	}
	return "movie"
}

// shown is whether an item belongs in the add-on: not blocked, and not a
// download that failed before it ever had files.
func shown(it db.LibraryItem, files int) bool {
	return it.Status != db.StatusBlocked && !(it.Status == db.StatusFailed && files == 0)
}

func (h *Handler) entries(ctx context.Context, u db.User) ([]entry, error) {
	lib, err := h.svc.Library(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(lib))
	for i, it := range lib {
		ids[i] = it.ID
	}
	files, err := h.svc.FilesOf(ctx, u.ID, ids)
	if err != nil {
		return nil, err
	}
	byItem := map[int64][]db.File{}
	for _, f := range files {
		if video.MatchString(f.Path) {
			byItem[f.ItemID] = append(byItem[f.ItemID], f)
		}
	}
	var out []entry
	for _, it := range lib {
		if shown(it, len(byItem[it.ID])) {
			out = append(out, entry{item: it, videos: byItem[it.ID]})
		}
	}
	return out, nil
}

func (h *Handler) find(ctx context.Context, u db.User, itemID int64) (entry, bool, error) {
	es, err := h.entries(ctx, u)
	if err != nil {
		return entry{}, false, err
	}
	for _, e := range es {
		if e.item.ID == itemID {
			return e, true, nil
		}
	}
	return entry{}, false, nil
}

func name(it db.LibraryItem) string {
	if it.Name != "" {
		return it.Name
	}
	if it.Key.Value != "" {
		return "Torrent " + it.Key.Value[:min(12, len(it.Key.Value))]
	}
	return fmt.Sprintf("Item %d", it.ID)
}

// state is a short, user-facing line about whether it plays right now.
func state(it db.Item) string {
	switch it.Status {
	case db.StatusReady:
		return "Cached: plays now"
	case db.StatusQueued:
		return "Queued for download"
	case db.StatusActive:
		if it.Phase == "metadata" || it.SizeBytes == 0 {
			return "Finding peers"
		}
		return fmt.Sprintf("Downloading %d%%", int(it.Progress*100))
	case db.StatusFinalizing:
		return "Almost ready"
	case db.StatusEvicted:
		return "Not cached: press play to download it again"
	case db.StatusFailed:
		return "Download failed: press play to try again"
	}
	return it.Status
}

func (e entry) preview() map[string]any {
	m := map[string]any{
		"id":          idPrefix + strconv.FormatInt(e.item.ID, 10),
		"type":        e.kind(),
		"name":        name(e.item),
		"posterShape": "poster",
		"description": state(e.item.Item) + sizeLine(e.item.SizeBytes),
	}
	return m
}

func sizeLine(n int64) string {
	if n <= 0 {
		return ""
	}
	return " · " + human(n)
}

func human(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// ── Catalog, meta, stream ───────────────────────────────────────────────────

// catalog: /catalog/{type}/torza-library.json, or with extras such as
// /catalog/{type}/torza-library/skip=100.json. The whole library comes in
// the first page; later pages are empty.
func (h *Handler) catalog(w http.ResponseWriter, r *http.Request, u db.User) {
	typ := r.PathValue("type")
	rest := strings.TrimSuffix(r.PathValue("rest"), ".json")
	id, extra, _ := strings.Cut(rest, "/")
	if id != CatalogID || (typ != "movie" && typ != "series") {
		writeJSON(w, http.StatusNotFound, map[string]any{"metas": []any{}})
		return
	}
	metas := []map[string]any{}
	if q, _ := url.ParseQuery(extra); q.Get("skip") == "" || q.Get("skip") == "0" {
		es, err := h.entries(r.Context(), u)
		if err != nil {
			h.internal(w, "catalog", err)
			return
		}
		search := strings.ToLower(q.Get("search"))
		for _, e := range es {
			if e.kind() == typ && (search == "" || strings.Contains(strings.ToLower(name(e.item)), search)) {
				metas = append(metas, e.preview())
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"metas": metas})
}

func (h *Handler) meta(w http.ResponseWriter, r *http.Request, u db.User) {
	itemID, _, ok := parseID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"meta": nil})
		return
	}
	e, found, err := h.find(r.Context(), u, itemID)
	if err != nil {
		h.internal(w, "meta", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"meta": nil})
		return
	}
	m := e.preview()
	if e.kind() == "series" {
		videos := make([]map[string]any, 0, len(e.videos))
		for i, f := range e.videos {
			videos = append(videos, map[string]any{
				"id":       fmt.Sprintf("%s%d:%d", idPrefix, e.item.ID, f.ID),
				"title":    path.Base(f.Path),
				"season":   1,
				"episode":  i + 1,
				"released": e.item.AddedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}
		m["videos"] = videos
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": m})
}

func (h *Handler) stream(w http.ResponseWriter, r *http.Request, u db.User) {
	itemID, fileID, ok := parseID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"streams": []any{}})
		return
	}
	e, found, err := h.find(r.Context(), u, itemID)
	if err != nil {
		h.internal(w, "stream", err)
		return
	}
	streams := []map[string]any{}
	if found {
		base := h.baseURL(r) + "/stremio/" + url.PathEscape(r.PathValue("key")) + "/play/"
		for _, f := range e.videos {
			if fileID != 0 && f.ID != fileID {
				continue
			}
			file := path.Base(f.Path)
			streams = append(streams, map[string]any{
				"url":   base + strconv.FormatInt(f.ID, 10) + "/" + url.PathEscape(file),
				"name":  "Torza\n" + shortState(e.item.Item),
				"title": file + "\n" + human(f.SizeBytes) + " · " + state(e.item.Item),
				"behaviorHints": map[string]any{
					"notWebReady": !strings.HasSuffix(strings.ToLower(file), ".mp4"),
					"bingeGroup":  "torza-" + strconv.FormatInt(e.item.ID, 10),
					"filename":    file,
					"videoSize":   f.SizeBytes,
				},
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": streams})
}

func shortState(it db.Item) string {
	switch it.Status {
	case db.StatusReady:
		return "Cached"
	case db.StatusEvicted, db.StatusFailed:
		return "Not cached"
	}
	return "Downloading"
}

// play redirects to a fresh signed link. An evicted or failed item is
// downloaded again (for GET only, so a player probing with HEAD doesn't
// start downloads) and the player is told to retry.
func (h *Handler) play(w http.ResponseWriter, r *http.Request, u db.User) {
	fileID, err := strconv.ParseInt(r.PathValue("file"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "Bad file id.", http.StatusBadRequest)
		return
	}
	var target string
	var it db.Item
	if r.Method == http.MethodHead {
		_, it, err = h.svc.Readable(r.Context(), u.ID, fileID)
		if err == nil {
			l, _, lerr := h.svc.FileLink(r.Context(), u.ID, fileID)
			target, err = l.URL, lerr
		}
	} else {
		var l service.Link
		l, it, err = h.svc.FileLink(r.Context(), u.ID, fileID)
		target = l.URL
	}
	switch {
	case err == nil:
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	case errors.Is(err, service.ErrNotReady), errors.Is(err, db.ErrTooManyActive):
		w.Header().Set("Retry-After", "60")
		msg := "Not cached yet: " + state(it) + ". Try again in a minute."
		if errors.Is(err, db.ErrTooManyActive) {
			msg = "This isn't cached, and you already have as many downloads running as your limit allows."
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
	case errors.Is(err, db.ErrBlocked):
		http.Error(w, "This was removed after a takedown request.", http.StatusUnavailableForLegalReasons)
	case errors.Is(err, service.ErrGone):
		http.Error(w, "This file isn't part of the item's current download.", http.StatusGone)
	case errors.Is(err, db.ErrNotFound):
		http.Error(w, "Not in your library.", http.StatusNotFound)
	default:
		h.log.Error("stremio play", "err", err)
		http.Error(w, "Internal error.", http.StatusInternalServerError)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// access logs one add-on request without its secret, so problems with a
// Stremio app can be seen in the api log.
func (h *Handler) access(r *http.Request, status int) {
	p := r.URL.Path
	if k := r.PathValue("key"); k != "" {
		p = strings.Replace(p, k, "<secret>", 1)
	} else if parts := strings.SplitN(p, "/", 4); len(parts) == 4 {
		p = "/stremio/<secret>/" + parts[3]
	}
	h.log.Info("stremio request", "method", r.Method, "path", p, "status", status,
		"origin", r.Header.Get("Origin"), "agent", r.UserAgent())
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// parseID reads "torza:<item>" or "torza:<item>:<file>", with or without the
// ".json" Stremio adds.
func parseID(s string) (itemID, fileID int64, ok bool) {
	s = strings.TrimSuffix(s, ".json")
	rest, found := strings.CutPrefix(s, idPrefix)
	if !found {
		return 0, 0, false
	}
	a, b, two := strings.Cut(rest, ":")
	itemID, err := strconv.ParseInt(a, 10, 64)
	if err != nil || itemID <= 0 {
		return 0, 0, false
	}
	if two {
		fileID, err = strconv.ParseInt(b, 10, 64)
		if err != nil || fileID <= 0 {
			return 0, 0, false
		}
	}
	return itemID, fileID, true
}

// baseURL is where this request reached the API, so play URLs work from
// wherever Stremio installed the add-on (localhost, the home network, or
// HTTPS behind Caddy).
func (h *Handler) baseURL(r *http.Request) string {
	if h.base != "" {
		return h.base
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	h.log.Error(what, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
