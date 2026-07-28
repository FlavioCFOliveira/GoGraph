package txn

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
