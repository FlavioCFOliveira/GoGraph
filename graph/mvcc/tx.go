package mvcc

// tx.go — the transaction a write CARRIES (rmp #2320).
//
// # What the ambient slot could not do
//
// [WriteStamp] holds one atomic slot naming the transaction currently stamping,
// and a write that carries no transaction of its own resolves through it. That
// is correct for exactly one shape of caller — a direct, per-operation atomic
// mutation made outside any transaction — and it was correct for every caller
// while the visibility barrier admitted ONE write bracket at a time.
//
// It stops being correct the moment two brackets overlap, and the failure is not
// a data race: every field involved is atomic, so -race is silent. The slot
// names whichever writer published LAST, so a statement whose mutations resolve
// through it can have its first write stamped with its own record and its second
// with a CONCURRENT transaction's. The transaction then publishes in two pieces
// at two instants, and a snapshot reader observes half of it. Measured on the
// build that flipped the autocommit bracket to [lpg.Graph.ApplyVersioned]
// without this: 105 942 torn observations from examples/27_concurrent_txn, with
// 147 slot overwrites of a still-ARMED transaction and ZERO fallbacks to the
// untransacted branch — so the writes were not losing their record, they were
// adopting somebody else's.
//
// [WriteStamp.Publish] already stated the rule this type implements: "a write
// that needs its own transaction must CARRY it rather than look it up".
//
// # The shape, and the prior art it follows
//
// Memgraph threads the transaction as an explicit PARAMETER into the primitives
// that create and check versions — `PrepareForWrite(Transaction *transaction,
// TObj *object)` and `CreateAndLinkDelta(Transaction *transaction, TObj *object,
// Args &&...args)`, both in `src/storage/v2/mvcc.hpp` — and hands the caller an
// ACCESSOR that holds one for the duration of its work: `Transaction
// transaction_` is a member of `memgraph::storage::Accessor`
// (`src/storage/v2/storage.hpp`), which exposes the mutating API
// (`virtual VertexAccessor CreateVertex() = 0;`). Nothing in that path consults
// per-storage ambient state to discover whose write it is executing. Read at
// commit 572d5b4311a279de550522344a6f10d352d11c48 (branch master, 2026-08-03).
//
// GoGraph adopts both halves: [Tx] here is the parameter, and
// [lpg.WriteView] plus [adjlist.Writer] are the accessors that carry it.
// CreateAndLinkDelta's `transaction->EnsureCommitInfoExists()` is
// [TxState.Ensure], which is why [Tx.Record] can be a one-line forward: the
// lazy allocation of the shared record already lives on the transaction, where
// this shape needs it.

// Tx names the write transaction one write belongs to, as carried by the write
// itself.
//
// The ZERO VALUE carries no transaction, which every store must read as "this
// write is not transactional": a direct Go-API mutation, committed the instant
// it is made. That reading is the correct one rather than a concession — such a
// call is per-operation atomic by contract, has no snapshot to be stale against
// and shares no commit instant with anything else.
//
// It is ONE word and is passed by value, so threading it through a write path
// costs no allocation and no indirection beyond the one the shared record
// already requires.
//
// Safe for concurrent use; it is an immutable handle onto state that is itself
// safe for concurrent use.
type Tx struct {
	// st is the transaction's stamping state, or nil for no transaction. It is
	// the pointer rather than a copy because the record and the version count on
	// it are per-transaction MUTABLE state that every one of the transaction's
	// writes must reach — the whole point of rmp #2301.
	st *TxState
}

// NewTx returns the handle for the transaction whose stamping state is st, or
// the zero handle when st is nil.
func NewTx(st *TxState) Tx { return Tx{st: st} }

// Valid reports whether tx names a transaction.
//
// It is false for the zero value, which is what an untransacted write presents.
func (tx Tx) Valid() bool { return tx.st != nil }

// ID returns the identity of the transaction tx names, or zero.
//
// It is stable from the transaction's FIRST write, because [TxState.Arm] stores
// it when the window opens — which is what a per-(shard, transaction) decision
// such as the adjacency's builder-reuse test needs, and what the commit RECORD
// cannot offer, since that is allocated lazily by the first version.
func (tx Tx) ID() uint64 {
	if tx.st == nil {
		return 0
	}
	return tx.st.TxID()
}

// Record returns the shared commit record the version being created right now
// must point at, allocating the transaction's record if this is its first
// version and counting that version.
//
// It returns nil in two cases, which a caller must treat identically — as "this
// write is not in a transaction", stamping it with a fresh commit timestamp of
// its own:
//
//   - tx is the zero value, so there is no transaction;
//   - tx names a transaction whose window has already been retracted, which is a
//     caller retaining the handle past its bracket. Stamping with the retracted
//     record would give the version a commit timestamp in the PAST and make it
//     visible to snapshots that predate the write; a fresh timestamp is later
//     than the write actually happened, which is the safe direction. See the
//     [WriteStamp] file comment.
//
// Called once per version created, never on a read.
func (tx Tx) Record() *CommitInfo {
	if tx.st == nil {
		return nil
	}
	return tx.st.Ensure()
}
