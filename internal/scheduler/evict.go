// Package scheduler runs housekeeping. Today that's cache eviction: deleting
// the least recently used torrents when the cache is too big or the disk is
// too full.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"torza/internal/db"
)

// Repo is the part of the database eviction needs.
type Repo interface {
	CacheBytes(ctx context.Context) (int64, error)
	EvictionCandidates(ctx context.Context, idleSince time.Time, limit int) ([]db.Torrent, error)
	MarkEvicted(ctx context.Context, infohash string) error
}

// Disk is the cache on disk. store.Store implements it.
type Disk interface {
	FreeBytes() (int64, error)
	Remove(infohash string) error
}

type Config struct {
	MaxBytes     int64         // evict when the cache is bigger than this (0 = no limit)
	MinFreeBytes int64         // evict when free disk space is below this (0 = no limit)
	IdleFor      time.Duration // never evict a torrent used more recently than this
}

const candidatesPerRun = 500

type Evictor struct {
	repo Repo
	disk Disk
	cfg  Config
	log  *slog.Logger
	now  func() time.Time
}

func New(repo Repo, disk Disk, cfg Config, log *slog.Logger) *Evictor {
	return &Evictor{repo: repo, disk: disk, cfg: cfg, log: log, now: time.Now}
}

// Plan picks which torrents to evict. Candidates must be sorted least recently
// used first. It stops as soon as the cache is under MaxBytes and the disk has
// MinFreeBytes free.
func Plan(candidates []db.Torrent, cacheBytes, freeBytes int64, cfg Config) []db.Torrent {
	var out []db.Torrent
	for _, t := range candidates {
		if !overLimits(cacheBytes, freeBytes, cfg) {
			break
		}
		out = append(out, t)
		cacheBytes -= t.SizeBytes
		freeBytes += t.SizeBytes
	}
	return out
}

func overLimits(cacheBytes, freeBytes int64, cfg Config) bool {
	return (cfg.MaxBytes > 0 && cacheBytes > cfg.MaxBytes) || (cfg.MinFreeBytes > 0 && freeBytes < cfg.MinFreeBytes)
}

// RunOnce checks the limits and evicts what's needed.
func (e *Evictor) RunOnce(ctx context.Context) error {
	cacheBytes, err := e.repo.CacheBytes(ctx)
	if err != nil {
		return err
	}
	free, err := e.disk.FreeBytes()
	if err != nil {
		return err
	}
	if !overLimits(cacheBytes, free, e.cfg) {
		return nil
	}
	candidates, err := e.repo.EvictionCandidates(ctx, e.now().Add(-e.cfg.IdleFor), candidatesPerRun)
	if err != nil {
		return err
	}
	plan := Plan(candidates, cacheBytes, free, e.cfg)

	var errs []error
	for _, t := range plan {
		// Mark it first so no new links are made, then delete the files.
		if err := e.repo.MarkEvicted(ctx, t.InfoHash); err != nil {
			if !errors.Is(err, db.ErrConflict) { // someone blocked it meanwhile: fine
				errs = append(errs, fmt.Errorf("evict %s: %w", t.InfoHash, err))
			}
			continue
		}
		if err := e.disk.Remove(t.InfoHash); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", t.InfoHash, err))
			continue
		}
		cacheBytes -= t.SizeBytes
		free += t.SizeBytes
		e.log.Info("evicted", "infohash", t.InfoHash, "bytes", t.SizeBytes, "last_used", t.LastAccessedAt)
	}
	if overLimits(cacheBytes, free, e.cfg) && len(plan) == len(candidates) && len(candidates) < candidatesPerRun {
		e.log.Warn("cache is still over its limit; everything left was used recently", "cache_bytes", cacheBytes, "free_bytes", free)
	}
	return errors.Join(errs...)
}

// Run calls RunOnce every interval until ctx ends.
func (e *Evictor) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.RunOnce(ctx); err != nil && ctx.Err() == nil {
			e.log.Error("eviction", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
