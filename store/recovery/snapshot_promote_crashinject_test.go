//go:build gograph_crashinject

package recovery

// This durability proof drives the crashinject-helper to SIGKILL itself
// at the recovery snapshot-promotion breakpoint, so it is compiled only
// under the gograph_crashinject build tag. Without the tag the helper
// embeds the production no-op crashpoint.Breakpoint and never crashes,
// which would make the SIGKILL assertion below fail. Run the crash
// battery with: go test -tags gograph_crashinject ./store/recovery/...

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// TestRecoveryPromoteCrash_PostRenamePreFsync is the regression gate for
// finding A1-F4 (the second-crash-during-recovery durability property).
//
// The helper builds the interrupted-publish on-disk state — a stranded
// snapshot.bak with the live snapshot name absent and the WAL prefix
// already truncated by a checkpoint, so the snapshot is the only durable
// copy of the seed workload — then runs recovery. Recovery promotes the
// backup (snapshot.bak -> snapshot) and the breakpoint SIGKILLs the
// process AFTER that rename but BEFORE the parent-dir fsync that makes it
// durable: precisely the window the fix instruments.
//
// Recovery from the resulting artefacts must reconstruct the FULL
// committed state — every checkpointed seed edge plus the WAL-only post
// edge — proving the promotion is idempotent and crash-safe across a
// second crash at this point. The breakpoint's existence (between the
// rename and the fsync) also pins the fix's ordering: the fsync provably
// follows the promotion rename.
func TestRecoveryPromoteCrash_PostRenamePreFsync(t *testing.T) {
	const scenario = "recovery.snapshot-promote-post-rename-pre-fsync"
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s\nstdout: %s\nstderr: %s",
			scenario, out.Stdout, out.Stderr)
	}

	// The promotion rename completed before the crash, so the live
	// snapshot name is present (the backup was renamed onto it).
	if _, err := os.Stat(filepath.Join(out.Dir, "snapshot", "manifest.json")); err != nil {
		t.Fatalf("promoted snapshot manifest missing after %s: %v", scenario, err)
	}

	// Re-run recovery over the post-crash artefacts: the full state must
	// survive — the checkpointed seed workload (snapshot) plus the
	// WAL-only post edge replayed on top.
	res := recoverInt64(t, out.Dir)
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true (promoted snapshot must load)")
	}
	assertCrashStateFull(t, res.Graph)
	// The WAL-only post edge committed after the checkpoint must replay.
	if !res.Graph.AdjList().HasEdge(3, 4) {
		t.Error("WAL-only post edge 3->4 lost across promotion crash + recovery")
	}
}
