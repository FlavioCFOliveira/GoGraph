package store_test

// missing_snapshot_test.go — rmp #2990: a directory whose WAL prefix a
// checkpoint truncated, and whose snapshot directory is then lost, must be
// refused by recovery rather than opened as a shorter, clean history.
//
// Layer: short.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// missingSnapshotCommits is the number of acknowledged commits the checkpoint
// folds. Any positive count reproduces the defect; a handful keeps the WAL
// prefix non-empty so the truncation is real.
const missingSnapshotCommits = 8

// checkpointedDir builds a store directory through store.Open, commits
// missingSnapshotCommits nodes, runs one checkpoint wired to the store's commit
// lock (so it publishes a snapshot and truncates the WAL prefix), and closes it.
// It returns the directory and the committed keys.
func checkpointedDir(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	keys := make([]string, missingSnapshotCommits)
	for i := range keys {
		keys[i] = fmt.Sprintf("folded-%02d", i)
	}
	commitNodes(t, o.Store(), keys...)

	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	if cp.Stats().WALTruncBytes == 0 {
		t.Fatal("the checkpoint truncated no WAL bytes: the fixture does not model a " +
			"truncated prefix and cannot detect the defect")
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dir, keys
}

// TestRecovery_RefusesTruncatedWALWithoutSnapshot reproduces rmp #2990 and
// asserts the refusal on both open paths: recovery.Open and store.Open.
//
// Before the fix recovery.Open returned a nil error with IsClean true and a
// graph holding none of the folded commits, and store.Open opened that graph
// for writing.
func TestRecovery_RefusesTruncatedWALWithoutSnapshot(t *testing.T) {
	t.Parallel()
	dir, keys := checkpointedDir(t)

	// The marker is what makes the state detectable; it must exist after a
	// truncation.
	marker := wal.PrefixTruncatedMarkerPath(filepath.Join(dir, "wal"))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("prefix marker %s after a truncating checkpoint: %v", marker, err)
	}

	// Control: with the snapshot in place the directory recovers clean and
	// holds every folded commit, so the refusal below is about the missing
	// snapshot and nothing else.
	ctl, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil || !ctl.IsClean() || !ctl.SnapshotHit {
		t.Fatalf("control recovery with the snapshot: err=%v clean=%v snapshotHit=%v",
			err, ctl.IsClean(), ctl.SnapshotHit)
	}
	for _, k := range keys {
		if !has(ctl.Graph.AdjList().Mapper(), k) {
			t.Fatalf("control recovery lost folded commit %q", k)
		}
	}

	if err := os.RemoveAll(filepath.Join(dir, "snapshot")); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if !errors.Is(err, recovery.ErrMissingSnapshot) {
		t.Fatalf("recovery.Open error = %v, want %v (IsClean=%v, recovered %d node(s) of %d folded)",
			err, recovery.ErrMissingSnapshot, res.IsClean(), res.Graph.AdjList().Order(), len(keys))
	}
	if res.IsClean() {
		t.Error("IsClean() = true on a refused recovery")
	}
	if !errors.Is(res.TailErr, recovery.ErrMissingSnapshot) {
		t.Errorf("TailErr = %v, want %v", res.TailErr, recovery.ErrMissingSnapshot)
	}

	walBefore := readWAL(t, dir)
	_, err = store.Open[string, float64](dir, openOptions())
	var unclean *store.UncleanRecoveryError[string, float64]
	if !errors.As(err, &unclean) || !errors.Is(err, store.ErrUncleanRecovery) ||
		!errors.Is(err, recovery.ErrMissingSnapshot) {
		t.Fatalf("store.Open error = %v, want an UncleanRecoveryError wrapping %v",
			err, recovery.ErrMissingSnapshot)
	}
	if got := readWAL(t, dir); !bytes.Equal(got, walBefore) {
		t.Error("store.Open modified the WAL of a refused directory")
	}
	requireWALLockFree(t, dir)
}

