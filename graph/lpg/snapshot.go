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
