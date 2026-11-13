package qbitsource

import (
	"context"
	"path/filepath"
	"testing"

	"torza/internal/db"
	"torza/internal/qbit"
	"torza/internal/source"
)

type fakeEngine struct {
	torrents map[string]qbit.Torrent
	files    []qbit.File
	added    []string
	deleted  []bool
}

func (e *fakeEngine) Add(_ context.Context, uri, savePath string) error {
	e.added = append(e.added, savePath)
	if _, ok := e.torrents["h"]; ok {
		return qbit.ErrAddRejected
	}
	e.torrents["h"] = qbit.Torrent{Hash: "h", State: "metaDL"}
	return nil
}

func (e *fakeEngine) Info(_ context.Context, hashes []string) ([]qbit.Torrent, error) {
	if t, ok := e.torrents[hashes[0]]; ok {
		return []qbit.Torrent{t}, nil
	}
	return nil, nil
}

func (e *fakeEngine) Files(context.Context, string) ([]qbit.File, error) { return e.files, nil }

func (e *fakeEngine) Delete(_ context.Context, hashes []string, deleteFiles bool) error {
	delete(e.torrents, hashes[0])
	e.deleted = append(e.deleted, deleteFiles)
	return nil
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	e := &fakeEngine{torrents: map[string]qbit.Torrent{}}
	s := New(e, data)
	job := source.Job{ItemID: 7, Key: db.Key{Kind: db.KindTorrent, Value: "h"}, Payload: "magnet:?xt=urn:btih:h", Dir: filepath.Join(data, "incoming", "7")}

	h, err := s.Start(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	// Starting again after a crash before the handle was stored is harmless.
	if _, err := s.Start(ctx, job); err != nil {
		t.Fatalf("second start: %v", err)
	}
	job.Handle = h
	if root, _ := s.Root(job); root != job.Dir {
		t.Fatalf("root %q, want %q", root, job.Dir)
	}

	r, err := s.Poll(ctx, job)
	if err != nil || r.Phase != "metadata" || r.Done {
		t.Fatalf("waiting for metadata: %+v %v", r, err)
	}
	e.torrents["h"] = qbit.Torrent{Hash: "h", Name: "Demo", State: "downloading", TotalSize: 100, AmountLeft: 40, Progress: .6}
	if r, _ = s.Poll(ctx, job); r.BytesDone != 60 || r.Done {
		t.Fatalf("downloading: %+v", r)
	}
	e.torrents["h"] = qbit.Torrent{Hash: "h", Name: "Demo", State: "checkingUP", TotalSize: 100, Progress: 1}
	if r, _ = s.Poll(ctx, job); r.Done {
		t.Fatal("done while qBittorrent is still checking")
	}
	e.torrents["h"] = qbit.Torrent{Hash: "h", Name: "Demo", State: "uploading", TotalSize: 100, Progress: 1}
	if r, _ = s.Poll(ctx, job); !r.Done {
		t.Fatal("not done at 100%")
	}

	e.files = []qbit.File{{Name: "Demo/a.mkv", Size: 100}}
	m, err := s.Finalize(ctx, job)
	if err != nil || m.Root != job.Dir || len(m.Files) != 1 || m.Files[0].Path != "Demo/a.mkv" {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if err := s.Release(ctx, job); err != nil || e.deleted[0] {
		t.Fatalf("release must keep the files: %v %v", err, e.deleted)
	}

	// qBittorrent losing the torrent: it's handed over again to the same folder.
	if _, err := s.Poll(ctx, job); err != nil || len(e.added) != 3 || e.added[2] != job.Dir {
		t.Fatalf("re-add: %v %v", err, e.added)
	}
	e.torrents["h"] = qbit.Torrent{Hash: "h", State: "error"}
	if _, err := s.Poll(ctx, job); source.ClassOf(err) != source.Permanent {
		t.Fatalf("error state: %v", err)
	}
	if err := s.Cancel(ctx, job, true); err != nil || !e.deleted[len(e.deleted)-1] {
		t.Fatalf("cancel: %v", err)
	}
}

func TestStartRefusesFoldersOutsideData(t *testing.T) {
	s := New(&fakeEngine{torrents: map[string]qbit.Torrent{}}, t.TempDir())
	if _, err := s.Start(context.Background(), source.Job{Key: db.Key{Value: "h"}, Dir: "/tmp/elsewhere"}); err == nil {
		t.Fatal("started outside the data directory")
	}
	if _, err := s.Poll(context.Background(), source.Job{Handle: []byte(`{"hash":"h","dir":"../x"}`)}); source.ClassOf(err) != source.Permanent {
		t.Fatalf("escaping handle: %v", err)
	}
}
