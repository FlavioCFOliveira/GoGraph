package recovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestParentDirFsync_ExistingDir confirms the recovery-local
// parentDirFsync runs without error against a real directory entry. On
// Windows the helper is a no-op by design (see parent_fsync_other.go) and
// still returns nil. Mirrors the sibling guards in store/wal and
// store/snapshot.
func TestParentDirFsync_ExistingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	child := filepath.Join(dir, "snapshot") // need not exist
	if err := parentDirFsync(child); err != nil {
		t.Fatalf("parentDirFsync(existing dir) returned %v; want nil", err)
	}
}

// TestParentDirFsync_MissingParent confirms parentDirFsync surfaces the
// underlying os.Open error when the parent directory is absent — the
// failure path that feeds the fail-stop in openCodec's promotion branch.
// The helper must not silently swallow the error: a promotion whose
// directory entry cannot be made durable must never be reported as a
// successful recovery.
//
// On Windows the helper is a no-op and always returns nil, so the
// assertion is skipped there.
func TestParentDirFsync_MissingParent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("parentDirFsync is a no-op on Windows; missing-parent error not surfaced")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist", "snapshot")
	if err := parentDirFsync(missing); err == nil {
		t.Fatalf("parentDirFsync(missing parent) returned nil; want error")
	}
}

// promoteFsyncCounter is the metric the promotion path increments after
// it fsyncs the parent directory of the promoted snapshot. The name must
// match the counter emitted in openCodec.
const promoteFsyncCounter = "store.recovery.snapshot.promoteParentFsync"

// captureBackend is a metrics.Backend that counts increments of a single
// named counter, so a test can assert the promotion path issued its
// parent-dir fsync. Only the target counter is tracked; every other
// metric is ignored, so installing it globally for the duration of one
// non-parallel test does not interfere with other counters.
type captureBackend struct {
	target string
	count  atomic.Uint64
}

func (c *captureBackend) IncCounter(name string, delta uint64) {
	if name == c.target {
		c.count.Add(delta)
	}
}

func (c *captureBackend) ObserveLatency(string, time.Duration) {}

// buildInterruptedPublishState commits "pre" data, checkpoints it (the
// WAL prefix is truncated, so the pre data lives ONLY in the snapshot),
// commits WAL-only "post" data, then stages the exact interrupted-publish
// crash window: the live snapshot archived to snapshot.bak with the live
// name absent and a stale staging directory stranded. It returns the
// store directory and the txn options needed to recover it.
func buildInterruptedPublishState(t *testing.T) (string, txn.Options[string, int64]) {
	t.Helper()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	snapDir := filepath.Join(dir, "snapshot")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx := store.Begin()
	mustTx(t, tx.AddNode("pre0"))
	mustTx(t, tx.SetNodeLabel("pre0", "Pre"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(pre): %v", err)
	}

	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	txPost := store.Begin()
	mustTx(t, txPost.AddNode("post0"))
	mustTx(t, txPost.SetNodeLabel("post0", "Post"))
	if err := txPost.Commit(); err != nil {
		t.Fatalf("Commit(post): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	snapBak := snapDir + ".bak"
	if err := os.Rename(snapDir, snapBak); err != nil {
		t.Fatalf("simulate crash: rename live snapshot to backup: %v", err)
	}
	staleTmp := snapDir + ".tmp"
	if err := os.MkdirAll(staleTmp, 0o750); err != nil {
		t.Fatalf("simulate crash: create stale staging dir: %v", err)
	}
	return dir, opts
}

// TestRecovery_PromotionIssuesParentDirFsync is the regression gate for
// finding A1-F4: when recovery promotes a stranded snapshot backup
// (snapshot.bak -> snapshot) during interrupted-publish repair, it MUST
// fsync the parent directory so the promotion's directory entry is
// durable — exactly as the snapshot publish path fsyncs its own publish
// rename. Without the fix the promotion rename's dirent can be lost in a
// crash within the writeback window, and because the checkpoint already
// truncated the WAL prefix the result is the silent total loss of every
// checkpointed transaction.
//
// The fsync itself cannot be observed by a functional test without
// kernel-level crash injection, so the gate observes the metric the
// promotion path emits immediately after a successful parent-dir fsync.
// The test is deliberately NOT parallel: it installs a global capturing
// metrics backend for the duration of one recovery, which Go runs in
// isolation from the package's t.Parallel() tests.
func TestRecovery_PromotionIssuesParentDirFsync(t *testing.T) {
	dir, opts := buildInterruptedPublishState(t)

	capB := &captureBackend{target: promoteFsyncCounter}
	metrics.SetBackend(capB)
	defer metrics.SetBackend(nil)

	res, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open after interrupted publish: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true (backup snapshot must be promoted)")
	}
	// The promotion must have happened (pre data survives) AND the
	// parent-dir fsync counter must have ticked exactly once.
	if !res.Graph.HasNodeLabel("pre0", "Pre") {
		t.Error("pre node missing label Pre after recovery (promotion did not load the backup)")
	}
	if got := capB.count.Load(); got != 1 {
		t.Errorf("%s incremented %d times, want exactly 1 (promotion must fsync the parent dir once)", promoteFsyncCounter, got)
	}
}

// TestRecovery_NoPromotionNoParentFsync pins the negative arm: when the
// live snapshot is intact (no backup to promote), recovery must NOT take
// the promotion path and therefore must NOT emit the promotion fsync
// metric. This guards against the fsync being moved onto the normal
// (non-promotion) load path, where it would be both wrong and a needless
// syscall on every open.
func TestRecovery_NoPromotionNoParentFsync(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx := store.Begin()
	mustTx(t, tx.AddNode("a"))
	mustTx(t, tx.SetNodeLabel("a", "A"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var mu sync.Mutex
	cp := checkpoint.New[string, int64](checkpoint.Config{Dir: dir}, g, w, &mu)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	capB := &captureBackend{target: promoteFsyncCounter}
	metrics.SetBackend(capB)
	defer metrics.SetBackend(nil)

	res, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open with intact live snapshot: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false, want true")
	}
	if got := capB.count.Load(); got != 0 {
		t.Errorf("%s incremented %d times on a non-promotion open, want 0", promoteFsyncCounter, got)
	}
}
