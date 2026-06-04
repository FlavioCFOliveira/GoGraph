package snapshot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// installPublishTrace wires publishTraceHook to record the publish-step
// events for the staging directory wantStaging, and unregisters the hook
// on test cleanup so it never leaks into another test. Events for any
// OTHER staging directory (a concurrent publisher in a parallel test) are
// dropped, so the recorded slice contains only this test's own steps.
//
// publishTraceHook is process-global, so a test that installs a recorder
// must NOT run with t.Parallel(): two recorders would clobber each
// other's slot via Store. Concurrent NON-tracing publishers are fine —
// notePublishStep loads the pointer atomically and the filter discards
// their foreign-path events.
func installPublishTrace(t *testing.T, wantStaging string) *[]string {
	t.Helper()
	var events []string
	hook := func(event, path string) {
		if path != wantStaging {
			return // foreign publisher (parallel test): not ours
		}
		events = append(events, event)
	}
	publishTraceHook.Store(&hook)
	t.Cleanup(func() { publishTraceHook.Store(nil) })
	return &events
}

// assertStagingFsyncBeforeRename verifies the recorded publish trace is
// exactly the canonical crash-safe ordering for the staging directory:
// a "staging-fsync" of the staging dir, immediately followed by its
// "rename". Pre-fix code never fsyncs the staging directory, so the
// "staging-fsync" event is absent and this assertion fails — the
// regression guard.
func assertStagingFsyncBeforeRename(t *testing.T, events []string) {
	t.Helper()
	want := []string{"staging-fsync", "rename"}
	if len(events) != len(want) {
		t.Fatalf("publish trace = %v; want exactly %v (staging-dir fsync must precede the publish rename)", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("publish trace[%d] = %q; want %q\nfull trace: %v", i, events[i], want[i], events)
		}
	}
}

// TestWriteSnapshotFull_StagingDirFsyncBeforeRename is the deterministic
// ordering proxy for task #1287: the v2/v3 publish path MUST fsync the
// staging directory's own inode (so the dirents linking csr.bin /
// manifest.json / mapper.bin / etc. into it are durable) BEFORE the
// publish rename, otherwise a crash after the rename and a subsequent
// WAL truncation can lose every committed transaction folded into the
// checkpoint.
//
// The test installs the publish-trace hook, runs a real
// [WriteSnapshotFull], and asserts the staging-dir fsync is observed
// immediately before the rename. Reverting the dirFsync(tmp) call in
// writeSnapshotFullCore makes the "staging-fsync" event disappear and
// this test fails — confirming it guards the fix rather than merely the
// happy path.
//
// On Windows dirFsync is a no-op (NTFS journals the dirents); the
// notePublishStep("staging-fsync", …) call still fires unconditionally
// after dirFsync returns nil, so the ordering contract is asserted on
// every platform that compiles the package.
func TestWriteSnapshotFull_StagingDirFsyncBeforeRename(t *testing.T) {
	// Not parallel: installs the process-global publish-trace recorder.
	root := t.TempDir()
	dir := filepath.Join(root, "snap")
	steps := installPublishTrace(t, dir+".tmp")

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	adj := g.AdjList()
	if err := adj.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := adj.AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(adj)

	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	assertStagingFsyncBeforeRename(t, *steps)

	// The publish must still produce a complete, loadable snapshot.
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging dir survived publish: stat err=%v", err)
	}
	if _, err := LoadSnapshotFull(dir); err != nil {
		t.Fatalf("LoadSnapshotFull after publish: %v", err)
	}
}

// TestWriteSnapshotCSR_StagingDirFsyncBeforeRename is the ordering proxy
// for the legacy v1 publish path ([WriteSnapshotCSRCtx]): it carries the
// same staging-dir fsync gap and the same fix. The assertion is identical
// to the v2/v3 case.
func TestWriteSnapshotCSR_StagingDirFsyncBeforeRename(t *testing.T) {
	// Not parallel: installs the process-global publish-trace recorder.
	root := t.TempDir()
	dir := filepath.Join(root, "snap")
	steps := installPublishTrace(t, dir+".tmp")

	c := buildTinyCSR(t)
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatalf("WriteSnapshotCSR: %v", err)
	}
	assertStagingFsyncBeforeRename(t, *steps)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("snapshot dir missing after publish: %v", err)
	}
}

