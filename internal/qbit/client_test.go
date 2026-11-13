package qbit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeQbit imitates the parts of qBittorrent's Web API the client uses.
type fakeQbit struct {
	mu       sync.Mutex
	logins   int
	added    []string
	savePath string
	deleted  string
	delFiles string
	addReply string
}

func (f *fakeQbit) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.logins++
		f.mu.Unlock()
		if r.FormValue("username") != "admin" || r.FormValue("password") != "secret" {
			w.Write([]byte("Fails."))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session", Path: "/"})
		w.Write([]byte("Ok."))
	})
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie("SID"); err != nil || c.Value != "session" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /api/v2/torrents/add", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.added = append(f.added, r.FormValue("urls"))
		f.savePath = r.FormValue("savepath")
		reply := f.addReply
		f.mu.Unlock()
		if reply == "" {
			reply = "Ok."
		}
		w.Write([]byte(reply))
	}))
	mux.HandleFunc("GET /api/v2/torrents/info", authed(func(w http.ResponseWriter, r *http.Request) {
		var out []Torrent
		for _, h := range strings.Split(r.URL.Query().Get("hashes"), "|") {
			if h == "aaaa" {
				out = append(out, Torrent{Hash: h, Name: "Movie", State: "downloading", Progress: 0.25, TotalSize: 400})
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("GET /api/v2/torrents/files", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hash") != "aaaa" {
			json.NewEncoder(w).Encode([]File{})
			return
		}
		json.NewEncoder(w).Encode([]File{{Index: 0, Name: "Movie/movie.mkv", Size: 400, Progress: 0.25}})
	}))
	mux.HandleFunc("POST /api/v2/torrents/delete", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = r.FormValue("hashes")
		f.delFiles = r.FormValue("deleteFiles")
		f.mu.Unlock()
	}))
	return mux
}

func TestClient(t *testing.T) {
	fake := &fakeQbit{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	ctx := context.Background()

	c, err := New(srv.URL+"/", "admin", "secret")
	if err != nil {
		t.Fatal(err)
	}

	// The first call gets a 403, logs in and retries.
	if err := c.Add(ctx, "magnet:?xt=urn:btih:aaaa", "/data/incoming/aaaa"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if fake.logins != 1 || len(fake.added) != 1 || fake.savePath != "/data/incoming/aaaa" {
		t.Fatalf("after Add: logins=%d added=%v savepath=%q", fake.logins, fake.added, fake.savePath)
	}

	infos, err := c.Info(ctx, []string{"aaaa", "bbbb"})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "Movie" || infos[0].Progress != 0.25 || infos[0].TotalSize != 400 {
		t.Fatalf("Info = %+v", infos)
	}

	files, err := c.Files(ctx, "aaaa")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 1 || files[0].Name != "Movie/movie.mkv" {
		t.Fatalf("Files = %+v", files)
	}

	if err := c.Delete(ctx, []string{"aaaa", "bbbb"}, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fake.deleted != "aaaa|bbbb" || fake.delFiles != "true" {
		t.Fatalf("Delete sent hashes=%q deleteFiles=%q", fake.deleted, fake.delFiles)
	}

	if fake.logins != 1 {
		t.Fatalf("logged in %d times, want 1 (the session cookie should be reused)", fake.logins)
	}
}

func TestAddRejected(t *testing.T) {
	fake := &fakeQbit{addReply: "Fails."}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c, _ := New(srv.URL, "admin", "secret")
	if err := c.Add(context.Background(), "magnet:?xt=urn:btih:aaaa", "/x"); !errors.Is(err, ErrAddRejected) {
		t.Fatalf("err = %v, want ErrAddRejected", err)
	}
}

func TestBadPassword(t *testing.T) {
	srv := httptest.NewServer((&fakeQbit{}).handler())
	defer srv.Close()
	c, _ := New(srv.URL, "admin", "wrong")
	if _, err := c.Info(context.Background(), nil); !errors.Is(err, ErrLogin) {
		t.Fatalf("err = %v, want ErrLogin", err)
	}
}
