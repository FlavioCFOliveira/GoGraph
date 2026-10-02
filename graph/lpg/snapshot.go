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
	//
	// # Why the memo is behind a pointer (rmp #2965, round 5)
	//
	// A shared snapshot's memo is guarded by a mutex, and a mutex held INSIDE the
	// snapshot makes every *Snapshot the read paths receive escape to the heap
	// (sync.Mutex's slow path leaks its receiver). The direct present-state
	// accessors read through a snapshot of their own on the caller's stack
	// ([Graph.latestCommitted]), and that stack value must stay on the stack for
	// a read to allocate nothing. So the shared memo lives behind memo, and a
	// snapshot with a nil memo is OWNED by the one goroutine that made it and pins
	// into owned instead, without a lock.
	memo *snapMemo
	// owned is the pin of an owned snapshot (memo == nil): the first len(owned)
	// in-flight records it has classified, with their verdicts. A single-object
	// read meets few in-flight records — one per transaction holding a version
	// on that object — so the array covers it. A read that walks MANY objects
	// through one owned snapshot ([Graph.TombstonedIDs] and
	// [Graph.committedLifeCounts] do) can meet more distinct in-flight
	// transactions than that, and stops pinning the excess: an unpinned record
	// is classified afresh at each visit, so one such transaction committing
	// part-way through the walk can be seen as uncommitted on objects visited
	// before its commit and as committed on objects visited after it. Those
	// walks therefore promise each object's committed state as of its visit,
	// not one instant for the whole walk.
	owned [4]pinnedVerdict
}

// snapMemo is a shared snapshot's verdict memo; see [Snapshot.memo].
type snapMemo struct {
	mu      sync.Mutex
	verdict map[*commitInfo]bool
}

// pinnedVerdict is one pinned classification of an owned snapshot.
type pinnedVerdict struct {
	info    *commitInfo
	visible bool
}

// sharedSnapshot is a [Snapshot] together with its memo, so a shared snapshot
// costs one allocation, as it did before the memo moved behind a pointer.
type sharedSnapshot struct {
	Snapshot
	m snapMemo
}

// newSharedSnapshot returns a snapshot that may be read by several goroutines
// at once.
func newSharedSnapshot(startTS, txID uint64, slot int) *Snapshot {
	p := &sharedSnapshot{Snapshot: Snapshot{startTS: startTS, txID: txID, slot: slot}}
	p.memo = &p.m
	return &p.Snapshot
}

// visible reports whether a change stamped by info — or by the raw ts when info
// is nil — is visible to this snapshot, PINNING the answer for any record this
// snapshot first classifies while it is still in flight. A committed or aborted
// record needs no pin: its stamp is final. See the verdict field.
//
// A nil snapshot, or a raw timestamp with no record, resolves straight through:
// there is nothing mutable to pin. Safe for concurrent use, because a ReadView
// may be shared.
func (s *Snapshot) visible(info *commitInfo, ts, startTS, txID uint64) bool {
	if s == nil || info == nil {
		return mvcc.Visible(ts, startTS, txID)
	}
	m := s.memo
	if m == nil {
		return s.visibleOwned(info, startTS, txID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.verdict[info]; ok {
		return v
	}
	cur := info.TS()
	v := mvcc.Visible(cur, startTS, txID)
	// Only an IN-FLIGHT record's verdict can move: committed and aborted are
	// terminal states of [mvcc.CommitInfo], whose stamp is never written again,
	// so re-deciding such a record gives the answer pinned here every time.
	// Memoising it only grew the map — once per distinct committed transaction a
	// read touched, which a 4000-slot run resolved per statement made the
	// dominant cost of typing it (rmp #2888). The in-flight case, the one the
	// pin exists for, is recorded exactly as before.
	if cur >= mvcc.TxIDBase && cur != mvcc.AbortedTS {
		if m.verdict == nil {
			m.verdict = make(map[*commitInfo]bool, 4)
		}
		m.verdict[info] = v
	}
	return v
}

// visibleOwned is [Snapshot.visible] for an owned snapshot: the same pin, kept
// in the inline array and taken without a lock, because only the goroutine that
// made the snapshot reads through it.
func (s *Snapshot) visibleOwned(info *commitInfo, startTS, txID uint64) bool {
	for i := range s.owned {
		p := &s.owned[i]
		if p.info == nil {
			break
		}
		if p.info == info {
			return p.visible
		}
	}
	cur := info.TS()
	v := mvcc.Visible(cur, startTS, txID)
	if cur >= mvcc.TxIDBase && cur != mvcc.AbortedTS {
		for i := range s.owned {
			if s.owned[i].info == nil {
				s.owned[i] = pinnedVerdict{info: info, visible: v}
				break
			}
		}
	}
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
	return newSharedSnapshot(startTS, 0, slot)
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

// latestCommitted prepares cs as the read position of a direct present-state
// accessor — [Graph.GetNodeProperty], [Graph.HasNodeLabel], [Graph.NodeLabels]
// and every other accessor that takes no snapshot — and returns it, or nil when
// the versioning substrate is disarmed and the stored value is the only state.
//
// # Committed only (rmp #2965, round 5, finding R5-F3)
//
// A version a transaction has written and not yet published is invisible to
// every other reader. Before this, the direct accessors read the stored value,
// which carries every such version: a durable commit applied but not yet fsynced
// was readable through GetNodeProperty, HasNodeLabel and AdjList().HasEdge, and
// if the fsync then failed the commit was withdrawn — the reader had seen a
// write that never happened. The accessors now resolve as an implicit
// transaction reads: every committed version is visible and every uncommitted
// one is stepped back over, to the pre-image it replaced.
//
// The position needs no horizon slot: the versions it steps over are
// uncommitted, which no reclaimer frees, and it stops at the first committed
// one. cs is the caller's stack value, so a read whose newest version is
// committed — the common case — allocates nothing; the snapshot's verdict memo
// pins any in-flight transaction it classifies, so a transaction that commits
// mid-read is seen wholly or not at all.
//
// This is how Memgraph's accessors resolve a read: ApplyDeltasForRead walks an
// object's delta chain from the newest version back, undoing every delta the
// reading transaction may not see, and stops at the first one it may
// (memgraph/memgraph commit 3f2d6f8ed27ef6610933a218403f05f7a51a4d81;
// src/storage/v2/mvcc.hpp). PostgreSQL's HeapTupleSatisfiesMVCC likewise
// treats a tuple whose inserting transaction is still in progress as invisible
// to every other backend (postgres/postgres commit
// 50d6e533e4d9a0f70d798c534254007c83c0d428;
// src/backend/access/heap/heapam_visibility.c). A transaction reading its own
// writes does so through its own view ([Graph.WriterViewOf], [Graph.Writer]).
func (g *Graph[N, W]) latestCommitted(cs *Snapshot) *Snapshot {
	if !g.mvccArmed {
		return nil
	}
	cs.startTS = implicitStartTS
	return cs
}
