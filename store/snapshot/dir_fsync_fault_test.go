package snapshot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// This file drives the two directory-fsync failure branches whose entry points
// bind the OS backend directly ([WriteSnapshotCSRCtx] and [WriteIndexes]), and
// which therefore cannot be reached from the deterministic simulator however the
// in-memory disk is faulted:
//
//	writer.go  writeSnapshotCSRCtxWith  staging-directory fsync fails
//	indexes.go writeIndexesWith         indexes/ directory fsync fails
//
// Both were reported unreachable by audit finding F3 (rmp #2537); the other two
// sites it names — writeCaptureCore and writeCapturedIndexes — are driven
// through a real checkpoint on the simulated disk in internal/sim.
//
// Each oracle carries an UNCONDITIONAL verdict gate, a SEPARATE shape-only
// non-vacuity gate reading the injector's own fire count, and a CONTROL arm that
// runs the same call with the fault disarmed — because "the directory is absent"
// and "the trace is empty" are also what a call that did nothing at all would
// leave behind.

// errInjectedDirSyncFault is the failure the injector returns. Both branches
// wrap it with %w, so errors.Is on the returned error is proof of provenance:
// the error came from the directory fsync under test and not from some earlier
// step of the publish.
var errInjectedDirSyncFault = errors.New("snapshot test: injected directory fsync fault")

// dirSyncFaultFS is a fault-injecting filesystem backend: every operation is the
// production one ([osBackend]) except a DirSync of the armed path, which fails
// ONCE and then disarms. The one-shot, path-keyed semantics deliberately mirror
// SimDisk.ArmDirSyncFaultForPath so the two harnesses model the same event; fires
// is the reachability observable.
type dirSyncFaultFS struct {
	osBackend
	armed string
	fires int
}

// arm keys the injector on path, or disarms it when path is empty.
func (f *dirSyncFaultFS) arm(path string) { f.armed = path }

// DirSync fails once for the armed directory, then behaves as the OS backend.
func (f *dirSyncFaultFS) DirSync(path string) error {
	if f.armed != "" && path == f.armed {
		f.armed = ""
		f.fires++
		return errInjectedDirSyncFault
	}
	return f.osBackend.DirSync(path)
}

// buildWiderCSR returns a 4-edge CSR over 5 nodes — deliberately different from
// [buildTinyCSR], so a publish that wrongly went ahead would change the live
// snapshot's bytes and the unchanged-live-snapshot verdict can fail.
func buildWiderCSR(tb testing.TB) *csr.CSR[struct{}] {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			tb.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestWriteSnapshotCSR_StagingDirFsyncFault_AbortsBeforePublish drives
// writeSnapshotCSRCtxWith's staging-directory fsync branch — the last durability
// gate before the archive and publish renames of the legacy CSR publish path. It
// asserts the abort really aborts: the staging tree is removed, the previously
// published snapshot is untouched byte for byte, and the publish protocol never
// took its first step.
func TestWriteSnapshotCSR_StagingDirFsyncFault_AbortsBeforePublish(t *testing.T) {
	// Not parallel: installs the process-global publish-trace recorder.
	dir := filepath.Join(t.TempDir(), "snap")

	// A live snapshot to protect. Published through the production entry point,
	// before the recorder is installed, so its steps are not in the trace.
	if err := WriteSnapshotCSR(dir, buildTinyCSR(t)); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	liveManifest, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read live manifest: %v", err)
	}

	steps := installPublishTrace(t, dir)
	fsys := &dirSyncFaultFS{}
	fsys.arm(dir + ".tmp")

	perr := writeSnapshotCSRCtxWith(context.Background(), fsys, dir, buildWiderCSR(t))

	// Verdict 1: the publish fails, and it fails AT the staging fsync.
	if !errors.Is(perr, errInjectedDirSyncFault) {
		t.Fatalf("publish err = %v; want it to wrap the injected staging-dir fsync fault", perr)
	}
	// Verdict 2: the staging tree is cleaned up.
	if _, serr := os.Stat(dir + ".tmp"); !os.IsNotExist(serr) {
		t.Fatalf("staging directory survived the abort: stat err = %v; want IsNotExist (the branch must RemoveAll it)", serr)
	}
	// Verdict 3: nothing was published — the live snapshot is byte-identical, and
	// no stale backup was left behind.
	after, rerr := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // test-controlled path
	if rerr != nil {
		t.Fatalf("live snapshot damaged by the aborted publish: %v", rerr)
	}
	if !bytes.Equal(after, liveManifest) {
		t.Fatalf("the live snapshot changed although the staging fsync failed: an unsynced staging tree was published")
	}
	if _, serr := os.Stat(dir + ".bak"); !os.IsNotExist(serr) {
		t.Fatalf("archive backup exists: stat err = %v; want IsNotExist (the abort must precede the archive rename)", serr)
	}
	// Verdict 4: the publish protocol never took a step. staging-fsync is noted
	// only after the fsync returns nil, and archive/rename follow it.
	if len(*steps) != 0 {
		t.Fatalf("publish trace = %v; want no steps at all — the abort must precede the archive and publish renames", *steps)
	}
	// Non-vacuity (shape only): the injected fault fired exactly once.
	if fsys.fires != 1 {
		t.Fatalf("injected dir-fsync fires = %d, want 1: the branch under test never executed", fsys.fires)
	}
	// Control: the SAME call with the fault disarmed publishes and records the
	// canonical ordering, so the empty trace above is a decision and not a broken
	// recorder.
	if cerr := writeSnapshotCSRCtxWith(context.Background(), fsys, dir, buildWiderCSR(t)); cerr != nil {
		t.Fatalf("control publish: %v", cerr)
	}
	assertStagingFsyncBeforeRename(t, *steps)
	t.Logf("witness: faulted publish left 0 trace steps and an unchanged live manifest (%d bytes); the disarmed control recorded %v", len(liveManifest), *steps)
}

