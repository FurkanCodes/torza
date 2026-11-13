// Package ingest fills the cache. One Worker runs one fetcher (qBittorrent
// today): it claims queued items for that fetcher, drives them through the
// source, and moves finished files into the cache.
//
// An item with a handle belongs to the worker of its fetcher. Only that
// worker deletes what the fetcher wrote, including after a takedown.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"torza/internal/db"
	"torza/internal/source"
	"torza/internal/store"
)

// Repo is the part of the database ingest needs. *db.Repo implements it.
type Repo interface {
	Claim(ctx context.Context, fetcher string, n int, defaultMaxBytes int64) ([]db.Job, error)
	Held(ctx context.Context, fetcher string, defaultMaxBytes int64) ([]db.Job, error)
	SetHandle(ctx context.Context, itemID int64, fetcher string, handle []byte) error
	Progress(ctx context.Context, itemID int64, fetcher, phase, name string, size int64, progress float64) error
	BeginFinalize(ctx context.Context, itemID int64, fetcher, name string, files []db.ManifestFile) error
	MarkReady(ctx context.Context, itemID int64, fetcher string) error
	Fail(ctx context.Context, itemID int64, fetcher, reason string) error
	RequeueHeld(ctx context.Context, itemID int64, fetcher, reason string) error
	BlockedHeld(ctx context.Context, fetcher string) ([]db.Job, error)
	PurgedHeld(ctx context.Context, itemID int64, fetcher string) error
	LegacyFolders(ctx context.Context) (map[int64]string, error)
	LegacyFolderDone(ctx context.Context, itemID int64) error
}

type Config struct {
	MaxItemBytes int64 // server default for users without their own limit; 0 = none
	ClaimBatch   int   // most items claimed per tick
	Policy       Policy
}

type Worker struct {
	repo  Repo
	src   source.Source
	store store.Store
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
}

func New(repo Repo, src source.Source, st store.Store, cfg Config, log *slog.Logger) *Worker {
	if cfg.ClaimBatch <= 0 {
		cfg.ClaimBatch = 50
	}
	return &Worker{repo: repo, src: src, store: st, cfg: cfg, log: log.With("fetcher", src.Name()), now: time.Now}
}

