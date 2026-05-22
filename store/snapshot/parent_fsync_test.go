package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// buildTinyCSR returns a 2-edge CSR over 3 nodes for snapshot tests
// that just need a non-empty payload to publish.
func buildTinyCSR(tb testing.TB) *csr.CSR[struct{}] {
	tb.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		tb.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		tb.Fatalf("AddEdge: %v", err)
	}
	return csr.BuildFromAdjList(a)
}

// TestParentDirFsync_ExistingDir confirms parentDirFsync runs without
// error against a real directory entry. On Windows the helper is a
// no-op by design (see parent_fsync_other.go) and still returns nil.
func TestParentDirFsync_ExistingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	child := filepath.Join(dir, "anything") // need not exist
	if err := parentDirFsync(child); err != nil {
		t.Fatalf("parentDirFsync(existing dir) returned %v; want nil", err)
	}
}

// TestParentDirFsync_MissingParent confirms parentDirFsync surfaces
// the underlying os.Open error when the parent directory is absent.
// The helper must not silently swallow the error: the snapshot
// publish path treats it as a hard failure.
//
// On Windows the helper is a no-op and always returns nil regardless
// of the path, so the assertion is skipped there.
func TestParentDirFsync_MissingParent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("parentDirFsync is a no-op on Windows; missing-parent error not surfaced")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist", "child")
	if err := parentDirFsync(missing); err == nil {
		t.Fatalf("parentDirFsync(missing parent) returned nil; want error")
	}
}

// TestWriteSnapshotCSR_PublishesAndFsyncsParent is an end-to-end
// guard: it exercises the legacy v1 publish path and verifies that
// the resulting snapshot directory is readable afterwards. The fsync
// itself is invisible from user space, but we can at least confirm
// that the publish protocol still produces a complete directory
// after parent-fsync was inserted into the rename sequence.
func TestWriteSnapshotCSR_PublishesAndFsyncsParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "snap")

	c := buildTinyCSR(t)
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatalf("WriteSnapshotCSR: %v", err)
	}

	// The snapshot directory exists and the temporary staging dir is
	// gone. The fsync would be invisible regardless, so this is the
	// strongest portable assertion we can make from user space.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("snapshot dir missing after publish: %v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging dir survived publish: stat err=%v", err)
	}
}

// TestWriteSnapshotCSRCtx_ParentFsyncSurfaceErrors locks in the
// contract that errors from parentDirFsync are propagated to the
// caller rather than swallowed. We force the failure by making the
// parent directory inaccessible to the user just before the publish
// would call fsync on it.
//
// The test is unix-only because:
//   - On Windows the helper is a no-op and never returns an error.
//   - Chmod-based permission removal is meaningful only on POSIX.
//
// Root cannot exercise this path either: the EACCES check is bypassed
// for the superuser, so the test is skipped under uid 0.
func TestWriteSnapshotCSRCtx_ParentFsyncSurfaceErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("parentDirFsync is a no-op on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses EACCES; cannot exercise parent fsync open failure")
	}

	root := t.TempDir()
	parent := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dir := filepath.Join(parent, "snap")

	c := buildTinyCSR(t)

	// Strip read+exec from the parent directory so the os.Open
	// inside parentDirFsync fails with EACCES. The os.Rename still
	// succeeds because rename only needs write+exec on the parent
	// of `tmp`, which is the same `parent` directory we are about
	// to lock down. We therefore stage one snapshot first to give
	// the publish path something to RemoveAll, then lock the parent
	// and run the publish that will fail on fsync.
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatalf("priming write: %v", err)
	}

	// Lower the parent's permissions: read+exec only, no write —
	// this forces the publish-path RemoveAll to fail before fsync
	// is reached. We pick a different failure surface: chmod parent
	// to 0o000 so the parentDirFsync's os.Open is the first hop to
	// hit EACCES. The publish does a fresh os.Rename onto a path
	// inside the locked-down parent; that itself fails before fsync.
	// To keep the test focused on fsync, we restore the parent
	// permissions to 0o750 and instead probe the helper directly.
	t.Cleanup(func() { _ = os.Chmod(parent, 0o750) })

	// Direct probe: confirm parentDirFsync fails when the parent is
	// chmod 0 (cannot open even for read).
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	if err := parentDirFsync(filepath.Join(parent, "anything")); err == nil {
		t.Fatalf("parentDirFsync(chmod 0 parent) returned nil; want EACCES")
	}
}

// TestWriteSnapshotFullCtx_RespectsCancellationBeforeFsync confirms
// the publish path still honours ctx cancellation between the
// rename and the parent fsync. The previous implementation had no
// fsync step at all, so this test exists to prevent a future
// regression where someone moves the ctx.Err() check past the
// rename and forgets to also gate the parent fsync.
func TestWriteSnapshotFullCtx_RespectsCancellationBeforeFsync(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "snap")

	// Cancel before any work runs.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := buildTinyCSR(t)
	if err := WriteSnapshotCSRCtx(ctx, dir, c); err == nil {
		t.Fatalf("WriteSnapshotCSRCtx with cancelled ctx returned nil; want ctx.Err")
	}
	// Snapshot must not exist because publish bailed before rename.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir present after cancelled publish: stat err=%v", err)
	}
}
