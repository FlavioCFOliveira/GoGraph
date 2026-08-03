package mvcc

// stamp.go — the write stamp (rmp #2288, made per-transaction by rmp #2301): the
// ONE commit record a transaction's versions share, allocated only if the
// transaction actually versions something.
//
// # Why it is shared across packages
//
// A transaction's changes land in two packages that cannot import each other:
// node labels and node properties in lpg, topology — and, inside the same
// immutable entry, per-slot relationship types and columnar edge properties —
// in adjlist. All of them must become visible at ONE instant, so all of them
// must point at one record, so the holder of that record cannot live in either.
//
// # Why the state is per-TRANSACTION and the stamp is only a slot
//
// Until rmp #2301 the record, the version count and the pending transaction id
// were fields ON the [WriteStamp], one set per graph, and [WriteStamp.Begin]
// said so in its own doc: "the caller must be the only writer for the window's
// duration … so Begin never overwrites a live record". Audit finding E3
// (docs/audit-mvcc-sole-cc-2026-08-02.md §6.1) is what that costs the moment two
// writers overlap, and it is not a data race — every field is atomic — it is
// SILENT DATA LOSS:
//
//	writer A  Begin  → info = armedPending(A)
//	writer B  Begin  → info = armedPending(B)      A's window is gone
//	writer A  End    → takes B's record, or nil
//
// A's versions keep A's transaction id forever. No reader can ever see them, no
// reclaimer can ever free them, and nothing reports it. Pinned by
// TestWriteStamp_TwoTransactionsDoNotShareState.
//
// So the state moved onto [TxState], one per transaction, and [WriteStamp] kept
// exactly two things: the CLOCK, and one atomic slot naming the transaction
// currently stamping. The slot is REPLACED, never mutated, so N writers each
// mutate only their own [TxState].
//
// The slot still exists because a write that carries no transaction has to
// resolve one somehow: the public Go-API mutators are per-operation atomic
// rather than transactional, and adjlist reaches the record through this type
// without being able to see lpg's transaction. A write that DOES carry its
// transaction never consults the slot — it is handed [TxState] directly — which
// is why conflict detection is threaded rather than looked up (see
// graph/lpg/mvcc_writectx.go for the false conflict that taught us the
// difference).
//
// # Why the allocation is lazy
//
// The obvious shape allocates the record when the transaction opens. That put
// one allocation on [lpg.Graph.ApplyAtomically], which had been allocation-free
// and is guarded by a test that says so — and it charged that allocation to
// applies that version nothing at all, which is every read-only bracket and
// every write that changes no value.
//
// So the record is allocated by the FIRST version that needs it and by nothing
// else. On the armed path a version pays one atomic pointer load to find a
// record that already exists; a transaction that creates no version pays one
// atomic store to arm and one to disarm, and allocates nothing.
//
// # Why a CAS rather than a mutex
//
// Two callers can reach one [TxState]: its owner, and an unsynchronised
// public-API mutator that found the same transaction through the slot. The
// compare-and-swap makes the loser lose a candidate allocation instead of losing
// a record.
//
// # Why one field carries three states
//
// The obvious shape is a bool for "armed" beside a pointer for the record, and
// it cost 4.6 ns on [lpg.Graph.ApplyAtomically] — a bracket that had been
// 9.6 ns, so a 48 % regression on an apply that versions nothing at all.
// Five atomic read-modify-writes per bracket is what that buys.
//
// Folding both into the pointer removes three of them: [armedPending] is a
// sentinel that means "a transaction is open and has not needed a record yet",
// so arming is one store, retracting is one swap, and stamping is one load on
// the path where the record already exists.
//
// It also closes a hazard that only appears once [TxState] is recycled. A
// version that reaches a transaction which has ALREADY been retracted must not
// be stamped with its record: the record's commit timestamp is in the past, so
// the version would become visible to readers whose snapshot predates the write
// — a stale snapshot observing a later write. Retract stores nil, so a late
// [TxState.Ensure] sees "no window" and its caller falls back to an untransacted
// timestamp of its own. Getting the version stamped LATER than it happened is
// safe; getting it stamped EARLIER is not.

import "sync/atomic"

// armedPending marks an open transaction that has not yet created a version.
//
// It is a distinct address and never a real commit record: nothing reads its
// timestamp, and [TxState.Ensure] replaces it with a genuine record the moment
// one is needed. Its only job is to let one atomic pointer carry the three
// states an open transaction moves through — closed, open-and-empty,
// open-with-a-record.
var armedPending = &CommitInfo{}