// Run checks for work every interval, or sooner when wake fires, until ctx ends.
func (w *Worker) Run(ctx context.Context, interval time.Duration, wake <-chan struct{}) {
	if err := w.adoptLegacy(ctx); err != nil {
		w.log.Error("convert old cache folders", "err", err)
	}
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

// Tick does one round: stop blocked work, move held items along (which also
// recovers anything a crash interrupted), then claim and start new work.
// One failing step doesn't stop the others.
func (w *Worker) Tick(ctx context.Context) error {
	errs := []error{w.purgeBlocked(ctx)}
	held, err := w.repo.Held(ctx, w.src.Name(), w.cfg.MaxItemBytes)
	errs = append(errs, err)
	for _, j := range held {
		errs = append(errs, w.step(ctx, j))
	}
	claimed, err := w.repo.Claim(ctx, w.src.Name(), w.cfg.ClaimBatch, w.cfg.MaxItemBytes)
	errs = append(errs, err)
	for _, j := range claimed {
		errs = append(errs, w.step(ctx, j))
	}
	return errors.Join(errs...)
}

func (w *Worker) job(j db.Job) source.Job {
	return source.Job{ItemID: j.ItemID, Key: j.Key, Payload: j.Payload, Handle: j.Handle, Dir: w.store.IncomingDir(j.ItemID)}
}

// step moves one held item along and handles what goes wrong.
func (w *Worker) step(ctx context.Context, j db.Job) error {
	var err error
	if j.Status == db.StatusFinalizing {
		err = w.recoverFinalizing(ctx, j)
	} else {
		err = w.advance(ctx, j)
	}
	switch {
	case err == nil, errors.Is(err, db.ErrConflict):
		// ErrConflict: blocked meanwhile; purgeBlocked cleans up next tick.
		return nil
	case source.ClassOf(err) == source.Retry:
		return fmt.Errorf("item %d: %w", j.ItemID, err)
	default:
		return w.fail(ctx, j, source.Message(err))
	}
}

func (w *Worker) advance(ctx context.Context, j db.Job) error {
	sj := w.job(j)
	if sj.Handle == nil {
		// Claimed but not started, or a crash came before the handle was
		// stored: Start again. Sources make a second Start harmless.
		h, err := w.src.Start(ctx, sj)
		if err != nil {
			return err
		}
		if err := w.repo.SetHandle(ctx, j.ItemID, w.src.Name(), h); err != nil {
			return err
		}
		sj.Handle = h
		w.log.Info("download started", "item", j.ItemID, "key", j.Key.Value)
	}
	r, err := w.src.Poll(ctx, sj)
	if err != nil {
		return err
	}
	if d := w.cfg.Policy.Decide(j, r, w.now()); d.Fail != "" {
		return source.Fail(source.Permanent, d.Fail)
	}
	if r.Done {
		return w.finish(ctx, j, sj)
	}
	progress := 0.0
	if r.BytesTotal > 0 {
		progress = float64(r.BytesDone) / float64(r.BytesTotal)
	}
	return w.repo.Progress(ctx, j.ItemID, w.src.Name(), r.Phase, r.Name, r.BytesTotal, progress)
}

// finish stores the manifest before the source lets go of the files, so a
// crash at any later point is finished from the database and the disk:
//  1. Finalize: the final file list.
//  2. One transaction: file rows (pending) and status finalizing.
//  3. The source releases the files; 4. rename into store/; 5. ready.
func (w *Worker) finish(ctx context.Context, j db.Job, sj source.Job) error {
	m, err := w.src.Finalize(ctx, sj)
	if err != nil {
		return err
	}
	if err := store.CheckManifest(m.Root, m.Files); err != nil {
		return &source.Error{Class: source.Permanent, Msg: "the download contains unsafe or unexpected files", Err: err}
	}
	if err := w.repo.BeginFinalize(ctx, j.ItemID, w.src.Name(), m.Name, m.Files); err != nil {
		return err
	}
	return w.publish(ctx, j, sj, m.Root)
}

func (w *Worker) publish(ctx context.Context, j db.Job, sj source.Job, root string) error {
	if err := w.src.Release(ctx, sj); err != nil {
		return err
	}
	if err := w.store.Publish(root, j.ItemID); err != nil {
		return err
	}
	if err := w.repo.MarkReady(ctx, j.ItemID, w.src.Name()); err != nil {
		return err
	}
	w.log.Info("download ready", "item", j.ItemID)
	return nil
}

// recoverFinalizing finishes an item a crash left finalizing, from what is
// on disk: already in store/ → ready; still in its download folder →
// release, rename, ready; neither → download it again.
func (w *Worker) recoverFinalizing(ctx context.Context, j db.Job) error {
	sj := w.job(j)
	if w.store.Published(j.ItemID) {
		return w.repo.MarkReady(ctx, j.ItemID, w.src.Name())
	}
	root, err := w.src.Root(sj)
	if err != nil {
		return err
	}
	if store.Exists(root) {
		return w.publish(ctx, j, sj, root)
	}
	w.log.Warn("finished download vanished before it was moved; downloading again", "item", j.ItemID)
	return w.repo.RequeueHeld(ctx, j.ItemID, w.src.Name(), "")
}

// fail stops a download, deletes its partial files and records why.
func (w *Worker) fail(ctx context.Context, j db.Job, reason string) error {
	w.log.Warn("download failed", "item", j.ItemID, "reason", reason)
	err := errors.Join(w.src.Cancel(ctx, w.job(j), true), w.store.Remove(j.ItemID))
	if err != nil {
		return fmt.Errorf("item %d: clean up failed download: %w", j.ItemID, err)
	}
	if err := w.repo.Fail(ctx, j.ItemID, w.src.Name(), reason); err != nil && !errors.Is(err, db.ErrConflict) {
		return err
	}
	return nil
}

// purgeBlocked stops blocked downloads this fetcher holds and deletes their
// files.
func (w *Worker) purgeBlocked(ctx context.Context) error {
	jobs, err := w.repo.BlockedHeld(ctx, w.src.Name())
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if err := errors.Join(w.src.Cancel(ctx, w.job(j), true), w.store.Remove(j.ItemID)); err != nil {
			errs = append(errs, fmt.Errorf("purge item %d: %w", j.ItemID, err))
			continue
		}
		if err := w.repo.PurgedHeld(ctx, j.ItemID, w.src.Name()); err != nil {
			errs = append(errs, err)
			continue
		}
		w.log.Info("deleted blocked download", "item", j.ItemID)
	}
	return errors.Join(errs...)
}

// adoptLegacy renames finished folders from the old torrent-only layout.
func (w *Worker) adoptLegacy(ctx context.Context) error {
	folders, err := w.repo.LegacyFolders(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for id, folder := range folders {
		if err := w.store.AdoptLegacy(id, folder); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, w.repo.LegacyFolderDone(ctx, id))
	}
	if len(folders) > 0 {
		w.log.Info("converted old cache folders", "count", len(folders))
	}
	return errors.Join(errs...)
}
