//go:build gograph_crashinject

package recovery

// These durability proofs drive the crashinject-helper to SIGKILL itself
// at a checkpoint breakpoint, so they are compiled only under the
// gograph_crashinject build tag. Without the tag the helper embeds the
// production no-op crashpoint.Breakpoint and never crashes, which would
// make the SIGKILL assertions below fail. Run the crash battery with:
// go test -tags gograph_crashinject ./store/recovery/...

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
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

// crashPostEdge mirrors checkpointPostEdge, the edge the segmented-checkpoint
// scenarios commit after the seed. The parent asserts it survives.
var crashPostEdge = struct{ src, dst, weight int64 }{3, 4, 400}

// TestCheckpointCrash_P2SnapshotPublishedPreTruncate is the durability proof
// for the non-blocking checkpoint's checkpoint.p2-snapshot-published-pre-truncate
// breakpoint. A child commits an int64-keyed workload and triggers a
// non-blocking codec-aware checkpoint; the breakpoint SIGKILLs it AFTER the
// self-sufficient snapshot is published and recorded in the WAL control file
// but BEFORE any segment is unlinked.
//
// Recovery from the resulting artefacts must reconstruct the full committed
// state from the snapshot, re-applying no frame below its redo position —
// Durability holds at this crash point.
func TestCheckpointCrash_P2SnapshotPublishedPreTruncate(t *testing.T) {
	const scenario = "checkpoint.p2-snapshot-published-pre-truncate"
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
	// The control file already records the snapshot's redo position, so every
	// frame below it is folded into the snapshot and none is re-applied.
	if res.WALOps != 0 {
		t.Errorf("WALOps = %d; every frame lies below the recorded redo position and must not be re-applied", res.WALOps)
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
