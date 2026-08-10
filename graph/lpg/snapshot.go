package lpg

// snapshot.go — the read view (rmp #2289, MVCC P4b), introduced here because
// P3c's versioned accessors need something to be "as of".
//
// # What a Snapshot is
//
// Two timestamps and a horizon slot. The start timestamp decides what the read
// can see; the transaction id lets a writer see its OWN uncommitted work; the
// slot is how reclamation knows the reader is still there.
//
// It is deliberately NOT generic over the graph's node and weight types,
// because it holds no graph state — only the instant. One consequence is worth
// stating: a Snapshot taken from one graph and passed to another is
// meaningless, because the two have unrelated clocks. Callers hold exactly one
// graph, and the alternative (a type parameter, or a graph pointer that would
// need an interface to erase) buys nothing they would use.
//
// # nil means "the current value"
//
// Every versioned accessor takes a *Snapshot, and nil means "read the stored
// value with no version walk". That is not a convenience default — it is the
// ONLY correct answer for a writer inside the barrier, which must see its own
// eagerly-applied work including the parts it has not yet published. Making the
// plain accessors delegate with nil is what keeps one implementation per
// accessor instead of two that can drift.

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// Snapshot is a consistent read view of the graph at one instant.
//
// Obtain one with [Graph.BeginRead] and release it with [Graph.EndRead],
// exactly once, on every path including error and panic ones — a snapshot that
// is never released holds the reclamation watermark and versions accumulate
// behind it.
//
// A nil *Snapshot passed to a versioned accessor means "the current stored
// value", which is what a writer inside the visibility barrier needs.
//
// Safe for concurrent use by readers: it is immutable once returned.
type Snapshot struct {
	// startTS is the instant this read observes. A change committed at or
	// before it is visible; anything later is not.
	startTS uint64
	// txID is the reading transaction's own id, so a writer that reads inside
	// its own transaction sees its own uncommitted changes. Zero for a
	// read-only snapshot, which matches no transaction id the clock can mint.
	txID uint64
	// slot is the horizon slot this reader occupies, returned to
	// [Graph.EndRead].
	slot int
	// verdict PINS this snapshot's visibility answer for each commit record it has
	// already classified (rmp #2378).
	//
	// # The defect this closes
	//
	// [mvcc.Visible] is evaluated separately for every substructure a read
	// touches, against [mvcc.CommitInfo.TS] — a field that is MUTABLE and flips
	// when the transaction commits. A reader that straddles a commit therefore
	// classified one substructure before the flip and the next after it, and
	// observed a state no serial order produced: an edge and its two endpoint
	// labels, written by ONE transaction, seen partially applied through a pinned
	// snapshot. Measured at 2-5 failures per 100 runs of
	// [TestIsolation_CrossSubstructure_EdgeImpliesLabels] under the gate's own
	// parallel-package load, on BOTH the exclusive bare-API bracket and the
	// engine's [Graph.ApplyVersioned].
	//
	// It dates from rmp #2344 (`5a71cc1c`), which removed Graph.View. Until then a
	// reader HELD the visibility barrier, so its correlated reads were atomic by
	// construction and the tear could not occur. Nothing replaced that property;
	// the snapshot resolves each read as-of, but nothing tied the reads together.
	//
	// # Why memoising the verdict is exactly the reference shape
	//
	// PostgreSQL's snapshot is xmin plus the LIST of in-progress XIDs
	// (GetSnapshotData), and InnoDB's read view is the same: both decide visibility
	// from state captured ONCE. Pinning the verdict per record gets that lazily —
	//
	//   - a record IN FLIGHT when first classified stays invisible for this
	//     snapshot's lifetime, which is precisely the in-progress-list rule;
	//   - a record committed at or below startTS is visible, and its ts is
	//     immutable thereafter, so the memo changes nothing;
	//   - a record committed above startTS is invisible, likewise immutable.
	//
	// So exactly one case changes, and it changes to the answer the snapshot should
	// always have given. Lazy is safe: a record committing between [Graph.BeginRead]
	// and its first classification can only have done so ABOVE startTS, because the
	// contiguous frontier never advances past an unfinished commit.
	//
	// # Both halves are required
	//
	// Pinning the verdict alone does NOT fix it (measured 2/100), because
	// AdjList.entryAsOfLoaded short-circuits on a GLOBAL versionActive counter and
	// never consults the verdict on those reads. Dropping that counter alone does
	// not fix it either (measured 3/100), because the verdict still moves mid-read.
	// Together: 0 failures in 300 runs.
	mu      sync.Mutex
	verdict map[*commitInfo]bool
}

