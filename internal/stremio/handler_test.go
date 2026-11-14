package stremio_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torza/internal/api"
	"torza/internal/auth"
	"torza/internal/db"
	"torza/internal/dbtest"
	"torza/internal/links"
	"torza/internal/service"
)

const apiKey = "torza_test-key"

type env struct {
	h    http.Handler
	repo *db.Repo
	svc  *service.Service
	user db.User
	key  string // the add-on secret
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	repo := dbtest.Repo(t)
	u, err := repo.CreateUser(ctx, "u@example.test", false, auth.HashKey(apiKey), "p")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(repo, links.NewSigner([]byte("0123456789abcdef0123456789abcdef")),
		service.Config{PublicURL: "http://dl.test", LinkTTL: time.Hour})
	h := api.New(svc, api.Config{PublicURL: "http://dl.test"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	e := &env{h: h, repo: repo, svc: svc, user: u}

	rec := e.do(t, "POST", "/v1/stremio", apiKey, `{"label":"TV"}`)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	mu, _ := created["manifest_url"].(string)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(mu, "http://api.test/stremio/tzs_") ||
		!strings.HasPrefix(created["install_url"].(string), "stremio://api.test/stremio/tzs_") {
		t.Fatalf("create add-on: %d %v", rec.Code, created)
	}
	e.key = strings.TrimSuffix(strings.TrimPrefix(mu, "http://api.test/stremio/"), "/manifest.json")
	return e
}

func (e *env) do(t *testing.T, method, target, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://api.test"+target, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) get(t *testing.T, p string) (int, map[string]any) {
	t.Helper()
	rec := e.do(t, "GET", "/stremio/"+e.key+p, "", "")
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("%s: no CORS header", p)
	}
	return rec.Code, v
}

// item adds a torrent and finishes it with the given files.
func (e *env) item(t *testing.T, n int, name string, files ...string) int64 {
	t.Helper()
	ctx := context.Background()
	k := db.Key{Kind: db.KindTorrent, Value: fmt.Sprintf("%040x", n)}
	res, err := e.repo.Add(ctx, e.user.ID, k, "m", name, "qbittorrent", db.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		return res.Item.ID
	}
	if _, err := e.repo.Claim(ctx, "qbittorrent", 10, 0); err != nil {
		t.Fatal(err)
	}
	var m []db.ManifestFile
	for _, f := range files {
		m = append(m, db.ManifestFile{Path: f, SizeBytes: 1_500_000_000})
	}
	if err := e.repo.BeginFinalize(ctx, res.Item.ID, "qbittorrent", name, m); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.MarkReady(ctx, res.Item.ID, "qbittorrent"); err != nil {
		t.Fatal(err)
	}
	return res.Item.ID
}

func metas(t *testing.T, v map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range v["metas"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func TestManifestAndAuth(t *testing.T) {
	e := newEnv(t)
	code, m := e.get(t, "/manifest.json")
	if code != 200 || m["id"] != "org.torza.library" || len(m["catalogs"].([]any)) != 2 || m["idPrefixes"].([]any)[0] != "torza:" {
		t.Fatalf("manifest: %d %v", code, m)
	}
	if rec := e.do(t, "GET", "/stremio/tzs_wrong/manifest.json", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", rec.Code)
	}
	// An add-on secret is not an API key, and the other way round.
	if rec := e.do(t, "GET", "/v1/me", e.key, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("add-on secret used as API key: %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/stremio/"+apiKey+"/manifest.json", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("API key used as add-on secret: %d", rec.Code)
	}
	if rec := e.do(t, "OPTIONS", "/stremio/"+e.key+"/manifest.json", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d", rec.Code)
	}
}

func TestCatalogMetaAndStreams(t *testing.T) {
	e := newEnv(t)
	movie := e.item(t, 1, "Big Buck Bunny", "Big Buck Bunny/bbb.mp4", "Big Buck Bunny/bbb.srt", "Big Buck Bunny/poster.jpg")
	show := e.item(t, 2, "Open Shorts", "Shorts/ep1.mkv", "Shorts/ep2.mkv")
	queued := e.item(t, 3, "Sintel")

	_, movies := e.get(t, "/catalog/movie/torza-library.json")
	ms := metas(t, movies)
	if len(ms) != 2 { // the movie and the queued item (no files yet)
		t.Fatalf("movie catalog: %v", ms)
	}
	_, series := e.get(t, "/catalog/series/torza-library.json")
	ss := metas(t, series)
	if len(ss) != 1 || ss[0]["id"] != fmt.Sprintf("torza:%d", show) || ss[0]["name"] != "Open Shorts" {
		t.Fatalf("series catalog: %v", ss)
	}
	if _, v := e.get(t, "/catalog/movie/torza-library/skip=100.json"); len(metas(t, v)) != 0 {
		t.Fatal("second page isn't empty")
	}
	if _, v := e.get(t, "/catalog/movie/torza-library/search=bunny.json"); len(metas(t, v)) != 1 {
		t.Fatal("search didn't narrow the catalog")
	}

	_, mv := e.get(t, fmt.Sprintf("/meta/series/torza:%d.json", show))
	videos := mv["meta"].(map[string]any)["videos"].([]any)
	if len(videos) != 2 || videos[1].(map[string]any)["title"] != "ep2.mkv" {
		t.Fatalf("series videos: %v", videos)
	}
	ep2 := videos[1].(map[string]any)["id"].(string)

	_, st := e.get(t, "/stream/series/"+ep2+".json")
	streams := st["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("episode streams: %v", streams)
	}
	s0 := streams[0].(map[string]any)
	if !strings.HasPrefix(s0["url"].(string), "http://api.test/stremio/"+e.key+"/play/") || s0["behaviorHints"].(map[string]any)["notWebReady"] != true {
		t.Fatalf("stream: %v", s0)
	}

	_, st = e.get(t, fmt.Sprintf("/stream/movie/torza:%d.json", movie))
	streams = st["streams"].([]any)
	if len(streams) != 1 || !strings.HasSuffix(streams[0].(map[string]any)["url"].(string), "/bbb.mp4") {
		t.Fatalf("movie streams (videos only): %v", streams)
	}
	if _, st := e.get(t, fmt.Sprintf("/stream/movie/torza:%d.json", queued)); len(st["streams"].([]any)) != 0 {
		t.Fatal("a queued item with no files has streams")
	}
	if _, st := e.get(t, "/stream/movie/tt0111161.json"); len(st["streams"].([]any)) != 0 {
		t.Fatal("answered for an id that isn't Torza's")
	}
}

func TestPlay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.item(t, 1, "Big Buck Bunny", "bbb.mp4")
	_, st := e.get(t, fmt.Sprintf("/stream/movie/torza:%d.json", id))
	play := strings.TrimPrefix(st["streams"].([]any)[0].(map[string]any)["url"].(string), "http://api.test")

	rec := e.do(t, "GET", play, "", "")
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "http://dl.test/d/") {
		t.Fatalf("play ready: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Evicted: HEAD doesn't start a download; GET does, and asks to retry.
	if err := e.repo.MarkEvicted(ctx, id); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "HEAD", play, "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HEAD on evicted: %d", rec.Code)
	}
	if it, _, _ := e.repo.LibraryItem(ctx, e.user.ID, id); it.Status != db.StatusEvicted {
		t.Fatalf("HEAD started a download: %q", it.Status)
	}
	rec = e.do(t, "GET", play, "", "")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("GET on evicted: %d", rec.Code)
	}
	if it, _, _ := e.repo.LibraryItem(ctx, e.user.ID, id); it.Status != db.StatusQueued {
		t.Fatalf("play didn't queue the evicted item: %q", it.Status)
	}
	_, mv := e.get(t, fmt.Sprintf("/meta/movie/torza:%d.json", id))
	if d := mv["meta"].(map[string]any)["description"].(string); !strings.HasPrefix(d, "Queued") {
		t.Fatalf("description while queued: %q", d)
	}

	// Blocked: gone from the catalog, play refused.
	if _, err := e.repo.Block(ctx, db.Key{Kind: db.KindTorrent, Value: fmt.Sprintf("%040x", 1)}, "x", "", e.user.ID); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "GET", play, "", ""); rec.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("play blocked: %d", rec.Code)
	}
	if _, v := e.get(t, "/catalog/movie/torza-library.json"); len(metas(t, v)) != 0 {
		t.Fatal("blocked item still in the catalog")
	}
}

