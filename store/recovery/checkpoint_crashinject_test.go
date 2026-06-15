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

// crashPostEdge mirrors the extra edge runCheckpointPrefixCrash commits after
// the first clean checkpoint, so the WAL carries a real suffix when the second
// checkpoint's prefix-truncate crashes. The parent asserts it survives.
var crashPostEdge = struct{ src, dst, weight int64 }{3, 4, 400}

// TestCheckpointCrash_P2SnapshotPublishedPreTruncate is the durability proof
// for the non-blocking checkpoint's checkpoint.p2-snapshot-published-pre-truncate
// breakpoint. A child commits an int64-keyed workload and triggers a
// non-blocking codec-aware checkpoint; the breakpoint SIGKILLs it AFTER the
// self-sufficient snapshot is published and durable but BEFORE the WAL prefix
// is truncated.
//
// Recovery from the resulting artefacts (durable snapshot + still-intact full
// WAL) must reconstruct the full committed state. The whole WAL is replayed on
// top of the snapshot; replay is idempotent, so the final graph is identical to
// the pre-crash state — Durability holds at this crash point.
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
	// The WAL was NOT truncated, so it is replayed on top of the snapshot.
	if res.WALOps == 0 {
		t.Error("WALOps = 0; the intact WAL should have replayed on top of the snapshot")
	}
	assertCrashStateFull(t, res.Graph)
}

// TestCheckpointCrash_TruncatePrefixInterleavings is the durability proof for
// the three crash points inside wal.Writer.TruncatePrefix's atomic
// copy-then-rename. The child commits the seed, runs one complete checkpoint
// (folding the seed into a self-sufficient snapshot and prefix-truncating the
// WAL), commits one more "post" edge so the WAL carries a real suffix, then
// triggers a SECOND checkpoint whose prefix-truncate SIGKILLs the child at the
// named point:
//
//   - tmp-written-pre-rename:   the suffix-only temp file is durable but the
//     atomic rename has NOT happened — the ORIGINAL full WAL is intact.
//   - post-rename-pre-dirfsync: the rename is done but its dirent may not be
//     durable — recovery tolerates either the old full or new suffix-only WAL.
//   - post-rename-pre-bookkeeping: the suffix-only WAL is durable.
//
// At every point recovery must reconstruct the FULL committed state — seed
// (from the snapshot) plus the post edge (from whichever WAL survives) — losing
// nothing. This is the crux: a non-blocking checkpoint must never lose a
// transaction committed during the lock-free snapshot write.
func TestCheckpointCrash_TruncatePrefixInterleavings(t *testing.T) {
	scenarios := []string{
		"checkpoint.truncprefix.tmp-written-pre-rename",
		"checkpoint.truncprefix.post-rename-pre-dirfsync",
		"checkpoint.truncprefix.post-rename-pre-bookkeeping",
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
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
			res := recoverInt64(t, out.Dir)
			if !res.SnapshotHit {
				t.Fatalf("SnapshotHit = false, want true at %s", scenario)
			}
			// Seed (from the snapshot) plus the post edge (from the surviving
			// WAL) must both be present — no committed transaction lost.
			assertCrashStateFull(t, res.Graph)
			if !res.Graph.AdjList().HasEdge(crashPostEdge.src, crashPostEdge.dst) {
				t.Errorf("post edge %d->%d lost across %s", crashPostEdge.src, crashPostEdge.dst, scenario)
			}
		})
	}
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
