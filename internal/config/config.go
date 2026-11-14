// Package config reads Torza's settings from environment variables, so the same
// programs run unchanged on a laptop, in Docker or on a server.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting. Each service only uses the parts it needs.
type Config struct {
	DatabaseURL string
	DataDir     string // holds incoming/ (downloading) and store/ (the cache)
	PublicURL   string // base URL users reach the stream service at
	APIURL      string // base URL users reach the API at; empty = whatever address a request came to
	SigningKey  []byte // secret used to sign download links

	APIAddr    string
	StreamAddr string

	QbitURL  string
	QbitUser string
	QbitPass string

	LinkTTL          time.Duration // how long a download link keeps working
	MaxActivePerUser int           // downloads one user can have running at once (0 = no limit)
	MaxTorrentBytes  int64         // biggest item a user may download, unless the owner set their own limit (0 = no limit)
	MetadataTimeout  time.Duration // give up if no peer sends the file list in time
	PollInterval     time.Duration // how often ingest checks its fetcher
	Fetchers         []string      // which fetchers this ingest container runs

	EvictInterval     time.Duration // how often the scheduler checks disk space
	EvictIdleFor      time.Duration // never evict anything used more recently than this (protects what someone is watching)
	CacheMaxBytes     int64         // evict when the cache grows past this (0 = no limit)
	CacheMinFreeBytes int64         // evict when free disk space drops below this (0 = no limit)
}

// Load reads the environment and reports every bad value at once.
func Load() (Config, error) {
	var p parser
	c := Config{
		DatabaseURL: p.str("DATABASE_URL", "postgres://torza:torza@postgres:5432/torza?sslmode=disable"),
		DataDir:     p.str("DATA_DIR", "/data"),
		PublicURL:   strings.TrimRight(p.str("PUBLIC_URL", "http://localhost:8081"), "/"),
		APIURL:      strings.TrimRight(p.str("API_PUBLIC_URL", ""), "/"),
		APIAddr:     p.str("API_ADDR", ":8080"),
		StreamAddr:  p.str("STREAM_ADDR", ":8081"),
		QbitURL:     p.str("QBIT_URL", "http://qbittorrent:8080"),
		QbitUser:    p.str("QBIT_USER", "admin"),
		QbitPass:    p.str("QBIT_PASS", ""),

		LinkTTL:          p.duration("LINK_TTL", 6*time.Hour),
		MaxActivePerUser: p.integer("MAX_ACTIVE_PER_USER", 3),
		MaxTorrentBytes:  p.bytes("MAX_TORRENT_BYTES", 100e9),
		MetadataTimeout:  p.duration("METADATA_TIMEOUT", 2*time.Hour),
		PollInterval:     p.duration("POLL_INTERVAL", 5*time.Second),
		Fetchers:         strings.Split(p.str("INGEST_FETCHERS", "qbittorrent"), ","),

		EvictInterval:     p.duration("EVICT_INTERVAL", 10*time.Minute),
		EvictIdleFor:      p.duration("EVICT_IDLE_FOR", 6*time.Hour),
		CacheMaxBytes:     p.bytes("CACHE_MAX_BYTES", 0),
		CacheMinFreeBytes: p.bytes("CACHE_MIN_FREE_BYTES", 20e9),
	}
	if key := os.Getenv("SIGNING_KEY"); key != "" {
		b, err := hex.DecodeString(key)
		if err != nil {
			p.errs = append(p.errs, fmt.Errorf("SIGNING_KEY: must be hex: %w", err))
		}
		c.SigningKey = b
	}
	return c, errors.Join(p.errs...)
}

// RequireSigningKey is called by the services that create or check links.
func (c Config) RequireSigningKey() error {
	if len(c.SigningKey) < 32 {
		return errors.New("SIGNING_KEY must be at least 32 bytes of hex; generate one with: openssl rand -hex 32")
	}
	return nil
}

type parser struct{ errs []error }

func (p *parser) str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not a duration like 30s, 10m or 6h", key, v))
		return def
	}
	return d
}

func (p *parser) integer(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		p.errs = append(p.errs, fmt.Errorf("%s: %q is not a whole number", key, v))
		return def
	}
	return n
}

func (p *parser) bytes(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := ParseBytes(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

// ParseBytes reads sizes like "500GB", "1.5TB", "512MiB" or a plain number of
// bytes. KB/MB/GB/TB are powers of 1000 (how disks are sold); KiB/MiB/GiB/TiB
// are powers of 1024.
func ParseBytes(s string) (int64, error) {
	v := strings.ToUpper(strings.TrimSpace(s))
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"K", 1e3}, {"B", 1},
	}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			mult = u.mult
			v = strings.TrimSpace(strings.TrimSuffix(v, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a size like 500GB or 2TB", s)
	}
	return int64(n * mult), nil
}
