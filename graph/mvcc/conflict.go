package mvcc

// conflict.go — write-write conflict detection (rmp #2300).
//
// Design: docs/design-write-conflict-detection.md.
//
// # The rule, in one line
//
// A writer may modify an object only if the object's newest version is VISIBLE
// to it. If it is not, someone the writer cannot see wrote that version, and
// overwriting it would lose their update.
//
// That is not a new predicate. It is [Visible], applied to the chain head
// instead of to a version being read — and the delta chains already run exactly
// this expression to decide whether to undo a version, which is what
// nodeLabelDelta.mustUndo is (graph/lpg/mvcc_labels.go:124-130). Asking
// "would I have had to undo this version in order to read it?" and "may I
// overwrite it?" are the same question, so they get the same answer from the
// same code. Two definitions of visibility that can drift apart is precisely
// how a lost update gets shipped.
//
// # Prior art
//
// Memgraph's PrepareForWrite (memgraph/memgraph, branch master, read
// 2026-08-02; src/storage/v2/mvcc.hpp) tests the newest delta's timestamp in
// three cases, in this order:
//
//	if (ts == transaction->transaction_id) { ... return true; }   // my own
//	if (ts < transaction->start_timestamp)  { ... return true; }   // before me
//	transaction->has_serialization_error = true; return false;     // otherwise
//
// [Conflicts] is those three cases expressed through [Visible]. The boundary
// differs by one comparison — Memgraph tests `ts < start_timestamp`, GoGraph
// `ts <= startTS` — because GoGraph's start timestamp is the CONTIGUOUS
// FRONTIER (rmp #2298): a commit exactly at the frontier has finished, so it is
// visible by construction.
//
// PostgreSQL's shape was considered and NOT taken. Its heap update path returns
// TM_BeingModified and then, under READ COMMITTED, WAITS for the other
// transaction and re-evaluates. Waiting needs a wait-for graph and a deadlock
// detector, which is exclusion by another name and the opposite of what this
// sprint is for; and at snapshot isolation there is nothing useful to wait for,
// since a transaction may not adopt a version newer than its own snapshot and
// the answer after the wait is still a serialization failure.

import "errors"

// ErrSerializationConflict is returned when a transaction tries to modify an
// object whose newest version it cannot see: either another transaction is
// still writing it, or another transaction committed it after this one began.
//
// It is RETRIABLE. The transaction that receives it has not lost any work that
// a retry cannot redo — its own writes are discarded, it takes a fresh
// snapshot, and it tries again against a state that now includes the change it
// collided with. Callers should match it with [errors.Is] rather than by string.
var ErrSerializationConflict = errors.New("mvcc: serialization conflict: the object was modified by a concurrent transaction")

// Conflict describes a detected write-write conflict, so an error message can
// say WHICH transaction lost to WHAT rather than only that something collided.
//
// It wraps [ErrSerializationConflict], so `errors.Is(err, ErrSerializationConflict)`
// identifies it and `errors.As` recovers the detail.
type Conflict struct {
	// Store names the versioned store the conflict was detected in — "node
	// labels", "node properties", "adjacency", and so on. It is the first thing
	// a reader of a bug report needs and the last thing a stack trace gives.
	Store string
	// HeadTS is the effective timestamp of the version that blocked the write:
	// another transaction's id while it is in flight, or its commit timestamp
	// once it has committed.
	HeadTS uint64
	// StartTS and TxID are the losing transaction's snapshot and identity.
	StartTS uint64
	TxID    uint64
}

func (c *Conflict) Error() string {
	return "mvcc: serialization conflict in " + c.Store + ": the newest version is not visible to this transaction"
}

// Unwrap makes errors.Is(err, ErrSerializationConflict) true.
func (c *Conflict) Unwrap() error { return ErrSerializationConflict }

// ConcurrentWriter reports whether the blocking version belongs to a
// transaction that has NOT finished — first-updater-wins — as opposed to one
// that committed after this transaction's snapshot, which is
// first-committer-wins.
//
// Both are serialization failures and both are retriable; they are
// distinguished only so an operator reading metrics can tell overlapping
// writers from a snapshot that went stale.
func (c *Conflict) ConcurrentWriter() bool { return c.HeadTS >= TxIDBase }

// Conflicts reports whether a transaction holding startTS and txID may modify
// an object whose newest version carries headTS.
//
// It is the exact negation of [Visible]: a version the transaction could not
// have READ is a version it must not OVERWRITE. See the file comment for why
// the two share one predicate rather than having one each.
//
// A headTS of zero means the object has no recorded version — nothing has
// written it since the last reclamation — and never conflicts.
//
// # An ABORTED head never conflicts either (rmp #2300)
//
// [AbortedTS] names a transaction whose changes are permanently invisible to every
// reader, so displacing its version cannot lose anybody's update — there is no
// update to lose. It is the one place where [Conflicts] is NOT the plain negation
// of [Visible]: an aborted version must stay INVISIBLE (a reader still has to undo
// it to reach the pre-abort value, so it may not be treated as committed) while
// being freely OVERWRITABLE.
//
// Without this the exemption is not an optimisation but a liveness bug, and it was
// measured the moment abort was wired: AbortedTS sits above [TxIDBase], so
// `!Visible` is true for it forever, and the FIRST transaction to abort on an object
// made that object permanently unwritable. Every later writer was refused, retried,
// and was refused again — examples/27_concurrent_txn's writers exhausted a
// nine-attempt retry chain on their first aborted account.
//
// Memgraph does not need the exemption because its abort path UNLINKS the
// transaction's deltas from the chains it touched
// (`InMemoryStorage::InMemoryAccessor::Abort`, src/storage/v2/inmemory/storage.cpp,
// read at 572d5b4311a279de550522344a6f10d352d11c48), so no aborted delta is ever at
// a head to be tested. GoGraph keeps the version and exempts it instead; unlinking
// is rmp #2318's, and when it lands this branch becomes unreachable rather than
// wrong.
func Conflicts(headTS, startTS, txID uint64) bool {
	if headTS == 0 || headTS == AbortedTS {
		return false
	}
	return !Visible(headTS, startTS, txID)
}

// NewConflict builds the typed error for a detected conflict in store.
func NewConflict(store string, headTS, startTS, txID uint64) *Conflict {
	return &Conflict{Store: store, HeadTS: headTS, StartTS: startTS, TxID: txID}
}
