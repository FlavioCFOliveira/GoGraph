package wal

// sync_latency_test.go — rmp #3022: the testing-only fsync latency.
//
// Layer: short.

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSyncLatency_SeedReproducesSequenceWithinBounds pins the two properties a
// failing run relies on to be replayed: one seed yields one sequence of delays,
// and every delay lies in [lo, hi].
func TestSyncLatency_SeedReproducesSequenceWithinBounds(t *testing.T) {
	t.Parallel()
	const lo, hi = time.Millisecond, 5 * time.Millisecond
	a, b, c := NewSyncLatency(42, lo, hi), NewSyncLatency(42, lo, hi), NewSyncLatency(43, lo, hi)
	differs := false
	for i := range 64 {
		da, db, dc := a.next(), b.next(), c.next()
		if da != db {
			t.Fatalf("draw %d: seed 42 gave %v and %v", i, da, db)
		}
		if da < lo || da > hi {
			t.Fatalf("draw %d: %v outside [%v, %v]", i, da, lo, hi)
		}
		differs = differs || da != dc
	}
	if !differs {
		t.Fatal("seeds 42 and 43 drew identical sequences: the seed is not used")
	}
}

// TestOpenWithSyncLatency_DelaysCommitAndDirectoryFsync pins that the latency
// reaches both seams: the commit-path data fsync and the directory fsync (here
// the prefix marker's), and that a nil latency installs nothing.
func TestOpenWithSyncLatency_DelaysCommitAndDirectoryFsync(t *testing.T) {
	t.Parallel()
	const d = 20 * time.Millisecond
	w, err := OpenWithSyncLatency(filepath.Join(t.TempDir(), "wal"), NewSyncLatency(1, d, d))
	if err != nil {
		t.Fatalf("OpenWithSyncLatency: %v", err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Append([]byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	start := time.Now()
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := time.Since(start); got < d {
		t.Errorf("Sync took %v, want at least the injected %v", got, d)
	}
	start = time.Now()
	if err := w.MarkPrefixTruncated(); err != nil {
		t.Fatalf("MarkPrefixTruncated: %v", err)
	}
	if got := time.Since(start); got < d {
		t.Errorf("the marker's directory fsync took %v, want at least the injected %v", got, d)
	}

	plain, err := OpenWithSyncLatency(filepath.Join(t.TempDir(), "wal"), nil)
	if err != nil {
		t.Fatalf("OpenWithSyncLatency(nil): %v", err)
	}
	defer func() { _ = plain.Close() }()
	if plain.syncLatency != nil {
		t.Error("a nil latency installed a delay")
	}
}
