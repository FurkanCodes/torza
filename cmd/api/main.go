// Command api runs Torza's public REST API.
//
//	api                                   start the server
//	api create-user -email you@x [-admin] create a user and print their API key
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"torza/internal/api"
	"torza/internal/auth"
	"torza/internal/config"
	"torza/internal/db"
	"torza/internal/jobs"
	"torza/internal/links"
	"torza/internal/run"
)

func main() {
	log := run.Logger("api")
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
	repo := db.NewRepo(pool)

	if len(os.Args) > 1 && os.Args[1] == "create-user" {
		if err := createUser(ctx, repo, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "create-user:", err)
			os.Exit(1)
		}
		return
	}

	if err := cfg.RequireSigningKey(); err != nil {
		run.Fatal(log, "config", err)
	}
	nc, err := jobs.Connect(cfg.NATSURL, log)
	if err != nil {
		run.Fatal(log, "nats", err)
	}
	defer nc.Close()

	srv := api.New(repo, jobs.NewNotifier(nc), links.NewSigner(cfg.SigningKey), api.Config{
		PublicURL:        cfg.PublicURL,
		LinkTTL:          cfg.LinkTTL,
		MaxActivePerUser: cfg.MaxActivePerUser,
	}, log)
	if err := run.ServeHTTP(ctx, log, cfg.APIAddr, srv.Handler()); err != nil {
		run.Fatal(log, "http", err)
	}
}

func createUser(ctx context.Context, repo *db.Repo, args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
	email := fs.String("email", "", "the user's email address")
	admin := fs.Bool("admin", false, "allow this user to handle takedowns")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("-email is required")
	}
	key, hash, err := auth.NewAPIKey()
	if err != nil {
		return err
	}
	u, err := repo.CreateUser(ctx, *email, *admin, hash, auth.DisplayPrefix(key))
	if err != nil {
		return err
	}
	fmt.Printf("Created user %d (%s, admin=%v)\nAPI key: %s\nSave it now. Torza only stores a hash, so it can't show the key again.\n", u.ID, u.Email, u.IsAdmin, key)
	return nil
}
