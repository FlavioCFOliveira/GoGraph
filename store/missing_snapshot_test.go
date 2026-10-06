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