// TestWriteIndexes_IndexesDirFsyncFault drives writeIndexesWith's indexes/
// directory fsync branch: the per-file fsyncs have all succeeded, the directory
// fsync that makes their names durable fails, and the writer must remove the
// half-durable indexes/ directory rather than report entries whose dirents may
// not survive a crash.
func TestWriteIndexes_IndexesDirFsyncFault(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	bt := btree.New[string]()
	if err := mgr.CreateIndex("btree.age", bt); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	for i := 0; i < 8; i++ {
		bt.Insert(string(rune('a'+i)), 0)
	}

	dir := filepath.Join(t.TempDir(), "snap")
	idxDir := filepath.Join(dir, IndexesDir)
	fsys := &dirSyncFaultFS{}
	fsys.arm(idxDir)

	entries, werr := writeIndexesWith(fsys, dir, mgr)

	// Verdict 1: the write fails, and it fails AT the indexes/ fsync.
	if !errors.Is(werr, errInjectedDirSyncFault) {
		t.Fatalf("writeIndexes err = %v; want it to wrap the injected indexes-dir fsync fault", werr)
	}
	// Verdict 2: no manifest entries are handed back for index files whose
	// dirents were never made durable.
	if len(entries) != 0 {
		t.Fatalf("writeIndexes returned %d entries on the fsync-failure path; want none", len(entries))
	}
	// Verdict 3: the half-durable directory is removed.
	if _, serr := os.Stat(idxDir); !os.IsNotExist(serr) {
		t.Fatalf("indexes directory survived the failure: stat err = %v; want IsNotExist (the branch must RemoveAll it)", serr)
	}
	// Non-vacuity (shape only): the injected fault fired exactly once.
	if fsys.fires != 1 {
		t.Fatalf("injected dir-fsync fires = %d, want 1: the branch under test never executed", fsys.fires)
	}
	// Control: the SAME call with the fault disarmed writes the index and keeps
	// the directory, so the absent directory above is a decision the failure path
	// took and not simply a call that never writes anything.
	ctrl, cerr := writeIndexesWith(fsys, dir, mgr)
	if cerr != nil {
		t.Fatalf("control writeIndexes: %v", cerr)
	}
	if len(ctrl) != 1 {
		t.Fatalf("control returned %d entries, want 1", len(ctrl))
	}
	files, derr := os.ReadDir(idxDir)
	if derr != nil || len(files) != 1 {
		t.Fatalf("control left %d files in %s (err %v), want 1", len(files), idxDir, derr)
	}
	t.Logf("witness: faulted write removed %s and returned 0 entries; the disarmed control left %d file(s) and 1 entry", idxDir, len(files))
}
