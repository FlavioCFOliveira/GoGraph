package checkpoint_test

// The end-to-end half of the rmp #2784 gate: a by-handle edge record that
// store/snapshot CAN capture still commits, still checkpoints, and still lets
// the checkpointer reclaim the WAL prefix, now that store/txn refuses the
// records it cannot capture.
//
// # Why the over-cap run is not here, and what stands in for it
//
// The defect was established end to end against a build with store/snapshot's
// maxPerRecordCount lowered to 64: an edge handle carrying 65 properties
// COMMITTED, three consecutive checkpoints then failed with
//
//	snapshot: capture edgehandles.bin: snapshot: field too long for the
//	reader's cap: edge handle property count is 65 bytes, maximum 64
//
// and the WAL stayed at 3362 bytes across all three — never truncated, growing
// for as long as the record lived. The 64-property control committed,
// checkpointed, and truncated the WAL to 0. Deleting one property then made the
// next checkpoint succeed and reclaim the WAL: the migration path for a store
// written before the guard needs no tooling and loses no data.
//
// That run cannot be reproduced at the REAL cap. Building a handle that holds
// 1 Mi properties costs on the order of eight hours of CPU on an Apple M4 — the
// write path is quadratic in the handle's property count, measured at
// 6.27 s / 25.13 s / 101.81 s / 452.45 s for 16k / 32k / 64k / 128k, which puts
// 1_048_577 between 7.5 h and 8.4 h — for reasons that lie in lpg's MVCC
// pre-image clone and its copy-on-write key registry, not in this bound. The refusal at the real cap is instead proven through Commit in
// store/txn (a refused transaction never applies its ops, so it never pays the
// quadratic), and that store/snapshot really refuses at 1_048_577 and really
// writes at 1_048_576 is proven in store/snapshot. This file proves the
// remaining link: that installing the guard did not break the ordinary case.

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCheckpoint_HandleRecordUnderCapStillFolds_2784 drives one edge handle
// carrying labels AND properties, across several transactions, through commit
// and checkpoint, and asserts the WAL prefix is actually reclaimed.
//
// The WAL assertion is the load-bearing one. A guard that refused a legitimate
// record would show up as a failed commit; a guard that let an uncapturable one
// through would show up here as a WAL that never shrinks, which is the defect
// #2784 names. Asserting only "checkpoint returned nil" would miss the second.
func TestCheckpoint_HandleRecordUnderCapStillFolds_2784(t *testing.T) {
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
	if err := tx.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := tx.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}
	h := g.NextEdgeHandle()
	if err := tx.AddEdgeWithHandle("a", "b", 1.0, h); err != nil {
		t.Fatalf("AddEdgeWithHandle: %v", err)
	}
	if err := tx.SetEdgeLabelByHandle("a", "b", h, "KNOWS"); err != nil {
		t.Fatalf("SetEdgeLabelByHandle: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit edge: %v", err)
	}

	// Several transactions, so the handle's bag ACCUMULATES across commits —
	// the accumulation that makes DefaultMaxTxnOps no bound on this quantity at
	// all, and that the guard therefore has to read out of the graph rather than
	// count out of the batch.
	const perTx, txCount = 40, 5
	for b := 0; b < txCount; b++ {
		tx := st.Begin()
		for i := 0; i < perTx; i++ {
			k := "k" + strconv.Itoa(b*perTx+i)
			if err := tx.SetEdgePropertyByHandle("a", "b", h, k, lpg.Int64Value(int64(i))); err != nil {
				t.Fatalf("SetEdgePropertyByHandle %s: %v", k, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("over-restricted: commit %d of an ordinary by-handle record was "+
				"refused: %v (rmp #2784)", b, err)
		}
	}

	if got := len(g.EdgePropertiesByHandle("a", "b", h)); got != perTx*txCount {
		t.Fatalf("handle carries %d properties, want %d — the fixture did not land, so "+
			"anything this test goes on to assert is vacuous", got, perTx*txCount)
	}
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("the WAL is empty before the checkpoint, so a later truncation would " +
			"prove nothing")
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.Codec()),
	)
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatalf("a by-handle record well under the cap could not be checkpointed: %v", err)
	}
	fi2, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal after checkpoint: %v", err)
	}
	if fi2.Size() >= fi.Size() {
		t.Fatalf("the checkpoint did not reclaim the WAL prefix: %d bytes before, %d after. "+
			"That is the shape of the rmp #2784 defect — capture refused, phase 3 never "+
			"ran — and it must not appear for a record under the cap", fi.Size(), fi2.Size())
	}
}
