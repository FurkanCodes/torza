// Package service holds the rules every way into Torza shares: the REST API,
// the web page and the stream service call it, never the database directly.
package service

import (
	"context"
	"errors"
	"net/url"
	"path"
	"time"

	"torza/internal/db"
	"torza/internal/links"
	"torza/internal/magnet"
)

var (
	// ErrNotReady means the item isn't cached yet; Item says what it is doing.
	ErrNotReady = errors.New("not ready")
	// ErrGone means the file isn't part of the item's current download.
	ErrGone = errors.New("file is no longer part of this item")
)

// FetcherFor is which fetcher fills content of a key kind. Stage 1 has one.
func FetcherFor(kind string) string { return "qbittorrent" }

type Config struct {
	PublicURL string        // where the stream service is reached
	LinkTTL   time.Duration // how long a download link works
	Defaults  db.Limits     // limits for users the owner hasn't set
}

type Service struct {
	repo   *db.Repo
	signer *links.Signer
	cfg    Config
	now    func() time.Time
}

func New(repo *db.Repo, signer *links.Signer, cfg Config) *Service {
	return &Service{repo: repo, signer: signer, cfg: cfg, now: time.Now}
}

// Key turns a parsed magnet into its source key.
func Key(m magnet.Magnet) db.Key { return db.Key{Kind: db.KindTorrent, Value: m.InfoHash} }

// Add puts content in a user's library, starting a download if it isn't
// cached or on its way.
func (s *Service) Add(ctx context.Context, userID int64, m magnet.Magnet) (db.AddResult, error) {
	return s.repo.Add(ctx, userID, Key(m), m.URI(), m.Name, FetcherFor(db.KindTorrent), s.cfg.Defaults)
}

// CheckResult is one answer of Check.
type CheckResult struct {
	Input string
	Valid bool
	db.Availability
}

// Check reports whether each input is cached, on its way or unknown, without
// starting anything. Inputs that aren't magnets or hashes come back invalid.
func (s *Service) Check(ctx context.Context, inputs []string) ([]CheckResult, error) {
	out := make([]CheckResult, len(inputs))
	var keys []db.Key
	var at []int
	for i, in := range inputs {
		out[i].Input = in
		m, err := magnet.Parse(in)
		if err != nil {
			continue
		}
		out[i].Valid = true
		keys = append(keys, Key(m))
		at = append(at, i)
	}
	if len(keys) == 0 {
		return out, nil
	}
	avail, err := s.repo.Check(ctx, keys)
	if err != nil {
		return nil, err
	}
	for j, a := range avail {
		out[at[j]].Availability = a
	}
	return out, nil
}

func (s *Service) Library(ctx context.Context, userID int64) ([]db.LibraryItem, error) {
	return s.repo.Library(ctx, userID)
}

func (s *Service) Item(ctx context.Context, userID, itemID int64) (db.Item, []db.File, error) {
	return s.repo.LibraryItem(ctx, userID, itemID)
}

func (s *Service) Remove(ctx context.Context, userID, itemID int64) error {
	return s.repo.RemoveFromLibrary(ctx, userID, itemID)
}

// Link is a signed download link.
type Link struct {
	FileID    int64     `json:"file_id"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FileLink makes a link for one file. If the item was evicted or failed, it
// is queued again as this user and ErrNotReady comes back with the item.
func (s *Service) FileLink(ctx context.Context, userID, fileID int64) (Link, db.Item, error) {
	f, it, err := s.repo.FileForUser(ctx, userID, fileID)
	if err != nil {
		return Link{}, db.Item{}, err
	}
	it, err = s.ready(ctx, userID, it)
	if err != nil {
		return Link{}, it, err
	}
	if f.State != db.FilePresent {
		return Link{}, it, ErrGone
	}
	return s.sign(userID, f), it, nil
}

// ItemLinks makes links for every file of a ready item, in one call.
func (s *Service) ItemLinks(ctx context.Context, userID, itemID int64) ([]Link, db.Item, error) {
	it, files, err := s.repo.LibraryItem(ctx, userID, itemID)
	if err != nil {
		return nil, db.Item{}, err
	}
	if it, err = s.ready(ctx, userID, it); err != nil {
		return nil, it, err
	}
	out := make([]Link, 0, len(files))
	for _, f := range files {
		out = append(out, s.sign(userID, f))
	}
	return out, it, nil
}

// ready returns nil for a ready item. Blocked → ErrBlocked. Evicted or
// failed → queued again as this user, then ErrNotReady.
func (s *Service) ready(ctx context.Context, userID int64, it db.Item) (db.Item, error) {
	switch it.Status {
	case db.StatusReady:
		return it, nil
	case db.StatusBlocked:
		return it, db.ErrBlocked
	case db.StatusEvicted, db.StatusFailed:
		again, err := s.repo.Requeue(ctx, userID, it.ID, FetcherFor(db.KindTorrent), s.cfg.Defaults)
		if err != nil {
			return it, err
		}
		return again, ErrNotReady
	}
	return it, ErrNotReady
}

func (s *Service) sign(userID int64, f db.File) Link {
	expires := s.now().Add(s.cfg.LinkTTL).Truncate(time.Second)
	token := s.signer.Sign(links.Claims{FileID: f.ID, UserID: userID, Expires: expires})
	return Link{
		FileID: f.ID, Path: f.Path, SizeBytes: f.SizeBytes, ExpiresAt: expires.UTC(),
		URL: s.cfg.PublicURL + "/d/" + token + "/" + url.PathEscape(path.Base(f.Path)),
	}
}

// Readable checks a user may read a file right now, for the stream service.
// It re-checks everything a signed link can't: the user still has the item,
// it is ready and not blocked, and the file is part of it.
func (s *Service) Readable(ctx context.Context, userID, fileID int64) (db.File, db.Item, error) {
	f, it, err := s.repo.FileForUser(ctx, userID, fileID)
	if err != nil {
		return db.File{}, db.Item{}, err
	}
	switch {
	case it.Status == db.StatusBlocked:
		return f, it, db.ErrBlocked
	case it.Status != db.StatusReady:
		return f, it, ErrNotReady
	case f.State != db.FilePresent:
		return f, it, ErrGone
	}
	return f, it, nil
}

// Touch records real use of an item, for eviction.
func (s *Service) Touch(ctx context.Context, itemID int64) error { return s.repo.Touch(ctx, itemID) }

// Account is what /v1/me shows.
type Account struct {
	User   db.User
	Limits db.Limits
	Usage  db.Usage
}

func (s *Service) Account(ctx context.Context, u db.User) (Account, error) {
	l, err := s.repo.LimitsFor(ctx, u.ID, s.cfg.Defaults)
	if err != nil {
		return Account{}, err
	}
	usage, err := s.repo.UsageFor(ctx, u.ID)
	return Account{User: u, Limits: l, Usage: usage}, err
}

func (s *Service) Authorize(ctx context.Context, keyHash []byte) (db.User, error) {
	return s.repo.UserByKeyHash(ctx, keyHash)
}

// Block takes content down by magnet or hash, for the owner.
func (s *Service) Block(ctx context.Context, admin db.User, m magnet.Magnet, reason, reference string) (int64, error) {
	return s.repo.Block(ctx, Key(m), reason, reference, admin.ID)
}

// SetLimits sets a user's own limits; nil means the server default.
func (s *Service) SetLimits(ctx context.Context, userID int64, maxActive *int, maxItemBytes *int64, priority int) error {
	return s.repo.SetLimits(ctx, userID, maxActive, maxItemBytes, priority)
}
