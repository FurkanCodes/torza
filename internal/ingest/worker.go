// Package ingest drives downloads. It hands queued torrents to qBittorrent,
// records their progress, and moves finished torrents into the cache.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"torza/internal/db"
	"torza/internal/qbit"
	"torza/internal/store"
)

// Repo is the part of the database ingest needs.
type Repo interface {
	TorrentsByStatus(ctx context.Context, statuses ...string) ([]db.Torrent, error)
	BlockedUnpurged(ctx context.Context) ([]db.Torrent, error)
	MarkDownloading(ctx context.Context, infohash string) error
	UpdateProgress(ctx context.Context, infohash, name string, size int64, progress float64) error
	MarkReady(ctx context.Context, infohash, name string, size int64, files []db.FileInput) error
	MarkFailed(ctx context.Context, infohash, reason string) error
	MarkPurged(ctx context.Context, infohash string) error
}

// Engine is the torrent client. *qbit.Client implements it.
type Engine interface {
	Add(ctx context.Context, magnetURI, savePath string) error
	Info(ctx context.Context, hashes []string) ([]qbit.Torrent, error)
	Files(ctx context.Context, hash string) ([]qbit.File, error)
	Delete(ctx context.Context, hashes []string, deleteFiles bool) error
}

type Config struct {
	MaxTorrentBytes int64         // 0 = no limit
	MetadataTimeout time.Duration // 0 = wait forever for peers
}

