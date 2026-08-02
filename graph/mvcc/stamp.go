package mvcc

// stamp.go — the write stamp (rmp #2288): the ONE commit record a transaction's
// versions share, allocated only if the transaction actually versions
// something.
//
// # Why it is shared across packages
//
// A transaction's changes land in two packages that cannot import each other:
// node labels and node properties in lpg, topology — and, inside the same
// immutable entry, per-slot relationship types and columnar edge properties —
// in adjlist. All of them must become visible at ONE instant, so all of them
// must point at one record, so the holder of that record cannot live in either.
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
// else. On the armed path a version pays one atomic bool load plus one atomic
// pointer load to find a record that already exists; a transaction that creates
// no version pays one atomic store to arm and one to disarm, and allocates
// nothing.
//
// # Why a CAS rather than a mutex
//
// Under the barrier there is exactly one writer, so the compare-and-swap always
// succeeds on the first attempt and the loser branch is unreachable. It is
// written anyway because the public Go-API mutators are per-operation atomic
// rather than transactional, so an unsynchronised caller can reach [Stamp]
// while a transaction holds the barrier; the CAS makes that lose a candidate
// allocation instead of losing a record.
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
// so Begin is one store, End is one swap, and Stamp is one load on the path
// where the record already exists.

import "sync/atomic"

// armedPending marks an open transaction that has not yet created a version.
//
// It is a distinct address and never a real commit record: nothing reads its
// timestamp, and [WriteStamp.Stamp] replaces it with a genuine record the
// moment one is needed. Its only job is to let one atomic pointer carry the
// three states an open transaction moves through — closed, open-and-empty,
// open-with-a-record.
var armedPending = &CommitInfo{}

// WriteStamp resolves how the version records a write creates are timestamped.
//
// Armed between [WriteStamp.Begin] and [WriteStamp.End], every version takes the
// transaction's shared record. Outside that window each version takes a fresh
// commit timestamp of its own, which is the correct reading of a direct
// mutation made outside any transaction: it is committed the instant it is
// made.
//
// The zero value has no clock and stamps everything zero — visible to every
// reader — which is what an unversioned store wants. Attach a clock with
// [WriteStamp.SetClock].
//
// Safe for concurrent use.
type WriteStamp struct {
	clock *Clock
	// info carries three states in one atomic word: nil for no open
	// transaction, [armedPending] for one that has created no version yet, and
	// anything else for the record its versions share. See the file comment for
	// what the alternative measured.
	info atomic.Pointer[CommitInfo]
	// count is how many versions have been stamped with the current record, so
	// the caller can charge exactly that much to a reclamation budget without
	// sampling a counter before and after. It is touched only once a record
	// exists, so info and count are never inconsistent.
	count atomic.Int64
	// untracked counts versions stamped with NO transaction open — a direct
	// Go-API mutation. Those have no End to charge them to a reclamation
	// budget, and without this they accumulate forever: a caller that never
	// opens a transaction would leak one record per modification for the life
	// of the process. See [WriteStamp.TakeUntracked].
	untracked atomic.Int64
	// pendingTxID is the transaction id [WriteStamp.Begin] minted for the open
	// window, which the record allocated by the first version will carry.
	//
	// It is minted at Begin rather than with the record because the WRITER
	// needs its own identity BEFORE it writes anything: it reads through a
	// snapshot carrying that id, and the ts == txID branch of [Visible] is what
	// lets it see its own uncommitted versions and nobody else's (rmp #2299).
	// A lazily-minted id would not exist yet at the moment the read view is
	// built, and a read view built with the wrong id sees none of its own work.
	//
	// The ALLOCATION stays lazy — that is the property the file comment above
	// defends and it is untouched. What Begin now costs is one atomic add.
	pendingTxID atomic.Uint64
}

// SetClock attaches the clock that mints transaction ids and commit
// timestamps.
//
// Must be called before any write and never concurrently with another
// operation.
//
// Not safe for concurrent use.
func (w *WriteStamp) SetClock(c *Clock) { w.clock = c }