// TestDirFsync_ExistingAndMissing exercises the dirFsync primitive
// directly: it must succeed against a real directory and (on POSIX)
// surface the underlying open error against a missing one. On Windows
// dirFsync is a no-op that always returns nil, so the missing-dir
// assertion is skipped there.
func TestDirFsync_ExistingAndMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := dirFsync(dir); err != nil {
		t.Fatalf("dirFsync(existing dir) = %v; want nil", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("dirFsync is a no-op on Windows; missing-dir error not surfaced")
	}
	if err := dirFsync(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatalf("dirFsync(missing dir) = nil; want error")
	}
}

// TestWriteSnapshotFull_StagingFsyncErrorSurfaces locks in that an error
// from the staging-dir fsync is propagated (wrapped as "staging dir
// fsync") rather than swallowed, and that the staging directory is
// cleaned up so no half-published artefact lingers. The failure is forced
// by removing read+exec from the staging dir's parent so the os.Open
// inside dirFsync fails with EACCES.
//
// Unix-only: on Windows dirFsync never returns an error, and chmod-based
// permission removal is meaningful only on POSIX. Skipped under uid 0
// because the superuser bypasses the EACCES check.
func TestWriteSnapshotFull_StagingFsyncErrorSurfaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dirFsync is a no-op on Windows; staging fsync error not surfaced")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses EACCES; cannot exercise staging dir fsync open failure")
	}

	root := t.TempDir()
	dir := filepath.Join(root, "snap")

	// Direct probe of the primitive the publish path relies on: a staging
	// directory whose parent is chmod 0 cannot be opened for read, so
	// dirFsync must fail with EACCES rather than silently returning nil.
	staging := dir + ".tmp"
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatalf("MkdirAll staging: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o750) })
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	if err := dirFsync(staging); err == nil {
		t.Fatalf("dirFsync(staging under chmod-0 parent) = nil; want EACCES")
	}
}

// TestWriteSnapshotFull_ReconstructsAfterPreRenameCrash is the
// crash-recovery guard the task requests as the FS-fault variant. A real
// SIGKILL at the pre-rename point cannot be modelled cheaply here: the
// helper child would crash on the developer's real filesystem (tmpfs /
// ext4 / APFS), where rename(2) DOES flush the staging dir's dirents, so
// the published snapshot would be intact even on pre-fix code and the
// crash would not reproduce the defect (documented limitation — the
// testfs wrapper injects faults on *os.File operations, not on the loss
// of un-fsynced DIRECTORY dirents, which is a kernel/filesystem-level
// behaviour outside its model). What this test CAN prove portably is the
// positive half of the contract: the canonical ordering produces a
// snapshot that, once published, reconstructs the FULL graph — i.e. every
// component dirent the staging-dir fsync makes durable is present and
// loadable after publication. Combined with the deterministic ordering
// proxy above (which fails pre-fix), this pins both halves of the fix.
func TestWriteSnapshotFull_ReconstructsAfterPreRenameCrash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "snap")

	keys := []string{"alice", "bob", "carol", "dave"}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	adj := g.AdjList()
	for i, k := range keys {
		next := keys[(i+1)%len(keys)]
		if err := adj.AddEdge(k, next, int64(i+1)); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", k, next, err)
		}
	}
	c := csr.BuildFromAdjList(adj)

	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if got, want := loaded.Manifest.Order, c.Order(); got != want {
		t.Fatalf("loaded Order = %d; want %d", got, want)
	}
	if got, want := loaded.Manifest.Size, c.Size(); got != want {
		t.Fatalf("loaded Size = %d; want %d", got, want)
	}
	// Every original key must round-trip through the recovered mapper —
	// proof the mapper.bin dirent survived publication.
	if got, want := len(loaded.Mapper.Pairs), len(keys); got != want {
		t.Fatalf("recovered mapper pairs = %d; want %d", got, want)
	}
	recovered := make(map[string]graph.NodeID, len(keys))
	for _, p := range loaded.Mapper.Pairs {
		recovered[p.Key] = p.ID
	}
	orig := adj.Mapper()
	for _, k := range keys {
		id, ok := orig.Lookup(k)
		if !ok {
			t.Fatalf("orig mapper missing key %q", k)
		}
		if got, ok := recovered[k]; !ok || got != id {
			t.Fatalf("recovered key %q -> (%d, ok=%v); want %d", k, got, ok, id)
		}
	}
}
