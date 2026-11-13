// Package qbitsource fills items from torrents through qBittorrent.
package qbitsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"torza/internal/db"
	"torza/internal/qbit"
	"torza/internal/source"
)

// Engine is the part of qBittorrent the source uses. *qbit.Client implements it.
type Engine interface {
	Add(ctx context.Context, magnetURI, savePath string) error
	Info(ctx context.Context, hashes []string) ([]qbit.Torrent, error)
	Files(ctx context.Context, hash string) ([]qbit.File, error)
	Delete(ctx context.Context, hashes []string, deleteFiles bool) error
}

// handle is what the source stores on an item.
type handle struct {
	Hash string `json:"hash"`
	Dir  string `json:"dir"` // save folder, relative to the data directory
}

type Source struct {
	engine  Engine
	dataDir string
}

// New makes the source. dataDir is the data directory that handles' folders
// are relative to.
func New(engine Engine, dataDir string) *Source {
	return &Source{engine: engine, dataDir: dataDir}
}

func (s *Source) Name() string { return "qbittorrent" }

func (s *Source) Start(ctx context.Context, job source.Job) ([]byte, error) {
	rel, err := filepath.Rel(s.dataDir, job.Dir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("download folder %s is outside the data directory", job.Dir)
	}
	h := handle{Hash: job.Key.Value, Dir: filepath.ToSlash(rel)}
	if err := os.MkdirAll(job.Dir, 0o755); err != nil {
		return nil, err
	}
	if err := s.add(ctx, job.Payload, h); err != nil {
		return nil, err
	}
	return json.Marshal(h)
}

// add hands the torrent to qBittorrent. qBittorrent refuses torrents it
// already has, which is fine: a restart between adding and storing the
// handle lands here.
func (s *Source) add(ctx context.Context, magnetURI string, h handle) error {
	err := s.engine.Add(ctx, magnetURI, s.abs(h))
	if err == nil {
		return nil
	}
	if infos, ierr := s.engine.Info(ctx, []string{h.Hash}); ierr == nil && len(infos) > 0 {
		return nil
	}
	return err
}

func (s *Source) Poll(ctx context.Context, job source.Job) (source.Report, error) {
	h, err := parse(job.Handle)
	if err != nil {
		return source.Report{}, err
	}
	infos, err := s.engine.Info(ctx, []string{h.Hash})
	if err != nil {
		return source.Report{}, err
	}
	if len(infos) == 0 {
		// qBittorrent lost it (its state was reset). Hand it over again; it
		// checks the data already on disk and only fetches what is missing.
		if err := s.add(ctx, job.Payload, h); err != nil {
			return source.Report{}, fmt.Errorf("re-add lost torrent: %w", err)
		}
		return source.Report{Phase: "starting"}, nil
	}
	in := infos[0]
	r := source.Report{Name: in.Name, BytesTotal: in.TotalSize, BytesDone: in.TotalSize - in.AmountLeft}
	switch {
	case in.State == "error" || in.State == "missingFiles":
		return r, source.Fail(source.Permanent, "the torrent client reported: "+in.State)
	case in.TotalSize <= 0:
		r.Phase, r.BytesDone = "metadata", 0
	case strings.HasPrefix(in.State, "checking"):
		r.Phase = "verifying"
	case in.Progress >= 1 && !busy(in.State):
		r.Phase, r.Done = "downloading", true
	default:
		r.Phase = "downloading"
	}
	return r, nil
}

// busy reports qBittorrent states where the files may still change even at
// 100% progress.
func busy(state string) bool {
	return strings.HasPrefix(state, "checking") || state == "moving" || state == "metaDL" || state == "allocating"
}

func (s *Source) Finalize(ctx context.Context, job source.Job) (source.Manifest, error) {
	h, err := parse(job.Handle)
	if err != nil {
		return source.Manifest{}, err
	}
	infos, err := s.engine.Info(ctx, []string{h.Hash})
	if err != nil {
		return source.Manifest{}, err
	}
	name := ""
	if len(infos) > 0 {
		name = infos[0].Name
	}
	files, err := s.engine.Files(ctx, h.Hash)
	if err != nil {
		return source.Manifest{}, err
	}
	if len(files) == 0 {
		return source.Manifest{}, errors.New("finished torrent reports no files")
	}
	m := source.Manifest{Root: s.abs(h), Name: name}
	for _, f := range files {
		m.Files = append(m.Files, manifestFile(f.Name, f.Size))
	}
	return m, nil
}

func (s *Source) Root(job source.Job) (string, error) {
	h, err := parse(job.Handle)
	if err != nil {
		return "", err
	}
	return s.abs(h), nil
}

func (s *Source) Release(ctx context.Context, job source.Job) error {
	h, err := parse(job.Handle)
	if err != nil {
		return err
	}
	// Removing the torrent but keeping its files stops seeding and lets
	// the files be moved. Removing an unknown hash is not an error.
	return s.engine.Delete(ctx, []string{h.Hash}, false)
}

func (s *Source) Cancel(ctx context.Context, job source.Job, deleteData bool) error {
	if job.Handle == nil {
		return s.engine.Delete(ctx, []string{job.Key.Value}, deleteData)
	}
	h, err := parse(job.Handle)
	if err != nil {
		return err
	}
	err = s.engine.Delete(ctx, []string{h.Hash}, deleteData)
	if deleteData {
		err = errors.Join(err, os.RemoveAll(s.abs(h)))
	}
	return err
}

func (s *Source) abs(h handle) string { return filepath.Join(s.dataDir, filepath.FromSlash(h.Dir)) }

func parse(b []byte) (handle, error) {
	var h handle
	if err := json.Unmarshal(b, &h); err != nil || h.Hash == "" || !filepath.IsLocal(filepath.FromSlash(h.Dir)) {
		return handle{}, source.Fail(source.Permanent, "stored download state is unreadable")
	}
	return h, nil
}

func manifestFile(path string, size int64) db.ManifestFile {
	return db.ManifestFile{Path: path, SizeBytes: size}
}
