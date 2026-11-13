package scheduler

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"torza/internal/db"
)

func torrents(sizes ...int64) []db.Item {
	out := make([]db.Item, len(sizes))
	for i, s := range sizes {
		out[i] = db.Item{ID: int64(i + 1), SizeBytes: s}
	}
	return out
}

func hashes(ts []db.Item) []int64 {
	var out []int64
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestPlan(t *testing.T) {
	cands := torrents(40, 40, 40)
	cases := []struct {
		name        string
		cache, free int64
		cfg         Config
		want        []int64
	}{
		{"within limits", 100, 1000, Config{MaxBytes: 200, MinFreeBytes: 100}, nil},
		{"cache too big", 120, 1000, Config{MaxBytes: 100}, []int64{1}},
		{"disk too full", 120, 10, Config{MinFreeBytes: 90}, []int64{1, 2}},
		{"no limits set", 1e12, 0, Config{}, nil},
		{"not enough to evict", 1000, 1000, Config{MaxBytes: 10}, []int64{1, 2, 3}},
	}
	for _, c := range cases {
		got := hashes(Plan(cands, c.cache, c.free, c.cfg))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: evicted %v, want %v", c.name, got, c.want)
		}
	}
}

type fakeRepo struct {
	cache      int64
	candidates []db.Item
	idleSince  time.Time
	evicted    []int64
}

func (r *fakeRepo) UsedBytes(context.Context) (int64, error)            { return r.cache, nil }
func (r *fakeRepo) BlockedUnheld(context.Context) ([]int64, error)      { return nil, nil }
func (r *fakeRepo) MarkPurged(context.Context, int64) error             { return nil }
func (r *fakeRepo) KeepFolders(context.Context) (map[int64]bool, error) { return nil, nil }
func (r *fakeRepo) EvictionCandidates(_ context.Context, idleSince time.Time, _ int) ([]db.Item, error) {
	r.idleSince = idleSince
	return r.candidates, nil
}
func (r *fakeRepo) MarkEvicted(_ context.Context, ih int64) error {
	r.evicted = append(r.evicted, ih)
	return nil
}

type fakeDisk struct {
	free    int64
	removed []int64
}

func (d *fakeDisk) FreeBytes() (int64, error) { return d.free, nil }
func (d *fakeDisk) Remove(id int64) error     { d.removed = append(d.removed, id); return nil }
func (d *fakeDisk) Sweep(map[int64]bool, time.Duration, time.Time) ([]string, error) {
	return nil, nil
}

func TestRunOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repo := &fakeRepo{cache: 120, candidates: torrents(40, 40, 40)}
	disk := &fakeDisk{free: 1000}
	e := New(repo, disk, Config{MaxBytes: 50, IdleFor: 15 * time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = func() time.Time { return now }

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2}; !slices.Equal(repo.evicted, want) || !slices.Equal(disk.removed, want) {
		t.Fatalf("evicted %v, removed %v, want %v", repo.evicted, disk.removed, want)
	}
	if !repo.idleSince.Equal(now.Add(-15 * time.Minute)) {
		t.Fatalf("asked for candidates idle since %v", repo.idleSince)
	}

	// Within limits: nothing is touched.
	repo2 := &fakeRepo{cache: 10, candidates: torrents(40)}
	e2 := New(repo2, &fakeDisk{free: 1000}, Config{MaxBytes: 50}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e2.RunOnce(context.Background()); err != nil || len(repo2.evicted) != 0 {
		t.Fatalf("evicted %v within limits (err %v)", repo2.evicted, err)
	}
}
