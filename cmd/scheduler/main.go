// Command scheduler evicts least recently used torrents when the cache is too
// big or the disk is too full.
package main

import (
	"torza/internal/config"
	"torza/internal/db"
	"torza/internal/run"
	"torza/internal/scheduler"
	"torza/internal/store"
)

func main() {
	log := run.Logger("scheduler")
	cfg, err := config.Load()
	if err != nil {
		run.Fatal(log, "config", err)
	}
	ctx, stop := run.Context()
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		run.Fatal(log, "database", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		run.Fatal(log, "migrate", err)
	}

	e := scheduler.New(db.NewRepo(pool), store.New(cfg.DataDir), scheduler.Config{
		MaxBytes:     cfg.CacheMaxBytes,
		MinFreeBytes: cfg.CacheMinFreeBytes,
		IdleFor:      cfg.EvictIdleFor,
	}, log)
	log.Info("scheduler running", "interval", cfg.EvictInterval.String())
	e.Run(ctx, cfg.EvictInterval)
}
