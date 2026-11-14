// Package run holds the small pieces every service's main() needs.
package run

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Version is set at build time (the release tag); "dev" otherwise.
var Version = "dev"

// Logger returns a JSON logger tagged with the service name and version.
func Logger(service string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", service, "version", Version)
}

// Context is cancelled on Ctrl-C or SIGTERM (what Docker sends to stop a container).
func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Fatal logs an error and exits.
func Fatal(log *slog.Logger, msg string, err error) {
	log.Error(msg, "err", err)
	os.Exit(1)
}

// ServeHTTP runs an HTTP server until ctx is cancelled, then gives open
// requests up to 15 seconds to finish.
//
// There's deliberately no write timeout: downloads of large files can take
// hours.
func ServeHTTP(ctx context.Context, log *slog.Logger, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