// TxState is ONE write transaction's stamping state: the commit record its
// versions share, and how many of them there are.
//
// It belongs to the transaction, not to the graph. Two concurrent writers hold
// two distinct values and neither can observe the other's — which is the whole
// point of rmp #2301 and what a per-graph field could not give.
//
// The zero value is not armed. Arm it with [TxState.Arm], hand it to
// [WriteStamp.Publish] so untransacted writes can find it, and close it with
// [WriteStamp.End] or [TxState.Retract].
//
// Safe for concurrent use.
type TxState struct {
	// info carries three states in one atomic word: nil for no open
	// transaction, [armedPending] for one that has created no version yet, and
	// anything else for the record its versions share. See the file comment for
	// what the alternative measured, and for why a retracted window must read as
	// "no window" rather than as its own past record.
	info atomic.Pointer[CommitInfo]
	// count is how many versions have been stamped with the current record, so
	// the owner can charge exactly that much to a reclamation budget without
	// sampling a counter before and after. It is touched only once a record
	// exists, so info and count are never inconsistent.
	count atomic.Int64
	// txID is the transaction's identity, which the record allocated by the
	// first version will carry.
	//
	// It is set by [TxState.Arm] rather than with the record because the WRITER
	// needs its own identity BEFORE it writes anything: it reads through a
	// snapshot carrying that id, and the ts == txID branch of [Visible] is what
	// lets it see its own uncommitted versions and nobody else's (rmp #2299).
	// A lazily-minted id would not exist yet at the moment the read view is
	// built, and a read view built with the wrong id sees none of its own work.
	//
	// The ALLOCATION stays lazy — that is the property the file comment defends
	// and it is untouched. Arming costs one atomic store.
	//
	// Atomic because a recycled [TxState] is armed again while an unsynchronised
	// public-API mutator may still be reading the one it found a moment ago
	// through the slot; -race is rmp #2301's acceptance instrument and a plain
	// field reports there.
	txID atomic.Uint64
}

// Arm opens st's stamping window for the transaction identified by txID, and
// reports whether it could be opened.
//
// It fails, returning false, when st still holds a record — which means it was
// retracted by nobody, or a late writer allocated one into it after its owner
// finished. Such a state must NOT be recycled: a version pointing at that
// record would publish with the wrong transaction. A caller reusing pooled
// state takes a fresh one instead.
//
// Arming allocates nothing; the record appears when the first version asks for
// it.
func (st *TxState) Arm(txID uint64) bool {
	if st.info.Load() != nil {
		return false
	}
	st.count.Store(0)
	st.txID.Store(txID)
	st.info.Store(armedPending)
	return true
}

// TxID returns the identity of the transaction currently armed on st, or zero.
func (st *TxState) TxID() uint64 { return st.txID.Load() }

// Reusable reports whether st can be armed for a new transaction — the test
// [TxState.Arm] makes, without arming.
//
// A recycling caller needs it separately because it must decide whether to keep
// a pooled value BEFORE it has a transaction id to arm with.
func (st *TxState) Reusable() bool { return st.info.Load() == nil }

// Retract closes st's window and returns the record its versions share, together
// with how many of them there are.
//
// A nil record means the transaction created no version, so there is nothing to
// publish and nothing to reclaim. The caller publishes the record — Retract
// deliberately does not, because only the caller knows whether the commit
// timestamp must be allocated before or after some other step.
func (st *TxState) Retract() (*CommitInfo, int64) {
	info := st.info.Swap(nil)
	if info == nil || info == armedPending {
		// No version was created, so no record was allocated and the counter was
		// never touched — nothing to reset and nothing to publish.
		return nil, 0
	}
	return info, st.count.Swap(0)
}

// Ensure returns the record the version being created right now must point at,
// allocating it if this is the first version of the transaction, and counts that
// version.
//
// It returns nil when st has no open window, which a caller must treat as "this
// write is not in a transaction" and stamp with a fresh commit timestamp of its
// own. See the file comment for why a retracted window may not answer with its
// own past record.
//
// Called once per version created, never on a read.
func (st *TxState) Ensure() *CommitInfo {
	info := st.info.Load()
	if info == nil {
		return nil
	}
	if info == armedPending {
		cand := NewCommitInfo(st.txID.Load())
		if st.info.CompareAndSwap(armedPending, cand) {
			info = cand
		} else if info = st.info.Load(); info == nil || info == armedPending {
			// The transaction closed underneath an unsynchronised caller, or
			// another caller is mid-CAS. Either way there is no record this
			// version may safely adopt.
			return nil
		}
	}
	st.count.Add(1)
	return info
}

