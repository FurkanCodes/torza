// Command ingest hands queued torrents to qBittorrent and moves finished ones
// into the cache.
package main

import (
	"torza/internal/config"
	"torza/internal/db"
	"torza/internal/ingest"
	"torza/internal/jobs"
	"torza/internal/qbit"
	"torza/internal/run"
	"torza/internal/store"
)

func main() {
	log := run.Logger("ingest")
	cfg, err := config.Load()
	if err != nil {
		run.Fatal(log, "config", err)
	}
	ctx, stop := run.Context()
	defer stop()

	st := store.New(cfg.DataDir)
	if err := st.EnsureDirs(); err != nil {
		run.Fatal(log, "data dir", err)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		run.Fatal(log, "database", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		run.Fatal(log, "migrate", err)
	}
	qb, err := qbit.New(cfg.QbitURL, cfg.QbitUser, cfg.QbitPass)
	if err != nil {
		run.Fatal(log, "qbittorrent", err)
	}
	nc, err := jobs.Connect(cfg.NATSURL, log)
	if err != nil {
		run.Fatal(log, "nats", err)
	}
	defer nc.Close()
	wake, err := jobs.Wake(nc, jobs.IngestQueue, jobs.SubjectQueued, jobs.SubjectBlocked)
	if err != nil {
		run.Fatal(log, "nats subscribe", err)
	}

	w := ingest.New(db.NewRepo(pool), qb, st, ingest.Config{
		MaxTorrentBytes: cfg.MaxTorrentBytes,
		MetadataTimeout: cfg.MetadataTimeout,
	}, log)
	log.Info("ingest running", "poll_interval", cfg.PollInterval.String())
	w.Run(ctx, cfg.PollInterval, wake)
}