// visible reports whether a change stamped by info — or by the raw ts when info
// is nil — is visible to this snapshot, PINNING the answer for any record this
// snapshot classifies more than once. See the verdict field.
//
// A nil snapshot, or a raw timestamp with no record, resolves straight through:
// there is nothing mutable to pin. Safe for concurrent use, because a ReadView
// may be shared.
func (s *Snapshot) visible(info *commitInfo, ts, startTS, txID uint64) bool {
	if s == nil || info == nil {
		return mvcc.Visible(ts, startTS, txID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.verdict[info]; ok {
		return v
	}
	v := mvcc.Visible(info.TS(), startTS, txID)
	if s.verdict == nil {
		s.verdict = make(map[*commitInfo]bool, 4)
	}
	s.verdict[info] = v
	return v
}

// StartTS returns the instant this snapshot observes.
//
// Exported so a caller in another package can carry the timestamp into a
// component that takes it directly, and so a test can assert what a read
// pinned.
func (s *Snapshot) StartTS() uint64 { return s.startTS }

// TxID returns the reading transaction's id, or zero for a read-only snapshot.
func (s *Snapshot) TxID() uint64 { return s.txID }

// BeginRead opens a read view at the current instant and registers it with the
// reclamation horizon, so no version this read can still reach is freed while
// it runs.
//
// The caller MUST pass the result to [Graph.EndRead] exactly once. Failing to
// do so holds the watermark for the life of the process.
//
// It returns nil when versioning is disarmed, which is the correct "read the
// current value" snapshot and costs nothing.
//
// Safe for concurrent use.
func (g *Graph[N, W]) BeginRead() *Snapshot {
	if !g.mvccArmed {
		return nil
	}
	// Register BEFORE reading the clock. The reverse order leaves a window in
	// which a reclaimer computes a watermark newer than this reader's start
	// timestamp and frees versions it is about to need; see
	// [mvcc.Horizon.EnterHolding].
	slot := g.horizon.EnterHolding()
	startTS := g.mvccClock.ReadTS()
	g.horizon.Publish(slot, startTS)
	return &Snapshot{startTS: startTS, slot: slot}
}

// EndRead releases a read view obtained from [Graph.BeginRead].
//
// It tolerates a nil snapshot, so a caller can defer it unconditionally.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EndRead(s *Snapshot) {
	if s == nil {
		return
	}
	g.horizon.Leave(s.slot)
	// The DRAIN wake (rmp #2308). A reader's departure is the one way the
	// reclamation watermark advances without anything being written, so nothing
	// else would tell the vacuum that the versions this reader was pinning are now
	// free. It replaces the read-path sweep [Graph.ReclaimIdle] used to perform
	// inline: same trigger, same throttle, but the work now happens on the
	// vacuum's goroutine instead of on the query's.
	g.wakeVacuumOnRelease()
}

// snapshotTimes unpacks a snapshot into the pair every versioned store's walk
// takes, and reports whether a walk is wanted at all.
//
// A nil snapshot means "the current stored value", so it reports false and the
// caller returns what it read without touching a chain.
func snapshotTimes(s *Snapshot) (startTS, txID uint64, walk bool) {
	if s == nil {
		return 0, 0, false
	}
	return s.startTS, s.txID, true
}