// TestRecovery_UntruncatedWALWithoutSnapshotStillOpens is the boundary: a WAL
// that was never prefix-truncated carries no marker and is the whole history,
// so a directory without a snapshot recovers it clean exactly as before.
func TestRecovery_UntruncatedWALWithoutSnapshotStillOpens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	commitNodes(t, o.Store(), "a", "b")
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(wal.PrefixTruncatedMarkerPath(filepath.Join(dir, "wal"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prefix marker present without any truncation: %v", err)
	}
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil || !res.IsClean() {
		t.Fatalf("recovery of an untruncated WAL: err=%v clean=%v", err, res.IsClean())
	}
	for _, k := range []string{"a", "b"} {
		if !has(res.Graph.AdjList().Mapper(), k) {
			t.Errorf("committed key %q missing", k)
		}
	}
}

// TestRecovery_PreMarkerStoreGainsRecordOnFirstSnapshotRecovery pins rmp #3002:
// a store whose WAL prefix was truncated before the marker existed (simulated by
// deleting the marker) gains the record the first time recovery loads its
// self-sufficient snapshot, so a later loss of the snapshot is refused.
func TestRecovery_PreMarkerStoreGainsRecordOnFirstSnapshotRecovery(t *testing.T) {
	t.Parallel()
	dir, keys := checkpointedDir(t)
	marker := wal.PrefixTruncatedMarkerPath(filepath.Join(dir, "wal"))
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker to simulate a pre-marker store: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil || !res.IsClean() || !res.SnapshotSelfSufficient {
		t.Fatalf("first recovery: err=%v clean=%v selfSufficient=%v",
			err, res.IsClean(), res.SnapshotSelfSufficient)
	}
	for _, k := range keys {
		if !has(res.Graph.AdjList().Mapper(), k) {
			t.Fatalf("first recovery lost folded commit %q", k)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker after the first snapshot-backed recovery: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(dir, "snapshot")); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if _, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	}); !errors.Is(err, recovery.ErrMissingSnapshot) {
		t.Fatalf("recovery after losing the snapshot = %v, want %v", err, recovery.ErrMissingSnapshot)
	}
}

// TestCheckpoint_RewritesPrefixMarkerAtEveryCheckpoint pins rmp #3002's other
// half: the control record is rewritten at every checkpoint, not only at a
// Writer's first truncation, so a marker lost while the store runs is restored by
// its next checkpoint.
func TestCheckpoint_RewritesPrefixMarkerAtEveryCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = o.Close() }()
	var unused sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &unused,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
	marker := wal.PrefixTruncatedMarkerPath(filepath.Join(dir, "wal"))
	for i, k := range []string{"first", "second"} {
		commitNodes(t, o.Store(), k)
		if err := cp.RunCheckpoint(); err != nil {
			t.Fatalf("checkpoint %d: %v", i+1, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("marker after checkpoint %d: %v", i+1, err)
		}
		if i == 0 {
			if err := os.Remove(marker); err != nil {
				t.Fatalf("remove marker: %v", err)
			}
		}
	}
}

// TestOpen_UncleanRecoveryWritesNoPrefixMarker pins the storage audit's F1 on
// rmp #3002: the marker is written only by a CLEAN recovery. A checkpointed store
// without the marker (a pre-marker store) whose last committed transaction holds
// an undecodable op recovers unclean with a nil error; neither the refused
// store.Open nor the read-only AllowUnclean open may write a byte, and the
// corruption must stay visible (F2).
func TestOpen_UncleanRecoveryWritesNoPrefixMarker(t *testing.T) {
	t.Parallel()
	dir, _ := checkpointedDir(t)
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	commitNodes(t, o.Store(), "after-checkpoint")
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	marker := wal.PrefixTruncatedMarkerPath(filepath.Join(dir, "wal"))
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker to simulate a pre-marker store: %v", err)
	}
	seqs := walCommitSeqs(t, dir)
	if len(seqs) == 0 {
		t.Fatal("no committed transaction in the WAL suffix to corrupt")
	}
	injectUndecodableBodyInCommittedTxn(t, dir, seqs[len(seqs)-1])

	noMarker := func(stage string) {
		t.Helper()
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s wrote the prefix marker on an unclean recovery (stat: %v)", stage, err)
		}
	}

	_, err = store.Open[string, float64](dir, openOptions())
	if !errors.Is(err, store.ErrUncleanRecovery) || !errors.Is(err, recovery.ErrCommittedTxnCorruptOp) {
		t.Fatalf("store.Open = %v, want %v wrapping %v", err, store.ErrUncleanRecovery, recovery.ErrCommittedTxnCorruptOp)
	}
	noMarker("the refused store.Open")

	ro, err := store.Open[string, float64](dir, readOnlyOptions())
	if err != nil {
		t.Fatalf("store.Open with AllowUnclean: %v", err)
	}
	defer func() { _ = ro.Close() }()
	if !ro.ReadOnly() {
		t.Fatal("AllowUnclean open of an unclean recovery is not read-only")
	}
	if !errors.Is(ro.Recovery().TailErr, recovery.ErrCommittedTxnCorruptOp) {
		t.Fatalf("TailErr = %v, want %v", ro.Recovery().TailErr, recovery.ErrCommittedTxnCorruptOp)
	}
	noMarker("the AllowUnclean open")
}
