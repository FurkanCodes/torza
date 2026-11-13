// Command stream serves cached files to holders of signed download links.
package main

import (
	"torza/internal/config"
	"torza/internal/db"
	"torza/internal/links"
	"torza/internal/run"
	"torza/internal/service"
	"torza/internal/store"
	"torza/internal/stream"
)

func main() {
	log := run.Logger("stream")
	cfg, err := config.Load()
	if err != nil {
		run.Fatal(log, "config", err)
	}
	if err := cfg.RequireSigningKey(); err != nil {
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

	signer := links.NewSigner(cfg.SigningKey)
	svc := service.New(db.NewRepo(pool), signer, service.Config{PublicURL: cfg.PublicURL, LinkTTL: cfg.LinkTTL})
	srv := stream.New(svc, signer, store.New(cfg.DataDir), log)
	if err := run.ServeHTTP(ctx, log, cfg.StreamAddr, srv.Handler()); err != nil {
		run.Fatal(log, "http", err)
	}
}
