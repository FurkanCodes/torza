// Command ingest fills the cache: it runs one loop per fetcher (qBittorrent
// today), claiming queued items and moving finished downloads into the cache.
package main

import (
	"fmt"
	"strings"
	"sync"

	"torza/internal/config"
	"torza/internal/db"
	"torza/internal/ingest"
	"torza/internal/qbit"
	"torza/internal/run"
	"torza/internal/source"
	"torza/internal/source/qbitsource"
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
	repo := db.NewRepo(pool)

	var sources []source.Source
	for _, name := range cfg.Fetchers {
		switch strings.TrimSpace(name) {
		case "qbittorrent":
			qb, err := qbit.New(cfg.QbitURL, cfg.QbitUser, cfg.QbitPass)
			if err != nil {
				run.Fatal(log, "qbittorrent", err)
			}
			sources = append(sources, qbitsource.New(qb, cfg.DataDir))
		default:
			run.Fatal(log, "config", fmt.Errorf("INGEST_FETCHERS: unknown fetcher %q", name))
		}
	}

	var wg sync.WaitGroup
	for _, src := range sources {
		wake := db.Listen(ctx, pool, log, db.ChanQueued(src.Name()), db.ChanBlocked)
		w := ingest.New(repo, src, st, ingest.Config{
			MaxItemBytes: cfg.MaxTorrentBytes,
			Policy:       ingest.Policy{MetadataTimeout: cfg.MetadataTimeout},
		}, log)
		log.Info("ingest running", "fetcher", src.Name(), "poll_interval", cfg.PollInterval.String())
		wg.Go(func() { w.Run(ctx, cfg.PollInterval, wake) })
	}
	wg.Wait()
}
