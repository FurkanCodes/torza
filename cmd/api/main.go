// Command api runs Torza's public REST API.
//
//	api                                   start the server
//	api migrate                           run database migrations and exit
//	api create-user -email you@x [-admin] create a user and print their API key
//	api qbit-config -user admin -password … print a qBittorrent.conf for a new install
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
	"torza/internal/links"
	"torza/internal/qbit"
	"torza/internal/run"
	"torza/internal/service"
)

func main() {
	// Needs no database or settings: the installer runs it before anything starts.
	if len(os.Args) > 1 && os.Args[1] == "qbit-config" {
		if err := qbitConfig(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "qbit-config:", err)
			os.Exit(1)
		}
		return
	}
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

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		fmt.Println("migrations are up to date")
		return
	}
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
	svc := service.New(repo, links.NewSigner(cfg.SigningKey), service.Config{
		PublicURL: cfg.PublicURL,
		LinkTTL:   cfg.LinkTTL,
		Defaults:  db.Limits{MaxActive: cfg.MaxActivePerUser, MaxItemBytes: cfg.MaxTorrentBytes},
	})
	srv := api.New(svc, api.Config{PublicURL: cfg.PublicURL, BaseURL: cfg.APIURL}, log)
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

func qbitConfig(args []string) error {
	fs := flag.NewFlagSet("qbit-config", flag.ContinueOnError)
	user := fs.String("user", "admin", "qBittorrent Web UI user")
	password := fs.String("password", "", "qBittorrent Web UI password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(*password) < 12 {
		return errors.New("-password must be at least 12 characters")
	}
	conf, err := qbit.Config(*user, *password)
	if err != nil {
		return err
	}
	fmt.Print(conf)
	return nil
}
