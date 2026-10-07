//go:build gograph_crashinject

package recovery

// walv2_crashinject_test.go — the crash points of the segmented WAL
// (docs/design-wal-v2.md §8): store creation, legacy migration, the
// checkpoint's control-file write and segment unlink, and the legacy stub. Each
// drives the crashinject-helper to SIGKILL itself at the named breakpoint and
// recovers the artefacts it leaves. Compiled only under the gograph_crashinject
// build tag; see checkpoint_crashinject_test.go.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// segFillerNodes mirrors the helper's filler count: nodes 100..139 each carry a
// 64 KiB "fill" property.
const segFillerNodes = 40

// runKilled runs scenario and asserts the child was SIGKILL'd at it.
func runKilled(t *testing.T, scenario string) string {
	t.Helper()
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s (exit %d)\nstdout: %s\nstderr: %s",
			scenario, out.ExitCode, out.Stdout, out.Stderr)
	}
	return out.Dir
}

// assertSeedPlusPost asserts the seed workload and the post edge survived.
func assertSeedPlusPost(t *testing.T, g *lpg.Graph[int64, int64], scenario string) {
	t.Helper()
	assertCrashStateFull(t, g)
	if !g.AdjList().HasEdge(crashPostEdge.src, crashPostEdge.dst) {
		t.Errorf("post edge %d->%d lost across %s", crashPostEdge.src, crashPostEdge.dst, scenario)
	}
}

// TestCheckpointCrash_SegmentReclaimInterleavings crashes a checkpoint of a
// three-segment log at every point of its control-file write and segment
// unlink. Whatever control file and whichever segments survive, recovery must
// return the whole committed state: seed, filler and post edge.
func TestCheckpointCrash_SegmentReclaimInterleavings(t *testing.T) {
	for _, scenario := range []string{
		"checkpoint.control-tmp-pre-rename",
		"checkpoint.control-renamed-pre-dirfsync",
		"checkpoint.unlink-partial",
		"checkpoint.unlink-done-pre-dirfsync",
	} {
		t.Run(scenario, func(t *testing.T) {
			dir := runKilled(t, scenario)
			res := recoverInt64(t, dir)
			if !res.SnapshotHit || !res.IsClean() {
				t.Fatalf("SnapshotHit=%t clean=%t (TailErr %v) at %s", res.SnapshotHit, res.IsClean(), res.TailErr, scenario)
			}
			assertSeedPlusPost(t, res.Graph, scenario)
			for i := int64(0); i < segFillerNodes; i++ {
				v, ok := res.Graph.GetNodeProperty(100+i, "fill")
				if s, _ := v.String(); !ok || len(s) != 64<<10 {
					t.Fatalf("filler node %d lost its 64 KiB property across %s", 100+i, scenario)
				}
			}
			// The unlink points ran after the control write: segment 1 lies wholly
			// below the oldest retained position and is either gone or ignored.
			if scenario == "checkpoint.unlink-done-pre-dirfsync" {
				if _, err := os.Stat(wal.SegmentPath(filepath.Join(dir, "wal"), 1)); err == nil {
					t.Errorf("segment 1 still exists after every unlink at %s", scenario)
				}
			}
		})
	}
}

// TestCheckpointCrash_LegacyStubReplacement crashes a checkpoint of a migrated
// legacy store after it renamed the seal stub over the legacy file and before
// the directory fsync. The snapshot records a redo position, so the legacy
// history is folded into it whichever version of the file survives.
func TestCheckpointCrash_LegacyStubReplacement(t *testing.T) {
	const scenario = "checkpoint.legacy-stub-renamed-pre-dirfsync"
	dir := runKilled(t, scenario)
	res := recoverInt64(t, dir)
	if !res.IsClean() {
		t.Fatalf("recovery not clean at %s: %v", scenario, res.TailErr)
	}
	assertSeedPlusPost(t, res.Graph, scenario)
}

// TestWALCrash_StoreCreation crashes the first open of an empty directory while
// it creates the control file or the first segment. Recovery opens an empty,
// clean store, and a later open completes the layout and accepts commits.
func TestWALCrash_StoreCreation(t *testing.T) {
	for _, scenario := range []string{
		"wal.control.tmp-written-pre-rename",
		"wal.control.renamed-pre-dirfsync",
		"wal.segment.spare-created-pre-dirfsync",
	} {
		t.Run(scenario, func(t *testing.T) {
			dir := runKilled(t, scenario)
			res := recoverInt64(t, dir)
			if !res.IsClean() || res.Graph.LiveOrder() != 0 {
				t.Fatalf("recovery after %s: clean=%t live=%d (TailErr %v)", scenario, res.IsClean(), res.Graph.LiveOrder(), res.TailErr)
			}
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatalf("reopen after %s: %v", scenario, err)
			}
			st := res.NewStore(w, txn.Options[int64, int64]{Codec: txn.NewInt64Codec(), WeightCodec: txn.NewInt64WeightCodec()})
			tx := st.Begin()
			if err := tx.AddEdge(7, 8, 9); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := recoverInt64(t, dir); !got.Graph.AdjList().HasEdge(7, 8) {
				t.Fatalf("the commit after the completed creation was lost (%s)", scenario)
			}
		})
	}
}

// TestWALCrash_Migration crashes the migration of a legacy store holding the
// seed. Recovery replays the legacy history; a later open completes the
// migration (the legacy file ends with its seal) and accepts commits.
func TestWALCrash_Migration(t *testing.T) {
	for _, scenario := range []string{
		"wal.migrate.control-written-pre-seal",
		"wal.migrate.sealed-pre-first-v2-frame",
	} {
		t.Run(scenario, func(t *testing.T) {
			dir := runKilled(t, scenario)
			res := recoverInt64(t, dir)
			if !res.IsClean() {
				t.Fatalf("recovery after %s not clean: %v", scenario, res.TailErr)
			}
			assertCrashStateFull(t, res.Graph)
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatalf("reopen after %s: %v", scenario, err)
			}
			st := res.NewStore(w, txn.Options[int64, int64]{Codec: txn.NewInt64Codec(), WeightCodec: txn.NewInt64WeightCodec()})
			tx := st.Begin()
			if err := tx.AddEdge(crashPostEdge.src, crashPostEdge.dst, crashPostEdge.weight); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			legacy, err := os.ReadFile(filepath.Join(dir, "wal")) //nolint:gosec // G304: path under the crash harness directory
			if err != nil {
				t.Fatal(err)
			}
			log, err := wal.OpenLog(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			ctl, _ := log.Control()
			seal := wal.EncodeLegacySeal(wal.LegacySeal{StoreID: ctl.StoreID})
			if !bytes.HasSuffix(legacy, seal) {
				t.Fatalf("the legacy log does not end with its seal after the migration completed (%s)", scenario)
			}
			assertSeedPlusPost(t, recoverInt64(t, dir).Graph, scenario)
		})
	}
}
