//go:build soak || nightly

package checkpoint_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The list-encoding asymmetry behind rmp #2750, end to end, at the only scale
// at which it exists.
//
// store/snapshot and store/txn both encode a list element as
// (uint8 kind | uint32 len | payload), but store/snapshot gives a PropInt64
// element a fixed 8 bytes where store/txn writes a varint — 13 bytes per element
// against 6 for a small integer. The asymmetry is therefore PER ELEMENT and
// cannot be scaled down: the smallest list whose snapshot encoding crosses the
// 1 GiB cap while its WAL frame stays inside wal.Encode's own 1 GiB frame
// ceiling has 82_595_525 elements.
//
// Cost, measured on an Apple M4: about 3 s and a peak of ~7.5 GiB RSS for the
// refusal test; the at-cap control adds a ~1 GiB snapshot write to the temporary
// directory. That is why this is soak and not short: nothing about it is slow,
// but it will not share a machine with `go test -race ./...`.
//
// The cheap parts of the same gate — that store/txn computes the snapshot's
// length exactly, that the bound fires at the right byte, and that it is wired
// into Commit — are in store/txn/snapshot_foldable_value_test.go and run in the
// short layer.

// smallIntListSnapshotLen is the number of bytes store/snapshot emits for a list
// of k PropInt64 elements: a uint32 element count, then per element a kind byte,
// a uint32 length, and the fixed 8-byte int64 payload.
func smallIntListSnapshotLen(k int) int64 { return 4 + int64(k)*13 }

// TestCheckpoint_UnfoldableListIsRefusedAtCommit_2750 is the severity proof and
// the fix's end-to-end gate.
//
// Before the fix this list committed durably, and every checkpoint from then on
// failed with snapshot.ErrFieldTooLong while the WAL prefix was retained — a WAL
// that grew without bound for as long as the value lived in the graph. It is now
// refused at commit, where the caller can still act on it.
func TestCheckpoint_UnfoldableListIsRefusedAtCommit_2750(t *testing.T) {
	testlayers.RequireSoak(t)

	const cap1GiB = 1 << 30
	// Smallest k whose SNAPSHOT encoding exceeds the cap.
	k := (cap1GiB-4)/13 + 1
	if smallIntListSnapshotLen(k) <= cap1GiB {
		t.Fatalf("fixture is not over the cap: %d <= %d", smallIntListSnapshotLen(k), cap1GiB)
	}
	// ... while its WAL op frame stays well inside wal.Encode's frame ceiling,
	// which is what makes this a fold defect and not a frame-size defect.
	if walFrame := 27 + 6*int64(k); walFrame >= cap1GiB {
		t.Fatalf("fixture would be refused by the WAL frame ceiling (%d bytes), which would "+
			"make the test prove the wrong thing", walFrame)
	}

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())

	tx := st.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit node: %v", err)
	}
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	walBefore := fi.Size()

	elems := make([]lpg.PropertyValue, k)
	for i := range elems {
		elems[i] = lpg.Int64Value(int64(i % 64))
	}

	tx2 := st.Begin()
	if err := tx2.SetNodeProperty("n", "p", lpg.ListValue(elems)); err != nil {
		t.Fatalf("SetNodeProperty refused while staging: %v", err)
	}
	cerr := tx2.Commit()
	if cerr == nil {
		t.Fatalf("Commit ACCEPTED a list whose snapshot encoding is %d bytes against a %d cap. "+
			"CaptureGraph refuses it, so every checkpoint fails and the WAL prefix is never "+
			"truncated (rmp #2750)", smallIntListSnapshotLen(k), cap1GiB)
	}
	if !errors.Is(cerr, txn.ErrFieldTooLong) {
		t.Fatalf("Commit refused with %v, which does not wrap txn.ErrFieldTooLong", cerr)
	}

	// The refusal cost nothing durable: no sequence's frames were left behind
	// that a checkpoint would have to fold, and a checkpoint still succeeds.
	fi2, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal 2: %v", err)
	}
	if fi2.Size() != walBefore {
		t.Errorf("the refused transaction wrote %d bytes to the WAL; a refusal must buffer "+
			"nothing durable (was %d, now %d)", fi2.Size()-walBefore, walBefore, fi2.Size())
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.Codec()),
	)
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("checkpoint failed after the value was refused, so the store is still "+
			"trapped: %v", err)
	}
}

// TestCheckpoint_AtCapListStillCommitsAndCheckpoints_2750 is the
// bounds-rather-than-over-restricts control, and the only place it can be made:
// a value at exactly the module cap is a LIST, because a SCALAR of 1 GiB is
// refused by wal.Encode's frame ceiling before the fold bound ever sees it (the
// op frame carries the value plus its framing).
//
// k here is the largest small-integer list whose snapshot encoding still fits,
// eight bytes under the cap.
func TestCheckpoint_AtCapListStillCommitsAndCheckpoints_2750(t *testing.T) {
	testlayers.RequireSoak(t)

	const cap1GiB = 1 << 30
	k := (cap1GiB - 4) / 13
	snapLen := smallIntListSnapshotLen(k)
	if snapLen > cap1GiB {
		t.Fatalf("fixture is over the cap: %d > %d", snapLen, cap1GiB)
	}
	t.Logf("at-cap fixture: %d elements, snapshot value %d bytes, cap %d (headroom %d)",
		k, snapLen, cap1GiB, cap1GiB-snapLen)

	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())

	tx := st.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit node: %v", err)
	}

	elems := make([]lpg.PropertyValue, k)
	for i := range elems {
		elems[i] = lpg.Int64Value(int64(i % 64))
	}
	tx2 := st.Begin()
	if err := tx2.SetNodeProperty("n", "p", lpg.ListValue(elems)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("over-restricted: a list at exactly the module cap (%d bytes, cap %d) was "+
			"refused at commit: %v", snapLen, cap1GiB, err)
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.Codec()),
	)
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("over-restricted: a list at exactly the module cap committed but could not be "+
			"checkpointed: %v", err)
	}
}