// OpenRecord returns st's record WITHOUT allocating one and without counting a
// version, or nil when st has no open window or has not needed a record yet.
//
// It is [TxState.Ensure] for a caller that wants the record only as an IDENTITY
// — to ask "which transaction is writing?" rather than "give me something to
// stamp a version with". See [WriteStamp.OpenInfo].
func (st *TxState) OpenRecord() *CommitInfo {
	info := st.info.Load()
	if info == nil || info == armedPending {
		return nil
	}
	return info
}

// WriteStamp resolves how the version records a write creates are timestamped
// when the write does not carry its own transaction.
//
// Between [WriteStamp.Begin] and [WriteStamp.End] it names the open
// transaction's [TxState], and every version resolved through it takes that
// transaction's shared record. Outside that window each version takes a fresh
// commit timestamp of its own, which is the correct reading of a direct mutation
// made outside any transaction: it is committed the instant it is made.
//
// The zero value has no clock and stamps everything zero — visible to every
// reader — which is what an unversioned store wants. Attach a clock with
// [WriteStamp.SetClock].
//
// Safe for concurrent use.
type WriteStamp struct {
	clock *Clock
	// cur names the transaction currently stamping, or nil. A SLOT, not state:
	// it is replaced by [WriteStamp.Begin] and [WriteStamp.End] and never
	// mutated, so two concurrent writers cannot corrupt each other's record or
	// version count. See the file comment for what the per-graph shape lost.
	cur atomic.Pointer[TxState]
	// untracked counts versions stamped with NO transaction open — a direct
	// Go-API mutation. Those have no End to charge them to a reclamation budget,
	// and without this they accumulate forever: a caller that never opens a
	// transaction would leak one record per modification for the life of the
	// process. See [WriteStamp.TakeUntracked].
	//
	// It stays on the stamp rather than moving to [TxState] because it is
	// precisely the count of versions that belong to NO transaction, and it is
	// correct under any number of concurrent writers: an atomic counter with one
	// consumer.
	untracked atomic.Int64
}

// SetClock attaches the clock that mints commit timestamps.
//
// Must be called before any write and never concurrently with another
// operation.
//
// Not safe for concurrent use.
func (w *WriteStamp) SetClock(c *Clock) { w.clock = c }

// Clock returns the attached clock, or nil.
func (w *WriteStamp) Clock() *Clock { return w.clock }

// Publish names st as the transaction that untransacted writes resolve to.
//
// st must already be armed ([TxState.Arm]); arming and publishing are separate
// so neither has a failure mode a caller can ignore — Arm can refuse a recycled
// state, and a Publish that armed internally would either swallow that or leave
// the caller to unpick a half-opened window.
//
// It allocates nothing. The caller owns st and must close the window with
// exactly one [WriteStamp.End].
//
// Concurrent Publish calls are what rmp #2304 will create. Each writer arms its
// OWN state, so no record and no count is lost; the slot names whichever arrived
// last, and that is the only thing the slot has ever promised — a write that
// needs its own transaction must CARRY it rather than look it up.
func (w *WriteStamp) Publish(st *TxState) { w.cur.Store(st) }

// End closes the window this stamp names and returns the record the
// transaction's versions share, together with how many of them there are.
//
// See [TxState.Retract], which does the work; End additionally clears the slot,
// so an untransacted write arriving afterwards is stamped as what it is.
//
// It is correct ONLY while at most one transaction is open at a time, because it
// closes whichever transaction the slot happens to name rather than the caller's
// own. A caller that can overlap another writer must use [WriteStamp.EndFor];
// see it for what the difference costs.
func (w *WriteStamp) End() (*CommitInfo, int64) {
	st := w.cur.Swap(nil)
	if st == nil {
		return nil, 0
	}
	return st.Retract()
}

// EndFor closes the window of the transaction the CALLER owns and returns the
// record its versions share, together with how many of them there are. It clears
// the slot only if it still names st, so a writer that published later keeps its
// own window open.
//
// # Why this exists and [WriteStamp.End] is not enough
//
// End takes whichever transaction the slot names. While the visibility barrier
// admitted one writer at a time that was always the caller's own, and rmp #2301
// left it that way on purpose: the slot's contract was only ever "whichever
// arrived last". rmp #2304 lets two write brackets overlap, and then End is
// audit finding E3 in its last remaining form — the same silent loss the state
// moved onto [TxState] to prevent, one level up:
//
//	writer A  Publish(&A.tx)
//	writer B  Publish(&B.tx)          the slot now names B
//	writer A  End                     retracts B: takes B's record and count
//	writer B  End                     the slot is nil — retracts nothing
//
// A publishes B's record at A's commit timestamp, so B's writes become visible
// at the wrong instant and B's own versions keep an in-flight transaction id for
// ever: invisible to every reader, unreclaimable by every reclaimer. And, as
// with E3, every field involved is atomic, so -race is silent on it.
//
// Clearing the slot conditionally is the second half. An unconditional clear
// would leave an overlapping writer's untransacted writes — every write the
// Cypher engine makes resolves the transaction through this slot — stamped as
// though no transaction were open, so one statement's mutations would take a
// fresh timestamp each and stop being atomically visible.
//
// Pinned by TestWriteStamp_EndForClosesOnlyItsOwnTransaction.
func (w *WriteStamp) EndFor(st *TxState) (*CommitInfo, int64) {
	if st == nil {
		return nil, 0
	}
	// Retract BEFORE clearing the slot. A late untransacted write that finds st
	// through the slot in between reads a retracted window, which [TxState.Ensure]
	// reports as "no window", and falls back to a fresh timestamp of its own —
	// stamped later than it happened, which is the safe direction (see the file
	// comment). The reverse order would leave a window nobody owns.
	info, count := st.Retract()
	w.cur.CompareAndSwap(st, nil)
	return info, count
}

