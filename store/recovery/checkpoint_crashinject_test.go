package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/lpg"
	"gograph/internal/crashinject"
	"gograph/store/txn"
)

// crashSeedEdges mirrors the deterministic int64 workload the
// crashinject-helper commits in runCheckpointCrash. The parent uses it
// to assert that no committed edge is lost across the injected crash.
var crashSeedEdges = []struct {
	src, dst int64
	weight   int64
}{
	{1, 2, 100},
	{2, 3, 200},
	{3, 1, 300},
}

// TestCheckpointCrash_PostSnapshotPreTruncate is the AC#4 durability
// proof for the checkpoint.post-snapshot-pre-truncate breakpoint. A
// child process commits an int64-keyed workload and triggers a
// codec-aware checkpoint; the breakpoint SIGKILLs it AFTER the
// self-sufficient snapshot is durable but BEFORE the WAL is truncated.
//
// Recovery from the resulting artefacts (durable snapshot + still-intact
// WAL) must reconstruct the full committed state. The WAL is replayed on
// top of the snapshot; the apply is idempotent, so the final graph is
// identical to the pre-crash state — Durability holds at this crash point.
func TestCheckpointCrash_PostSnapshotPreTruncate(t *testing.T) {
	const scenario = "checkpoint.post-snapshot-pre-truncate"
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s\nstdout: %s\nstderr: %s",
			scenario, out.Stdout, out.Stderr)
	}

	// The snapshot must be durable on disk (the breakpoint fires after it
	// is published).
	if _, err := os.Stat(filepath.Join(out.Dir, "snapshot", "manifest.json")); err != nil {
		t.Fatalf("snapshot not durable after %s: %v", scenario, err)
	}

	res := recoverInt64(t, out.Dir)
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true (self-sufficient snapshot present)")
	}
	// The WAL was NOT truncated, so it is replayed on top of the snapshot.
	if res.WALOps == 0 {
		t.Error("WALOps = 0; the intact WAL should have replayed on top of the snapshot")
	}
	assertCrashStateFull(t, res.Graph)
}

// TestCheckpointCrash_MidTruncate is the AC#4 durability proof for the
// checkpoint.mid-truncate breakpoint. The child commits the same
// workload and triggers the codec-aware checkpoint; the breakpoint
// SIGKILLs it in the MIDDLE of the WAL truncation, after the file has
// been shrunk to zero on disk.
//
// Recovery must reconstruct the full committed state from the
// self-sufficient snapshot ALONE, because the WAL is now empty. This is
// the strongest F3 guarantee: the snapshot stands on its own at the
// exact instant the WAL prefix is discarded.
func TestCheckpointCrash_MidTruncate(t *testing.T) {
	const scenario = "checkpoint.mid-truncate"
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s\nstdout: %s\nstderr: %s",
			scenario, out.Stdout, out.Stderr)
	}

	if _, err := os.Stat(filepath.Join(out.Dir, "snapshot", "manifest.json")); err != nil {
		t.Fatalf("snapshot not durable after %s: %v", scenario, err)
	}

	// The WAL was truncated to zero on disk before the crash.
	walInfo, err := os.Stat(filepath.Join(out.Dir, "wal"))
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if walInfo.Size() != 0 {
		t.Fatalf("WAL size = %d after %s, want 0 (truncation reached disk before the crash)",
			walInfo.Size(), scenario)
	}

	res := recoverInt64(t, out.Dir)
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (state must come from the snapshot alone)", res.WALOps)
	}
	assertCrashStateFull(t, res.Graph)
}

// recoverInt64 opens the int64-keyed store rooted at dir with the same
// codecs the crashinject-helper used.
func recoverInt64(t *testing.T, dir string) Result[int64, int64] {
	t.Helper()
	res, err := Open[int64, int64](dir, Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	return res
}

// assertCrashStateFull asserts the recovered graph carries every edge
// (with weight), the label, and the property the crashinject-helper
// committed before the crash.
func assertCrashStateFull(t *testing.T, g *lpg.Graph[int64, int64]) {
	t.Helper()
	for _, e := range crashSeedEdges {
		if !g.AdjList().HasEdge(e.src, e.dst) {
			t.Errorf("edge %d->%d lost across crash+recovery", e.src, e.dst)
			continue
		}
		found := false
		for n, wt := range g.AdjList().Neighbours(e.src) {
			if n == e.dst {
				found = true
				if wt != e.weight {
					t.Errorf("edge %d->%d weight = %d, want %d", e.src, e.dst, wt, e.weight)
				}
			}
		}
		if !found {
			t.Errorf("edge %d->%d weight unreadable across crash+recovery", e.src, e.dst)
		}
	}
	if !g.HasNodeLabel(1, "Root") {
		t.Error("node label Root lost across crash+recovery")
	}
	if v, ok := g.GetNodeProperty(2, "weight"); !ok {
		t.Error("node property weight lost across crash+recovery")
	} else if got, _ := v.Int64(); got != 42 {
		t.Errorf("node property weight = %d, want 42", got)
	}
}
