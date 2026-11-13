package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torza/internal/auth"
	"torza/internal/db"
	"torza/internal/links"
	"torza/internal/magnet"
)

const (
	userKey  = "torza_test-user-key"
	adminKey = "torza_test-admin-key"
	hash     = "c9e15763f722f23e98a29decdfae341b98d53056"
)

var signingKey = []byte("0123456789abcdef0123456789abcdef")

type fakeRepo struct {
	users map[string]db.User // keyed by string(key hash)

	addStatus string
	addQueued bool
	addErr    error
	added     []string

	library map[string]db.Torrent // the test user's library, by info hash
	files   map[int64]db.File
	blocked map[string]string
}

func (f *fakeRepo) UserByKeyHash(_ context.Context, h []byte) (db.User, error) {
	u, ok := f.users[string(h)]
	if !ok {
		return db.User{}, db.ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) AddTorrentForUser(_ context.Context, _ int64, m magnet.Magnet, _ int) (db.Torrent, bool, error) {
	if f.addErr != nil {
		return db.Torrent{}, false, f.addErr
	}
	f.added = append(f.added, m.InfoHash)
	return db.Torrent{InfoHash: m.InfoHash, Name: m.Name, Status: f.addStatus}, f.addQueued, nil
}

func (f *fakeRepo) ListUserTorrents(context.Context, int64) ([]db.Torrent, error) {
	var out []db.Torrent
	for _, t := range f.library {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeRepo) UserTorrent(_ context.Context, _ int64, ih string) (db.Torrent, []db.File, error) {
	t, ok := f.library[ih]
	if !ok {
		return db.Torrent{}, nil, db.ErrNotFound
	}
	var files []db.File
	for _, file := range f.files {
		if file.InfoHash == ih {
			files = append(files, file)
		}
	}
	return t, files, nil
}

func (f *fakeRepo) RemoveUserTorrent(_ context.Context, _ int64, ih string) error {
	if _, ok := f.library[ih]; !ok {
		return db.ErrNotFound
	}
	delete(f.library, ih)
	return nil
}

func (f *fakeRepo) FileForUser(_ context.Context, _ int64, id int64) (db.File, db.Torrent, error) {
	file, ok := f.files[id]
	if !ok {
		return db.File{}, db.Torrent{}, db.ErrNotFound
	}
	t, ok := f.library[file.InfoHash]
	if !ok {
		return db.File{}, db.Torrent{}, db.ErrNotFound
	}
	return file, t, nil
}

func (f *fakeRepo) Block(_ context.Context, ih, reason, _ string) error {
	f.blocked[ih] = reason
	return nil
}

type fakeNotifier struct{ queued, blocked []string }

func (n *fakeNotifier) TorrentQueued(ih string) error  { n.queued = append(n.queued, ih); return nil }
func (n *fakeNotifier) TorrentBlocked(ih string) error { n.blocked = append(n.blocked, ih); return nil }

func newTestServer() (http.Handler, *fakeRepo, *fakeNotifier) {
	repo := &fakeRepo{
		users: map[string]db.User{
			string(auth.HashKey(userKey)):  {ID: 1, Email: "user@example.com"},
			string(auth.HashKey(adminKey)): {ID: 2, Email: "admin@example.com", IsAdmin: true},
		},
		addStatus: db.StatusQueued,
		addQueued: true,
		library:   map[string]db.Torrent{},
		files:     map[int64]db.File{},
		blocked:   map[string]string{},
	}
	n := &fakeNotifier{}
	cfg := Config{PublicURL: "https://torza.test", LinkTTL: time.Hour, MaxActivePerUser: 3}
	s := New(repo, n, links.NewSigner(signingKey), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s.Handler(), repo, n
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

func TestAuth(t *testing.T) {
	h, _, _ := newTestServer()
	if rec := call(h, "GET", "/v1/me", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/me", "torza_wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: %d", rec.Code)
	}
	rec := call(h, "GET", "/v1/me", userKey, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "user@example.com") {
		t.Errorf("valid key: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/healthz", "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("healthz: %d", rec.Code)
	}
}

func TestAddTorrent(t *testing.T) {
	h, repo, n := newTestServer()
	body := `{"magnet": "magnet:?xt=urn:btih:` + hash + `&dn=Test"}`

	rec := call(h, "POST", "/v1/torrents", userKey, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if len(repo.added) != 1 || repo.added[0] != hash {
		t.Fatalf("repo got %v", repo.added)
	}
	if len(n.queued) != 1 || n.queued[0] != hash {
		t.Fatalf("ingest wasn't woken: %v", n.queued)
	}

	// Already cached: no new download, so ingest isn't woken.
	repo.addQueued, repo.addStatus = false, db.StatusReady
	if rec := call(h, "POST", "/v1/torrents", userKey, body); rec.Code != http.StatusOK {
		t.Fatalf("add cached: %d", rec.Code)
	}
	if len(n.queued) != 1 {
		t.Fatalf("woke ingest for a cached torrent")
	}

	cases := []struct {
		name string
		err  error
		body string
		want int
	}{
		{"blocked", db.ErrBlocked, body, http.StatusUnavailableForLegalReasons},
		{"too many", db.ErrTooManyActive, body, http.StatusTooManyRequests},
		{"bad magnet", nil, `{"magnet": "nope"}`, http.StatusBadRequest},
		{"not json", nil, `magnet please`, http.StatusBadRequest},
	}
	for _, c := range cases {
		repo.addErr = c.err
		if rec := call(h, "POST", "/v1/torrents", userKey, c.body); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
	}
}

func TestGetAndRemoveTorrent(t *testing.T) {
	h, repo, _ := newTestServer()
	repo.library[hash] = db.Torrent{InfoHash: hash, Status: db.StatusReady}
	repo.files[5] = db.File{ID: 5, InfoHash: hash, Path: "Movie/movie.mkv", SizeBytes: 10}

	rec := call(h, "GET", "/v1/torrents/"+strings.ToUpper(hash), userKey, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Movie/movie.mkv") {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/v1/torrents/xyz", userKey, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad hash: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/torrents/"+strings.Repeat("0", 40), userKey, ""); rec.Code != http.StatusNotFound {
		t.Errorf("not in library: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/torrents", userKey, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), hash) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "DELETE", "/v1/torrents/"+hash, userKey, ""); rec.Code != http.StatusNoContent {
		t.Errorf("remove: %d", rec.Code)
	}
	if rec := call(h, "DELETE", "/v1/torrents/"+hash, userKey, ""); rec.Code != http.StatusNotFound {
		t.Errorf("remove twice: %d", rec.Code)
	}
}

func TestCreateLink(t *testing.T) {
	h, repo, _ := newTestServer()
	repo.library[hash] = db.Torrent{InfoHash: hash, Status: db.StatusReady}
	repo.files[5] = db.File{ID: 5, InfoHash: hash, Path: "Movie/my movie.mkv"}

	rec := call(h, "POST", "/v1/files/5/link", userKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(resp.URL, "https://torza.test/d/")
	if !ok || !strings.HasSuffix(rest, "/my%20movie.mkv") {
		t.Fatalf("url = %q", resp.URL)
	}
	token := strings.SplitN(rest, "/", 2)[0]
	claims, err := links.NewSigner(signingKey).Verify(token)
	if err != nil || claims.FileID != 5 || claims.UserID != 1 || !claims.Expires.Equal(resp.ExpiresAt) {
		t.Fatalf("token claims %+v, err %v", claims, err)
	}

	repo.library[hash] = db.Torrent{InfoHash: hash, Status: db.StatusDownloading}
	if rec := call(h, "POST", "/v1/files/5/link", userKey, ""); rec.Code != http.StatusConflict {
		t.Errorf("not ready: %d", rec.Code)
	}
	if rec := call(h, "POST", "/v1/files/99/link", userKey, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown file: %d", rec.Code)
	}
	if rec := call(h, "POST", "/v1/files/abc/link", userKey, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d", rec.Code)
	}
}

func TestBlocklist(t *testing.T) {
	h, repo, n := newTestServer()
	body := `{"torrent": "` + hash + `", "reason": "copyright notice", "reference": "N-123"}`

	if rec := call(h, "POST", "/v1/admin/blocklist", userKey, body); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", rec.Code)
	}
	if rec := call(h, "POST", "/v1/admin/blocklist", adminKey, `{"torrent": "`+hash+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: %d", rec.Code)
	}
	if rec := call(h, "POST", "/v1/admin/blocklist", adminKey, body); rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	if repo.blocked[hash] != "copyright notice" {
		t.Fatalf("blocked = %v", repo.blocked)
	}
	if len(n.blocked) != 1 || n.blocked[0] != hash {
		t.Fatalf("ingest wasn't woken: %v", n.blocked)
	}
}