type Worker struct {
	repo   Repo
	engine Engine
	store  store.Store
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

func New(repo Repo, engine Engine, st store.Store, cfg Config, log *slog.Logger) *Worker {
	return &Worker{repo: repo, engine: engine, store: st, cfg: cfg, log: log, now: time.Now}
}

// Run checks for work every interval, or sooner when wake fires, until ctx ends.
func (w *Worker) Run(ctx context.Context, interval time.Duration, wake <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("ingest check", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// Tick does one round of work: delete blocked torrents, start queued ones,
// and update downloading ones. One failing step doesn't stop the others.
func (w *Worker) Tick(ctx context.Context) error {
	return errors.Join(w.purgeBlocked(ctx), w.startQueued(ctx), w.trackDownloads(ctx))
}

func (w *Worker) purgeBlocked(ctx context.Context) error {
	ts, err := w.repo.BlockedUnpurged(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		if err := errors.Join(
			w.engine.Delete(ctx, []string{t.InfoHash}, true),
			w.store.Remove(t.InfoHash),
		); err != nil {
			errs = append(errs, fmt.Errorf("purge %s: %w", t.InfoHash, err))
			continue
		}
		if err := w.repo.MarkPurged(ctx, t.InfoHash); err != nil {
			errs = append(errs, err)
			continue
		}
		w.log.Info("deleted blocked torrent", "infohash", t.InfoHash)
	}
	return errors.Join(errs...)
}

func (w *Worker) startQueued(ctx context.Context) error {
	ts, err := w.repo.TorrentsByStatus(ctx, db.StatusQueued)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		dir := w.store.IncomingDir(t.InfoHash)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := w.engine.Add(ctx, t.Magnet, dir); err != nil && !w.inEngine(ctx, t.InfoHash) {
			// qBittorrent refuses torrents it already has; that's fine.
			errs = append(errs, fmt.Errorf("start %s: %w", t.InfoHash, err))
			continue
		}
		if err := w.repo.MarkDownloading(ctx, t.InfoHash); err != nil {
			errs = append(errs, err)
			continue
		}
		w.log.Info("download started", "infohash", t.InfoHash, "name", t.Name)
	}
	return errors.Join(errs...)
}

func (w *Worker) trackDownloads(ctx context.Context) error {
	ts, err := w.repo.TorrentsByStatus(ctx, db.StatusDownloading)
	if err != nil || len(ts) == 0 {
		return err
	}
	hashes := make([]string, len(ts))
	for i, t := range ts {
		hashes[i] = t.InfoHash
	}
	infos, err := w.engine.Info(ctx, hashes)
	if err != nil {
		return err
	}
	byHash := make(map[string]qbit.Torrent, len(infos))
	for _, in := range infos {
		byHash[strings.ToLower(in.Hash)] = in
	}

	var errs []error
	for _, t := range ts {
		in, ok := byHash[t.InfoHash]
		if !ok {
			// qBittorrent lost it (for example its state was reset): hand it over again.
			w.log.Warn("torrent missing from qBittorrent, re-adding", "infohash", t.InfoHash)
			if err := w.engine.Add(ctx, t.Magnet, w.store.IncomingDir(t.InfoHash)); err != nil {
				errs = append(errs, fmt.Errorf("re-add %s: %w", t.InfoHash, err))
			}
			continue
		}
		if err := w.track(ctx, t, in); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.InfoHash, err))
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) track(ctx context.Context, t db.Torrent, in qbit.Torrent) error {
	size := in.TotalSize
	switch {
	case in.State == "error" || in.State == "missingFiles":
		return w.fail(ctx, t.InfoHash, "torrent engine reported: "+in.State)
	case size <= 0:
		// Still fetching the file list (metadata) from peers.
		if w.cfg.MetadataTimeout > 0 && w.now().Sub(time.Unix(in.AddedOn, 0)) > w.cfg.MetadataTimeout {
			return w.fail(ctx, t.InfoHash, "no peers found for this torrent")
		}
		return nil
	case w.cfg.MaxTorrentBytes > 0 && size > w.cfg.MaxTorrentBytes:
		return w.fail(ctx, t.InfoHash, fmt.Sprintf("torrent is %s; the limit is %s", humanBytes(size), humanBytes(w.cfg.MaxTorrentBytes)))
	case in.Progress >= 1 && !busy(in.State):
		return w.finish(ctx, t, in)
	default:
		return w.repo.UpdateProgress(ctx, t.InfoHash, in.Name, size, in.Progress)
	}
}

// busy reports qBittorrent states where the files may still change even at
// 100% progress.
func busy(state string) bool {
	return strings.HasPrefix(state, "checking") || state == "moving" || state == "metaDL" || state == "allocating"
}

func (w *Worker) finish(ctx context.Context, t db.Torrent, in qbit.Torrent) error {
	files, err := w.engine.Files(ctx, t.InfoHash)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("finished torrent reports no files")
	}
	inputs := make([]db.FileInput, 0, len(files))
	for _, f := range files {
		if !filepath.IsLocal(filepath.FromSlash(f.Name)) {
			return w.fail(ctx, t.InfoHash, "torrent contains an unsafe file path")
		}
		inputs = append(inputs, db.FileInput{Path: f.Name, SizeBytes: f.Size})
	}

	// Remove the torrent from qBittorrent but keep its files. That stops
	// seeding and releases the files so they can be moved.
	if err := w.engine.Delete(ctx, []string{t.InfoHash}, false); err != nil {
		return err
	}
	if err := w.store.Publish(t.InfoHash); err != nil {
		return err
	}
	if err := w.repo.MarkReady(ctx, t.InfoHash, in.Name, in.TotalSize, inputs); err != nil {
		// ErrConflict: it was blocked mid-download. purgeBlocked deletes the files.
		return err
	}
	w.log.Info("download ready", "infohash", t.InfoHash, "files", len(inputs), "bytes", in.TotalSize)
	return nil
}

// fail stops a download, deletes its partial files and records why.
func (w *Worker) fail(ctx context.Context, infohash, reason string) error {
	w.log.Warn("download failed", "infohash", infohash, "reason", reason)
	return errors.Join(
		w.engine.Delete(ctx, []string{infohash}, true),
		w.store.Remove(infohash),
		w.repo.MarkFailed(ctx, infohash, reason),
	)
}

func (w *Worker) inEngine(ctx context.Context, infohash string) bool {
	infos, err := w.engine.Info(ctx, []string{infohash})
	return err == nil && len(infos) > 0
}

func humanBytes(n int64) string {
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
