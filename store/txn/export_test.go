package txn

import "github.com/FlavioCFOliveira/GoGraph/graph/lpg"

// This file exposes internals to the external txn_test package. It is a _test.go
// file, so nothing here widens the public API of the package.

// ApplyWaiterCountForTest reports how many committers currently hold a parking
// slot on the sequence-ordered apply gate.
//
// It exists so a test can assert the gate's fast path is really taken — an
// uncontended committer must find appliedSeq == seq-1 and return without ever
// registering a slot — and so a leaked slot (the symptom of a lost wakeup)
// is observable rather than inferred from a hang.
func (s *Store[N, W]) ApplyWaiterCountForTest() int {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return len(s.applyWaiters)
}

// ApplyOpForTest applies op through wv exactly as [Tx.Commit]'s in-memory apply
// does, so a test can hold a transaction that writes what a store commit writes
// inside a bracket it controls.
func ApplyOpForTest[N comparable, W any](wv lpg.WriteView[N, W], op Op[N, W]) error {
	return applyOp(wv, op)
}

// BufferOpForTest buffers op on t as the op's public buffering method would.
func (t *Tx[N, W]) BufferOpForTest(op Op[N, W]) {
	t.ops = append(t.ops, op)
}

// IDBatchForTest reports the id reservation batch size of shard (WAL v2 step
// 4), or 0 for a store without a reserver.
func (s *Store[N, W]) IDBatchForTest(shard int) uint64 {
	if s.ids == nil {
		return 0
	}
	return s.ids.batchOf(shard)
}