// Stamp returns how the version being created right now records its
// visibility: the open transaction's shared record, or nil with a fresh commit
// timestamp when no transaction is open.
//
// Called once per version created, never on a read.
func (w *WriteStamp) Stamp() (*CommitInfo, uint64) {
	if st := w.cur.Load(); st != nil {
		if info := st.Ensure(); info != nil {
			return info, 0
		}
	}
	// No transaction is open: this write is committed the instant it is made and
	// takes a timestamp of its own.
	if w.clock == nil {
		return nil, 0
	}
	w.untracked.Add(1)
	// An untransacted write is committed the instant it is made, so its
	// timestamp is published immediately: there is no record for a reader to
	// find in flight.
	ts := w.clock.NextCommitTS()
	w.clock.PublishCommitTS(ts)
	return nil, ts
}

// OpenInfo returns the record of the open transaction WITHOUT allocating one
// and without counting a version, or nil when no transaction is open or its
// record has not been allocated yet.
//
// It is [WriteStamp.Info] for a caller that wants the record only as an
// IDENTITY — to ask "which transaction is writing?" rather than "give me
// something to stamp a version with". The adjacency uses it to decide whether a
// shard's private slot-array builder belongs to the transaction now writing
// (rmp #2301); getting nil there is not a problem, it just means clone rather
// than mutate in place, which is what a transaction's first version would do
// anyway.
//
// The distinction matters because [WriteStamp.Stamp] and [WriteStamp.Info] both
// have side effects — they allocate the shared record on first use and add to
// the version count — and an identity check must have neither, or a write that
// records nothing would be charged a version and an untransacted write would be
// handed a commit timestamp it never uses.
func (w *WriteStamp) OpenInfo() *CommitInfo {
	st := w.cur.Load()
	if st == nil {
		return nil
	}
	return st.OpenRecord()
}

// OpenTxID returns the identity of the transaction currently open on this stamp,
// or zero when none is.
//
// It is [WriteStamp.OpenInfo] for a caller that wants only the transaction's
// IDENTITY and needs it to be stable from the transaction's FIRST write.
// OpenInfo cannot offer that: the commit record is allocated lazily by the first
// version that needs one, so a caller asking before then gets nil and a caller
// asking after gets a record — two different answers within one transaction.
//
// The id has no such gap. [TxState.Arm] stores it when the window opens, before
// any write can happen, which is what rmp #2299 minted it eagerly for. So a
// per-(shard, transaction) decision keyed on this is stable for the whole
// transaction, where the same decision keyed on the record is not.
//
// It allocates nothing and counts no version.
func (w *WriteStamp) OpenTxID() uint64 {
	st := w.cur.Load()
	if st == nil {
		return 0
	}
	return st.TxID()
}

// Armed reports whether a transaction is currently open on this stamp.
//
// The higher layer uses it to tell "I am inside the barrier" from "I am a bare
// mutator", because the two need different reclamation treatment: the first is
// swept when the transaction closes, the second has to arrange its own.
func (w *WriteStamp) Armed() bool { return w.cur.Load() != nil }

// TakeUntracked returns how many versions have been stamped outside any
// transaction since the last call, and resets the counter.
func (w *WriteStamp) TakeUntracked() int64 { return w.untracked.Swap(0) }

// Info returns the record of the open transaction, allocating it if this is the
// first caller to need one, or nil when no transaction is open.
//
// It is [WriteStamp.Stamp] for a caller that only ever wants the record form —
// lpg's delta chains, which have no use for an inline timestamp when a
// transaction is open.
func (w *WriteStamp) Info() *CommitInfo {
	st := w.cur.Load()
	if st == nil {
		return nil
	}
	return st.Ensure()
}