// Clock returns the attached clock, or nil.
func (w *WriteStamp) Clock() *Clock { return w.clock }

// Begin opens a transaction's stamping window and returns the transaction id
// every version stamped in it will carry. It allocates nothing; the record
// appears when the first version asks for it.
//
// The returned id is what the writer reads through: a snapshot carrying it sees
// the transaction's own uncommitted versions, through the ts == txID branch of
// [Visible], and no other transaction's (rmp #2299). It is zero when no clock
// is attached, which is the unversioned case where nothing reads a timestamp
// back.
//
// The caller must be the only writer for the window's duration — the higher
// layer's exclusive barrier supplies that — so Begin never overwrites a live
// record.
func (w *WriteStamp) Begin() uint64 {
	var id uint64
	if w.clock != nil {
		id = w.clock.NextTxID()
	}
	w.pendingTxID.Store(id)
	w.info.Store(armedPending)
	return id
}

// End closes the window and returns the record the transaction's versions
// share, together with how many of them there are.
//
// A nil record means the transaction created no version, so there is nothing to
// publish and nothing to reclaim. The caller publishes the record — End
// deliberately does not, because only the caller knows whether the commit
// timestamp must be allocated before or after some other step.
func (w *WriteStamp) End() (*CommitInfo, int64) {
	info := w.info.Swap(nil)
	if info == nil || info == armedPending {
		// No version was created, so no record was allocated and the counter
		// was never touched — nothing to reset and nothing to publish.
		return nil, 0
	}
	return info, w.count.Swap(0)
}

// Stamp returns how the version being created right now records its
// visibility: the transaction's shared record, or nil with a fresh commit
// timestamp when no transaction is open.
//
// Called once per version created, never on a read.
func (w *WriteStamp) Stamp() (*CommitInfo, uint64) {
	info := w.info.Load()
	if info == nil {
		// No transaction is open: this write is committed the instant it is
		// made and takes a timestamp of its own.
		if w.clock == nil {
			return nil, 0
		}
		w.untracked.Add(1)
		// An untransacted write is committed the instant it is made, so its
		// timestamp is published immediately: there is no record for a reader
		// to find in flight.
		ts := w.clock.NextCommitTS()
		w.clock.PublishCommitTS(ts)
		return nil, ts
	}
	if info == armedPending {
		if w.clock == nil {
			return nil, 0
		}
		// The id was minted by Begin, so the record the first version
		// allocates carries the same id the writer is reading through. Minting
		// a fresh one here would give the writer a record it cannot see its
		// own writes through.
		id := w.pendingTxID.Load()
		if id == 0 {
			// No open window minted one — an unsynchronised caller reached
			// Stamp between Begin and its store, or the stamp was armed by a
			// path that predates rmp #2299. Fall back rather than stamp a
			// record with the zero id, which Visible would treat as a commit
			// timestamp of zero and show to every reader.
			id = w.clock.NextTxID()
		}
		cand := NewCommitInfo(id)
		if w.info.CompareAndSwap(armedPending, cand) {
			info = cand
		} else if info = w.info.Load(); info == nil || info == armedPending {
			// The transaction closed underneath an unsynchronised caller. Treat
			// the write as untransacted rather than stamping it with a record
			// nobody will publish.
			ts := w.clock.NextCommitTS()
			w.clock.PublishCommitTS(ts)
			return nil, ts
		}
	}
	w.count.Add(1)
	return info, 0
}

// Armed reports whether a transaction is currently open on this stamp.
//
// The higher layer uses it to tell "I am inside the barrier" from "I am a bare
// mutator", because the two need different reclamation treatment: the first is
// swept when the transaction closes, the second has to arrange its own.
func (w *WriteStamp) Armed() bool { return w.info.Load() != nil }

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
	if w.info.Load() == nil {
		return nil
	}
	info, _ := w.Stamp()
	return info
}
