// Package scheduler runs housekeeping: evicting least recently used items
// when the cache is too big or the disk too full, deleting the files of
// blocked items no fetcher holds, and sweeping folders no item owns.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"torza/internal/db"
)

// Repo is the part of the database the scheduler needs. *db.Repo implements it.
type Repo interface {
	UsedBytes(ctx context.Context) (int64, error)
	EvictionCandidates(ctx context.Context, idleSince time.Time, limit int) ([]db.Item, error)
	MarkEvicted(ctx context.Context, itemID int64) error
	BlockedUnheld(ctx context.Context) ([]int64, error)
	MarkPurged(ctx context.Context, itemID int64) error
	KeepFolders(ctx context.Context) (map[int64]bool, error)
}

// Disk is the cache on disk. store.Store implements it.
type Disk interface {
	FreeBytes() (int64, error)
	Remove(itemID int64) error
	Sweep(keep map[int64]bool, minAge time.Duration, now time.Time) ([]string, error)
}

type Config struct {
	MaxBytes     int64         // evict when the cache is bigger than this (0 = no limit)
	MinFreeBytes int64         // evict when free disk space is below this (0 = no limit)
	IdleFor      time.Duration // never evict an item used more recently than this
	SweepMinAge  time.Duration // leave folders younger than this alone
}

const candidatesPerRun = 500

type Scheduler struct {
	repo Repo
	disk Disk
	cfg  Config
	log  *slog.Logger
	now  func() time.Time
}

func New(repo Repo, disk Disk, cfg Config, log *slog.Logger) *Scheduler {
	if cfg.SweepMinAge <= 0 {
		cfg.SweepMinAge = time.Hour
	}
	return &Scheduler{repo: repo, disk: disk, cfg: cfg, log: log, now: time.Now}
}

// Plan picks which items to evict. Candidates must be sorted least recently
// used first. It stops as soon as the cache is under MaxBytes and the disk
// has MinFreeBytes free.
func Plan(candidates []db.Item, usedBytes, freeBytes int64, cfg Config) []db.Item {
	var out []db.Item
	for _, it := range candidates {
		if !overLimits(usedBytes, freeBytes, cfg) {
			break
		}
		out = append(out, it)
		usedBytes -= it.SizeBytes
		freeBytes += it.SizeBytes
	}
	return out
}

func overLimits(usedBytes, freeBytes int64, cfg Config) bool {
	return (cfg.MaxBytes > 0 && usedBytes > cfg.MaxBytes) || (cfg.MinFreeBytes > 0 && freeBytes < cfg.MinFreeBytes)
}

// RunOnce does every job once.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	return errors.Join(s.purge(ctx), s.evict(ctx), s.sweep(ctx))
}

// evict deletes least recently used ready items while the cache is over its
// limits. "Used" counts ready items plus the known sizes of downloads in
// progress, so it makes room for what is arriving.
func (s *Scheduler) evict(ctx context.Context) error {
	used, err := s.repo.UsedBytes(ctx)
	if err != nil {
		return err
	}
	free, err := s.disk.FreeBytes()
	if err != nil {
		return err
	}
	if !overLimits(used, free, s.cfg) {
		return nil
	}
	candidates, err := s.repo.EvictionCandidates(ctx, s.now().Add(-s.cfg.IdleFor), candidatesPerRun)
	if err != nil {
		return err
	}
	plan := Plan(candidates, used, free, s.cfg)
	var errs []error
	for _, it := range plan {
		// Mark it first so no new links are made, then delete the files.
		if err := s.repo.MarkEvicted(ctx, it.ID); err != nil {
			if !errors.Is(err, db.ErrConflict) { // blocked meanwhile: fine
				errs = append(errs, fmt.Errorf("evict item %d: %w", it.ID, err))
			}
			continue
		}
		if err := s.disk.Remove(it.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete item %d: %w", it.ID, err))
			continue
		}
		used -= it.SizeBytes
		free += it.SizeBytes
		s.log.Info("evicted", "item", it.ID, "bytes", it.SizeBytes, "last_used", it.LastUsedAt)
	}
	if overLimits(used, free, s.cfg) && len(plan) == len(candidates) && len(candidates) < candidatesPerRun {
		s.log.Warn("cache is still over its limit; everything left was used recently or is downloading", "used_bytes", used, "free_bytes", free)
	}
	return errors.Join(errs...)
}

// purge deletes the files of blocked items that no fetcher holds (ingest
// purges the ones it holds).
func (s *Scheduler) purge(ctx context.Context) error {
	ids, err := s.repo.BlockedUnheld(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := s.disk.Remove(id); err != nil {
			errs = append(errs, fmt.Errorf("purge item %d: %w", id, err))
			continue
		}
		if err := s.repo.MarkPurged(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.Info("deleted blocked item", "item", id)
	}
	return errors.Join(errs...)
}

// sweep deletes item folders whose item is not active, finalizing or ready:
// what a crash between deleting rows and files can leave behind.
func (s *Scheduler) sweep(ctx context.Context) error {
	keep, err := s.repo.KeepFolders(ctx)
	if err != nil {
		return err
	}
	removed, err := s.disk.Sweep(keep, s.cfg.SweepMinAge, s.now())
	for _, p := range removed {
		s.log.Warn("deleted a folder no item owns", "path", p)
	}
	return err
}

// Run calls RunOnce every interval until ctx ends.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("housekeeping", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
