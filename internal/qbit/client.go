// Package qbit is a small client for qBittorrent's Web API (v2).
//
// Torza doesn't implement BitTorrent itself. qBittorrent does the downloading,
// and Torza drives it over HTTP: add a magnet, ask for progress, list files,
// remove the torrent.
package qbit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrLogin means qBittorrent rejected the username or password.
	ErrLogin = errors.New("qbittorrent login failed")
	// ErrAddRejected means qBittorrent refused a torrent (often because it already has it).
	ErrAddRejected = errors.New("qbittorrent rejected the torrent")
)

// Torrent is a torrent as qBittorrent reports it.
type Torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`   // 0 to 1
	TotalSize   int64   `json:"total_size"` // 0 until the file list (metadata) arrives
	AmountLeft  int64   `json:"amount_left"`
	ContentPath string  `json:"content_path"`
	AddedOn     int64   `json:"added_on"` // unix seconds
}

// File is one file inside a torrent.
type File struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"` // path relative to the save folder, "/"-separated
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
}

// Client talks to one qBittorrent instance.
type Client struct {
	base       string
	user, pass string
	http       *http.Client
}

func New(baseURL, user, pass string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		user: user,
		pass: pass,
		http: &http.Client{Jar: jar, Timeout: 30 * time.Second},
	}, nil
}

// Add hands a magnet link to qBittorrent and tells it where to save the files.
func (c *Client) Add(ctx context.Context, magnetURI, savePath string) error {
	body, err := c.call(ctx, func() (*http.Request, error) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for k, v := range map[string]string{"urls": magnetURI, "savepath": savePath, "autoTMM": "false"} {
			if err := w.WriteField(k, v); err != nil {
				return nil, err
			}
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2/torrents/add", &buf)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		return req, nil
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(body)) == "Fails." {
		return ErrAddRejected
	}
	return nil
}

// Info returns the torrents with the given hashes. qBittorrent leaves out
// hashes it doesn't have.
func (c *Client) Info(ctx context.Context, hashes []string) ([]Torrent, error) {
	q := url.Values{}
	if len(hashes) > 0 {
		q.Set("hashes", strings.Join(hashes, "|"))
	}
	body, err := c.call(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v2/torrents/info?"+q.Encode(), nil)
	})
	if err != nil {
		return nil, err
	}
	var out []Torrent
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("qbittorrent torrents/info: %w", err)
	}
	return out, nil
}

// Files lists the files inside a torrent.
func (c *Client) Files(ctx context.Context, hash string) ([]File, error) {
	q := url.Values{"hash": {hash}}
	body, err := c.call(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v2/torrents/files?"+q.Encode(), nil)
	})
	if err != nil {
		return nil, err
	}
	var out []File
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("qbittorrent torrents/files: %w", err)
	}
	return out, nil
}

// Delete removes torrents from qBittorrent. With deleteFiles false the
// downloaded files stay on disk.
func (c *Client) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	form := url.Values{
		"hashes":      {strings.Join(hashes, "|")},
		"deleteFiles": {fmt.Sprint(deleteFiles)},
	}
	_, err := c.call(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2/torrents/delete", strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	})
	return err
}

// call sends a request. If qBittorrent answers 403 (not logged in), it logs in
// once and retries. newReq builds a fresh request each time because a request
// body can only be read once.
func (c *Client) call(ctx context.Context, newReq func() (*http.Request, error)) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Referer", c.base)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusForbidden && attempt == 0 {
			if err := c.login(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("qbittorrent %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
		}
		return body, nil
	}
}

func (c *Client) login(ctx context.Context) error {
	form := url.Values{"username": {c.user}, "password": {c.pass}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.base)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return ErrLogin
	}
	return nil
}
