package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torza/internal/auth"
	"torza/internal/db"
	"torza/internal/dbtest"
	"torza/internal/links"
	"torza/internal/service"
)

const (
	userKey  = "torza_test-user-key"
	adminKey = "torza_test-admin-key"
	hash     = "c9e15763f722f23e98a29decdfae341b98d53056"
)

var signingKey = []byte("0123456789abcdef0123456789abcdef")

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// webServer serves the web page only; it has no database.
func webServer() http.Handler {
	return New(nil, Config{PublicURL: "http://localhost:8081"}, discard()).Handler()
}

type env struct {
	h    http.Handler
	repo *db.Repo
}

// newEnv is a full API on a real database with a user and an admin.
func newEnv(t *testing.T, defaults db.Limits) *env {
	t.Helper()
	repo := dbtest.Repo(t)
	ctx := context.Background()
	if _, err := repo.CreateUser(ctx, "user@example.test", false, auth.HashKey(userKey), "u"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateUser(ctx, "admin@example.test", true, auth.HashKey(adminKey), "a"); err != nil {
		t.Fatal(err)
	}
	svc := service.New(repo, links.NewSigner(signingKey), service.Config{PublicURL: "http://dl.test", LinkTTL: time.Hour, Defaults: defaults})
	return &env{h: New(svc, Config{PublicURL: "http://dl.test"}, discard()).Handler(), repo: repo}
}

func call(h http.Handler, method, target, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return v
}

// finish plays ingest: every queued item becomes ready with the given files.
func (e *env) finish(t *testing.T, files ...string) {
	t.Helper()
	ctx := context.Background()
	jobs, err := e.repo.Claim(ctx, "qbittorrent", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		var m []db.ManifestFile
		for _, f := range files {
			m = append(m, db.ManifestFile{Path: f, SizeBytes: 100})
		}
		if err := e.repo.BeginFinalize(ctx, j.ItemID, "qbittorrent", "Demo", m); err != nil {
			t.Fatal(err)
		}
		if err := e.repo.MarkReady(ctx, j.ItemID, "qbittorrent"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, db.Limits{})
	for _, key := range []string{"", "wrong"} {
		if rec := call(e.h, "GET", "/v1/me", key, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d", key, rec.Code)
		}
	}
	rec := call(e.h, "GET", "/v1/me", userKey, "")
	me := decode(t, rec)
	if rec.Code != 200 || me["email"] != "user@example.test" || me["usage"] == nil || me["limits"] == nil {
		t.Fatalf("me: %d %v", rec.Code, me)
	}
	if rec := call(e.h, "POST", "/v1/admin/blocklist", userKey, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("admin route as user: %d", rec.Code)
	}
}

// The debrid flow: check → add → (download) → check again → links for the
// whole item → a second user adds it instantly.
func TestDebridFlow(t *testing.T) {
	e := newEnv(t, db.Limits{})
	magnetURI := "magnet:?xt=urn:btih:" + hash + "&dn=Demo"

	rec := call(e.h, "POST", "/v1/items/check", userKey, `{"items": ["`+magnetURI+`", "nonsense"]}`)
	items := decode(t, rec)["items"].([]any)
	if rec.Code != 200 || items[0].(map[string]any)["availability"] != "not_cached" || items[1].(map[string]any)["availability"] != "invalid" {
		t.Fatalf("check before: %d %v", rec.Code, items)
	}

	rec = call(e.h, "POST", "/v1/items", userKey, `{"magnet": "`+magnetURI+`"}`)
	added := decode(t, rec)
	if rec.Code != http.StatusAccepted || added["status"] != "queued" || added["hash"] != hash {
		t.Fatalf("add: %d %v", rec.Code, added)
	}
	id := int64(added["id"].(float64))

	rec = call(e.h, "POST", fmt.Sprintf("/v1/items/%d/links", id), userKey, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("links before ready: %d %s", rec.Code, rec.Body)
	}
	e.finish(t, "Demo/demo.mkv", "Demo/demo.srt")

	items = decode(t, call(e.h, "POST", "/v1/items/check", adminKey, `{"items": ["`+hash+`"]}`))["items"].([]any)
	if c := items[0].(map[string]any); c["availability"] != "cached" || len(c["files"].([]any)) != 2 {
		t.Fatalf("check after: %v", c)
	}

	rec = call(e.h, "POST", fmt.Sprintf("/v1/items/%d/links", id), userKey, "")
	ls := decode(t, rec)["links"].([]any)
	if rec.Code != 200 || len(ls) != 2 || !strings.HasPrefix(ls[0].(map[string]any)["url"].(string), "http://dl.test/d/") {
		t.Fatalf("links: %d %v", rec.Code, ls)
	}
	fileID := int64(ls[0].(map[string]any)["file_id"].(float64))
	if rec := call(e.h, "POST", fmt.Sprintf("/v1/files/%d/link", fileID), userKey, ""); rec.Code != 200 {
		t.Fatalf("file link: %d", rec.Code)
	}
	// Someone else's file isn't reachable.
	if rec := call(e.h, "POST", fmt.Sprintf("/v1/files/%d/link", fileID), adminKey, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("other user's file: %d", rec.Code)
	}

	// The admin adds the same content: instant, ready, with files.
	rec = call(e.h, "POST", "/v1/items", adminKey, `{"magnet": "`+hash+`"}`)
	if v := decode(t, rec); rec.Code != 200 || v["status"] != "ready" || len(v["files"].([]any)) != 2 {
		t.Fatalf("second add: %d %v", rec.Code, v)
	}

	list := decode(t, call(e.h, "GET", "/v1/items", userKey, ""))["items"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["hash"] != hash {
		t.Fatalf("list: %v", list)
	}
	if rec := call(e.h, "DELETE", fmt.Sprintf("/v1/items/%d", id), userKey, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec := call(e.h, "GET", fmt.Sprintf("/v1/items/%d", id), adminKey, ""); rec.Code != 200 {
		t.Fatalf("removal must be per user: %d", rec.Code)
	}
}

func TestEvictedLinkQueuesAgain(t *testing.T) {
	e := newEnv(t, db.Limits{})
	rec := call(e.h, "POST", "/v1/items", userKey, `{"magnet": "`+hash+`"}`)
	id := int64(decode(t, rec)["id"].(float64))
	e.finish(t, "a.mkv")
	if err := e.repo.MarkEvicted(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	rec = call(e.h, "POST", fmt.Sprintf("/v1/items/%d/links", id), userKey, "")
	if v := decode(t, rec); rec.Code != http.StatusAccepted || v["status"] != "queued" {
		t.Fatalf("link for evicted item: %d %v", rec.Code, v)
	}
}

func TestTorrentFileUpload(t *testing.T) {
	e := newEnv(t, db.Limits{})
	info := "d6:lengthi5e4:name8:demo.txt12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae"
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("torrent", "demo.torrent")
	_, _ = fw.Write([]byte("d4:info" + info + "e"))
	_ = w.Close()
	req := httptest.NewRequest("POST", "/v1/items", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+userKey)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if v := decode(t, rec); rec.Code != http.StatusAccepted || v["name"] != "demo.txt" || len(v["hash"].(string)) != 40 {
		t.Fatalf("upload: %d %v", rec.Code, v)
	}
}

func TestLimitsAndBlocks(t *testing.T) {
	e := newEnv(t, db.Limits{MaxActive: 1})
	if rec := call(e.h, "POST", "/v1/items", userKey, `{"magnet": "`+hash+`"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("first add: %d", rec.Code)
	}
	other := strings.Repeat("d", 40)
	if rec := call(e.h, "POST", "/v1/items", userKey, `{"magnet": "`+other+`"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	// The owner raises this user's limit.
	if rec := call(e.h, "PUT", "/v1/admin/users/1/limits", adminKey, `{"max_active": 5}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set limits: %d %s", rec.Code, rec.Body)
	}
	if rec := call(e.h, "POST", "/v1/items", userKey, `{"magnet": "`+other+`"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("after raising the limit: %d", rec.Code)
	}
	rec := call(e.h, "POST", "/v1/admin/blocklist", adminKey, `{"torrent": "`+hash+`", "reason": "notice", "reference": "N-1"}`)
	if rec.Code != 200 {
		t.Fatalf("block: %d %s", rec.Code, rec.Body)
	}
	if rec := call(e.h, "POST", "/v1/items", adminKey, `{"magnet": "`+hash+`"}`); rec.Code != http.StatusUnavailableForLegalReasons {
		t.Fatalf("add blocked: %d", rec.Code)
	}
	items := decode(t, call(e.h, "POST", "/v1/items/check", userKey, `{"items": ["`+hash+`"]}`))["items"].([]any)
	if items[0].(map[string]any)["availability"] != "blocked" {
		t.Fatalf("check blocked: %v", items)
	}
}

func TestBadInput(t *testing.T) {
	e := newEnv(t, db.Limits{})
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/items", `{"magnet": "nope"}`},
		{"POST", "/v1/items", `not json`},
		{"POST", "/v1/items/check", `{"items": []}`},
		{"POST", "/v1/items/check", `{"items": [` + strings.Repeat(`"x",`, MaxCheck) + `"x"]}`},
		{"GET", "/v1/items/abc", ""},
	} {
		if rec := call(e.h, c.method, c.path, userKey, c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s %q: %d", c.method, c.path, c.body, rec.Code)
		}
	}
	if rec := call(e.h, "GET", "/v1/items/999", userKey, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown item: %d", rec.Code)
	}
}
