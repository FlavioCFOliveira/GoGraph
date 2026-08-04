package wal_test

// syncgroup_durable_before_poison_test.go — rmp #2322.
//
// # The defect
//
// A committer whose frames are ALREADY on stable storage was told its commit had
// FAILED, because a later group round poisoned the writer before that committer
// reached its durability barrier. Recovery then replayed the transaction — it is
// durable and fully marked, so recovery is right to — while the client had been
// told to treat it as lost. A durable transaction nobody acknowledged is
// uncommitted state leaking in, and the crash simulator's ACID_ATOMICITY oracle
// caught it as "<failed-resurrected>" at three of five ST2 seeds.
//
// Two things were wrong, and only together do they make the failure reachable:
//
//  1. [wal.Writer.SyncGroup] inferred the caller's watermark from the writer's
//     current accepted offset. That offset is shared: another appender advances
//     it, and a poison REWINDS it to the durable size. So the committer was
//     asking a question about somebody else's frames. AppendRun now returns the
//     run's own watermark and SyncGroup takes it.
//  2. SyncGroup tested the sticky poison BEFORE testing durability. The two are
//     not mutually exclusive — frames can be durable AND the writer poisoned by a
//     later round — and only durability-first is sound.
//
// # Why this was invisible before
//
// The store serialised every commit behind a single-writer semaphore, so there
// was never a committer whose frames were durable while a LATER round failed:
// rounds could not overlap. Retiring that semaphore is exactly what rmp #2306
// does for write scaling, which is how the latent hole became reachable. The
// fail-all gate (TestSyncGroup_FailAll) could not see it either, because there
// every member's frames are in the SAME failed round and every member must fail.
//
// This test is the one that discriminates: it needs a member whose round
// SUCCEEDED and a later round that failed.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestSyncGroup_DurableCommitterSurvivesALaterPoison is deterministic and
// single-threaded: no goroutines, no timing, no seeds.
//
// FailSyncAfter:1 lets the first fsync succeed and fails the second, so the
// interleaving that needed a concurrent workload to hit is expressed as a
// straight line:
//
//	A appends            — and does NOT sync yet
//	B appends, B syncs   — round 1 SUCCEEDS, covering A's frames and B's
//	C appends, C syncs   — round 2 FAILS, poisoning the writer and discarding C
//	A syncs              — A's FIRST call, and its frames are on the platter
//
// A must be told it committed. C must be told it did not.
func TestSyncGroup_DurableCommitterSurvivesALaterPoison(t *testing.T) {
	t.Parallel()

	walPath := filepath.Join(t.TempDir(), "durable_before_poison.wal")
	ff, err := testfs.New(walPath, testfs.Faults{FailSyncAfter: 1})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	payloadA := bytes.Repeat([]byte{0xAA}, 16)
	payloadB := bytes.Repeat([]byte{0xBB}, 16)
	payloadC := bytes.Repeat([]byte{0xCC}, 16)

	appendOne := func(name string, payload []byte) int64 {
		mark, aerr := w.AppendRun(func(emit func([]byte) error) error { return emit(payload) })
		if aerr != nil {
			t.Fatalf("append %s: %v", name, aerr)
		}
		return mark
	}

	// A appends and deliberately does not reach its durability barrier yet.
	markA := appendOne("A", payloadA)

	// B appends and leads round 1. Its fsync covers the whole buffered suffix,
	// which includes A's frame: after this, A IS durable even though A has not
	// asked yet.
	markB := appendOne("B", payloadB)
	if serr := w.SyncGroup(markB); serr != nil {
		t.Fatalf("round 1 must succeed (FailSyncAfter:1 fails only the second fsync): %v", serr)
	}
	if markA > markB {
		t.Fatalf("markA=%d > markB=%d: A's frame must precede B's for round 1 to cover it", markA, markB)
	}

	// C appends and leads round 2, whose fsync fails. The writer poisons: C's
	// frame is truncated away and the sticky error is set.
	markC := appendOne("C", payloadC)
	syncErrC := w.SyncGroup(markC)
	if syncErrC == nil {
		t.Fatal("round 2 must fail: FailSyncAfter:1 makes the second fsync return EIO")
	}
	if !errors.Is(syncErrC, wal.ErrDurabilityFailed) {
		t.Errorf("round 2 error = %v; want errors.Is(wal.ErrDurabilityFailed)", syncErrC)
	}

	// THE ASSERTION. A reaches its durability barrier only now, after the poison.
	// Its frame is on stable storage, so its commit succeeded and it must be told
	// so — the sticky error belongs to C's round, not to A's.
	if serr := w.SyncGroup(markA); serr != nil {
		t.Fatalf("SyncGroup(markA=%d) = %v; want nil.\n"+
			"A's frame was made durable by round 1, which SUCCEEDED. Failing A here "+
			"tells a client its commit was lost while recovery will replay it — a "+
			"durable transaction nobody acknowledged (rmp #2322). SyncGroup must test "+
			"the caller's OWN watermark against durableSize BEFORE it consults the "+
			"sticky poison, which belongs to a later round.", markA, serr)
	}

	// The negative control, so the fix cannot be "return nil more often": C's
	// frame was discarded by the poison and C must still fail. Asked again, as a
	// woken group follower would re-evaluate it.
	if serr := w.SyncGroup(markC); serr == nil {
		t.Errorf("SyncGroup(markC=%d) = nil; want the durability error. C's frame is "+
			"ABOVE durableSize because the poison truncated it, so C must never be "+
			"told it committed.", markC)
	}

	// And the on-disk truth must match what each committer was told: A's and B's
	// frames survive, C's does not.
	_ = w.Close()
	surviving := readFramePayloads(t, walPath)
	want := [][]byte{payloadA, payloadB}
	if len(surviving) != len(want) {
		t.Fatalf("recovered %d frames, want %d (A and B durable, C discarded)", len(surviving), len(want))
	}
	for i := range want {
		if !bytes.Equal(surviving[i], want[i]) {
			t.Errorf("frame %d = %x, want %x", i, surviving[i], want[i])
		}
	}
}

// readFramePayloads returns the payload of every frame the WAL at path decodes
// cleanly, which is what a recovery pass would replay.
func readFramePayloads(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- path is this test's own temp fixture.
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	r := wal.NewReader(f, io.NopCloser(f))
	defer func() { _ = r.Close() }()
	var out [][]byte
	for fr := range r.Frames() {
		out = append(out, bytes.Clone(fr.Payload))
	}
	if terr := r.TailError(); terr != nil {
		t.Fatalf("tail error after the poison truncation: %v", terr)
	}
	return out
}
