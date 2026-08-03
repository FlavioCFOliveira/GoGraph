package recovery_test

// interleaved_txn_test.go — what interleaved WAL frames cost recovery
// (rmp #2302, audit finding E5).
//
// Design: docs/design-wal-transaction-contiguity.md.
//
// recovery.Open commits the ops carrying a marker's own TxnSeq and discards the
// buffered prefix as orphaned (recovery.go, the "Atomicity guard" comment), and
// says in its own words why that is sound: "The store serialises commits (single
// writer), so a transaction's frames are contiguous and never interleave with
// another's."
//
// These two tests are the same two transactions written two ways. Contiguous,
// both recover and nothing is orphaned. Interleaved, COMMITTED ops are dropped
// and the only trace is a counter. The losing arm stays here permanently: the
// failure is silent, so the next person to touch the append path needs to be able
// to see what it costs rather than read that it would.

import (
	"encoding/binary"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// orphanCounter is a metrics backend that records just the counter recovery
// increments when it discards a buffered prefix as orphaned. That counter is the
// ONLY signal the defect emits — there is no field on recovery.Result for it —
// which is precisely why the audit calls E5 "the most dangerous item in the whole
// sprint because its only symptom is a counter". Asserting on it means installing
// a backend that can see it.
type orphanCounter struct {
	mu sync.Mutex
	n  uint64
}

func (o *orphanCounter) IncCounter(name string, delta uint64) {
	if name != "store.recovery.openCodec.orphanedOps" {
		return
	}
	o.mu.Lock()
	o.n += delta
	o.mu.Unlock()
}
func (o *orphanCounter) ObserveLatency(string, time.Duration) {}
func (o *orphanCounter) SetGauge(string, float64)             {}

func (o *orphanCounter) count() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

// v3AddNode builds the payload txn.encodeOpTypedV3Into produces for an
// OpAddNode: version, kind, the 8-byte little-endian transaction sequence, the
// codec-encoded src key, a zero dst slot, and labelLen = 0.
func v3AddNode(t *testing.T, codec txn.Codec[string], seq uint64, key string) []byte {
	t.Helper()
	buf := []byte{txn.OpRecordV3, byte(txn.OpAddNode)}
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	var err error
	if buf, err = codec.Encode(buf, key); err != nil {
		t.Fatalf("codec.Encode(src=%q): %v", key, err)
	}
	if buf, err = codec.Encode(buf, ""); err != nil {
		t.Fatalf("codec.Encode(zero dst): %v", err)
	}
	buf = binary.LittleEndian.AppendUint16(buf, 0)
	return buf
}

// v3Commit builds the OpCommit marker for seq, matching
// txn.encodeCommitV3Into.
func v3Commit(seq uint64) []byte {
	buf := []byte{txn.OpRecordV3, byte(txn.OpCommit)}
	return binary.LittleEndian.AppendUint64(buf, seq)
}

// writeFrames writes payloads to a fresh WAL under dir, in the order given, and
// syncs. The order IS the test: it is what a contiguous run produces versus what
// two concurrent per-frame appenders produce.
func writeFrames(t *testing.T, dir string, payloads [][]byte) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	for i, p := range payloads {
		if aerr := w.Append(p); aerr != nil {
			t.Fatalf("Append(%d): %v", i, aerr)
		}
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
}

// recoverNodes replays the WAL under dir and reports which of the given keys the
// recovered graph holds, plus how many ops recovery orphaned.
func recoverNodes(t *testing.T, dir string, keys []string) (present map[string]bool, orphaned uint64) {
	t.Helper()
	oc := &orphanCounter{}
	metrics.SetBackend(oc)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	g := res.Graph
	if res.TailErr != nil {
		t.Fatalf("recovery reported a tail error on a WAL of individually valid frames: %v",
			res.TailErr)
	}
	present = make(map[string]bool, len(keys))
	for _, k := range keys {
		_, ok := g.AdjList().Mapper().Lookup(k)
		present[k] = ok
	}
	return present, oc.count()
}

// TestRecovery_ContiguousTransactionsRecoverCompletely is the acceptance
// criterion: two transactions, both committed, both fully restored, nothing
// orphaned.
//
// This is the frame order [wal.Writer.AppendRun] guarantees regardless of how
// many writers are appending, which is the whole reason it exists.
func TestRecovery_ContiguousTransactionsRecoverCompletely(t *testing.T) {
	dir := t.TempDir()
	codec := txn.NewStringCodec()
	writeFrames(t, dir, [][]byte{
		v3AddNode(t, codec, 1, "a1"),
		v3AddNode(t, codec, 1, "a2"),
		v3Commit(1),
		v3AddNode(t, codec, 2, "b1"),
		v3AddNode(t, codec, 2, "b2"),
		v3Commit(2),
	})

	present, orphaned := recoverNodes(t, dir, []string{"a1", "a2", "b1", "b2"})
	for _, k := range []string{"a1", "a2", "b1", "b2"} {
		if !present[k] {
			t.Errorf("node %q was committed and is missing after recovery", k)
		}
	}
	if orphaned != 0 {
		t.Errorf("recovery orphaned %d ops from two cleanly contiguous transactions, want 0",
			orphaned)
	}
}

// TestRecovery_InterleavedTransactionsDropCommittedOps is the negative arm — the
// defect, reproduced rather than argued.
//
// # The interleaving that LOSES data, and why the obvious one does not
//
// The first shape tried here was
//
//	a1(1)  b1(2)  a2(1)  commit(1)  b2(2)  commit(2)
//
// and it recovers all four nodes with ZERO orphans. That is not the fix working;
// it is a different violation. On commit(1) the buffer is [a1(1), b1(2), a2(1)],
// the suffix scan finds seq 1 at index 0, so start == 0 and ALL THREE are applied
// — b1 becomes durable under transaction 1's marker, before transaction 2
// committed. A crash between the two markers leaves b1 durable with no commit
// behind it: a phantom partial commit, covered separately below.
//
// Data LOSS needs the foreign op to sit in the buffer's PREFIX:
//
//	b1(2)  a1(1)  a2(1)  commit(1)  b2(2)  commit(2)
//
// Now the scan walks past index 0 to find seq 1, so start == 1 and b1 is
// discarded as an orphan. The buffer then resets, so b1 is never seen again —
// transaction 2 commits with only half its ops. Committed data gone, one counter
// incremented, nothing else.
//
// Both orders occur under concurrent per-frame appenders, which is why contiguity
// moved into the WAL writer rather than recovery being taught to tolerate
// interleaving.
func TestRecovery_InterleavedTransactionsDropCommittedOps(t *testing.T) {
	dir := t.TempDir()
	codec := txn.NewStringCodec()
	writeFrames(t, dir, [][]byte{
		v3AddNode(t, codec, 2, "b1"), // transaction 2's first op lands first
		v3AddNode(t, codec, 1, "a1"),
		v3AddNode(t, codec, 1, "a2"),
		v3Commit(1),
		v3AddNode(t, codec, 2, "b2"),
		v3Commit(2),
	})

	present, orphaned := recoverNodes(t, dir, []string{"a1", "a2", "b1", "b2"})
	t.Logf("interleaved recovery: present=%v orphanedOps=%d", present, orphaned)

	if present["b1"] {
		t.Fatal("b1 survived an interleaving that discards it as an orphan. Recovery now " +
			"tolerates interleaving, so the WAL contiguity guarantee is no longer what " +
			"protects atomicity — re-read docs/design-wal-transaction-contiguity.md before " +
			"deleting anything")
	}
	if orphaned == 0 {
		t.Fatal("b1 is missing but nothing was counted as orphaned: the one signal this " +
			"defect emits did not fire, so the instrument cannot see it")
	}
	// The rest of both transactions is present, which is what makes the loss
	// silent: three of four ops are there and the graph looks plausible.
	for _, k := range []string{"a1", "a2", "b2"} {
		if !present[k] {
			t.Errorf("node %q is missing; the test means to isolate b1's loss", k)
		}
	}
	t.Logf("reproduced: %d committed op(s) discarded as orphans; transaction 2 recovered "+
		"with only half its writes", orphaned)
}

// TestRecovery_InterleavedTransactionsCommitAnotherTransactionEarly is the second
// violation the same interleaving produces, in the order that does NOT lose data.
//
// With the foreign op in the buffer's middle, the suffix scan starts at 0 and
// applies it under the WRONG marker. Truncating the WAL just after that marker is
// the crash that makes it visible: transaction 2 never committed, and one of its
// ops is durable anyway.
func TestRecovery_InterleavedTransactionsCommitAnotherTransactionEarly(t *testing.T) {
	dir := t.TempDir()
	codec := txn.NewStringCodec()
	// Exactly the frames that precede transaction 2's own commit marker — i.e.
	// the durable state after a crash between the two markers.
	writeFrames(t, dir, [][]byte{
		v3AddNode(t, codec, 1, "a1"),
		v3AddNode(t, codec, 2, "b1"), // transaction 2 interleaves, mid-buffer
		v3AddNode(t, codec, 1, "a2"),
		v3Commit(1),
	})

	present, orphaned := recoverNodes(t, dir, []string{"a1", "a2", "b1"})
	t.Logf("recovery after a crash between the markers: present=%v orphanedOps=%d",
		present, orphaned)

	if !present["b1"] {
		t.Fatal("b1 was NOT applied under transaction 1's marker. Recovery no longer fuses " +
			"a foreign op into a committing transaction, so this violation is closed and " +
			"the reasoning in docs/design-wal-transaction-contiguity.md needs revisiting")
	}
	t.Log("reproduced: transaction 2 never committed, yet its op b1 is durable — a phantom " +
		"partial commit, which is an Atomicity violation rather than a Durability one")
}