func TestRevoke(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "GET", "/v1/stremio", apiKey, "")
	var list map[string][]map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list["addons"]) != 1 || list["addons"][0]["label"] != "TV" {
		t.Fatalf("list: %s", rec.Body)
	}
	id := int64(list["addons"][0]["id"].(float64))
	if rec := e.do(t, "DELETE", fmt.Sprintf("/v1/stremio/%d", id), apiKey, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if code, _ := e.get(t, "/manifest.json"); code != http.StatusUnauthorized {
		t.Fatalf("revoked add-on still works: %d", code)
	}
}

// With a public API address set (Tailscale, a domain), add-on and play links
// use it, whatever address the request came to.
func TestConfiguredBaseURL(t *testing.T) {
	e := newEnv(t)
	svcHandler := api.New(e.svc, api.Config{PublicURL: "http://dl.test", BaseURL: "https://torza.example.ts.net"},
		slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	e.h = svcHandler
	rec := e.do(t, "POST", "/v1/stremio", apiKey, "")
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if !strings.HasPrefix(created["manifest_url"].(string), "https://torza.example.ts.net/stremio/tzs_") {
		t.Fatalf("manifest url: %v", created)
	}
	id := e.item(t, 1, "Big Buck Bunny", "bbb.mp4")
	_, st := e.get(t, fmt.Sprintf("/stream/movie/torza:%d.json", id))
	if u := st["streams"].([]any)[0].(map[string]any)["url"].(string); !strings.HasPrefix(u, "https://torza.example.ts.net/stremio/") {
		t.Fatalf("play url: %s", u)
	}
}
