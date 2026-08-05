package mvcc

// counters.go — the write-side MVCC series, counted without a shared cache line
// (rmp #2312).
//
// # What is being counted, and why it belongs here
//
// Once MVCC is the module's only concurrency control, its health IS the module's
// health: how many writers are in flight, how many transactions commit, how many
// abort, and how many are refused for a serialization conflict. The first four are
// what an operator divides into each other — a conflict rate is conflicts over
// commits, and neither number is useful alone.
//
// They live in this package rather than in [lpg] because the quantities are
// properties of the versioning substrate, and because the striping below has to
// sit beside [cacheLine], which the horizon already defines for the same reason.
//
// # Why striped, and what the stripe costs
//
// A commit already pays several atomic read-modify-writes on shared lines — the
// clock's counter, the commit log's frontier, the reclamation debt — and this
// project has measured what one more contended line does to it: rmp #2203 measured
// a bare RWMutex degrading 17.6x from 1 to 10 cores in this codebase, and rmp #2322
// measured a mutex BEATING a lock-free CAS by 1.42x under contention. Adding three
// more shared counters to the commit path to observe the commit path would be
// measuring the observer.
//
// So the counters are striped over [counterStripes] cache-line-isolated banks and
// summed only when someone asks. The stripe index is the transaction's OWN id
// masked to the bank count, which costs one AND on a value the caller already holds
// — no goroutine-id parse, no procPin, no rotating counter to contend on. Ids are
// allocated monotonically, so N concurrent writers hold N consecutive ids and land
// on N distinct stripes: better spreading than a hash would give, for less work.
//
// The overhead is measured, not asserted: see docs/benchmarks/mvcc-observability-2026-08-05.md.
//
// # What a striped sum is, and is not
//
// [WriteCounters.Load] reads the stripes one at a time, so the result is not a
// consistent snapshot of an instant: a transaction that commits during the sum may
// or may not be in it. That is the correct trade for telemetry — a gauge scraped
// once a second does not need linearisability, and buying it would put the
// contention back that the striping removed.
//
// It IS exact when the graph is quiescent, which is what a test asserts against.
// [WriteCounters.Writers] cannot go negative even mid-flight, because a
// transaction's begin and end carry the same id and therefore hit the same stripe.

import "sync/atomic"

// counterStripes is how many cache-line-isolated banks the write counters are
// spread over. A power of two so the index is a mask.
//
// 64 covers every concurrency level this module load-tests to (its grid runs to
// 1024 goroutines, but a stripe collision costs a shared line only while two
// writers are inside their own commit at the same instant, and 64 is already 6x
// the core count of the reference machine). It costs 8 KiB per Graph — 6% of what
// the horizon's 1024 padded slots already cost — and [WriteCounters.Load] touches
// all of it, which is why the sum is not on any request path.
const counterStripes = 64

// writeStripe is one bank of write-side counters, alone on its cache line.
//
// The three counters share the line ON PURPOSE: a commit touches commits and
// writers, an abort touches aborts and writers, so one line per transaction is one
// coherence miss per transaction rather than two.
type writeStripe struct {
	commits atomic.Uint64
	aborts  atomic.Uint64
	// writers is a GAUGE, so it is signed and it goes down. It never goes
	// negative on a stripe: a transaction's [WriteCounters.BeginWriter] and
	// [WriteCounters.EndWriter] carry the same id and so address the same stripe.
	writers atomic.Int64
	_       [cacheLine - 24]byte
}

// WriteCounts is a point-in-time reading of [WriteCounters].
type WriteCounts struct {
	// Writers is how many write transactions are in flight.
	Writers int64
	// Commits and Aborts are the two OUTCOMES, and they partition the transactions
	// the substrate reached a decision about: Commits published an instant, Aborts
	// were refused publication. Cumulative and never reset, so two observers cannot
	// take them from each other.
	//
	// A transaction that versioned nothing and hit no conflict is in NEITHER, and
	// that is not an omission: it published no instant, so counting it as a commit
	// would put commits above the number of instants the clock ever allocated, and
	// it refused nothing, so counting it as an abort would invent a failure. There
	// was no decision to record.
	Commits uint64
	Aborts  uint64
	// Conflicts is how many write transactions were refused for a write-write
	// conflict, and ByStore attributes them to the store the first refusal came
	// from. Indexed by [ConflictStoreIndex].
	//
	// It is a CAUSE, not an outcome, so it is a SUBSET of Aborts rather than a third
	// bucket beside them — the same relationship PostgreSQL's pg_stat_database has
	// between xact_rollback and its conflict counters, where a transaction killed by
	// a conflict appears in both. Aborts says how many transactions failed;
	// Conflicts says how many of them failed for this reason.
	//
	// One per DOOMED TRANSACTION, not one per refused write: a doomed transaction
	// meets its conflict again on every write it still attempts, and a count that
	// scaled with transaction size could not be divided by Commits.
	Conflicts uint64
	ByStore   [ConflictStoreCount]uint64
}

// ConflictRate returns conflicts as a fraction of the transactions that reached an
// outcome, or zero when none has.
//
// The denominator is Commits+Aborts and NOT Commits alone: a workload in which every
// transaction conflicts would otherwise divide by zero and report no contention at
// all. Conflicts is not added to it, because a conflicting transaction is already
// counted in Aborts and adding it would deflate its own rate.
func (c *WriteCounts) ConflictRate() float64 {
	decided := c.Commits + c.Aborts
	if decided == 0 {
		return 0
	}
	return float64(c.Conflicts) / float64(decided)
}

// WriteCounters counts write-transaction outcomes without a shared cache line.
//
// The zero value is ready to use. Safe for concurrent use.
type WriteCounters struct {
	stripes [counterStripes]writeStripe
	// conflicts and byStore are NOT striped, because a conflict is exceptional by
	// contract: a workload in which conflicts are frequent enough for a shared line
	// to matter has a contention problem that this counter is there to report, not
	// to participate in.
	conflicts atomic.Uint64
	byStore   [ConflictStoreCount]atomic.Uint64
}

// stripe returns the bank transaction txID accounts to.
func (c *WriteCounters) stripe(txID uint64) *writeStripe {
	return &c.stripes[txID&(counterStripes-1)]
}

// BeginWriter records that the transaction identified by txID has opened.
func (c *WriteCounters) BeginWriter(txID uint64) { c.stripe(txID).writers.Add(1) }

// EndWriter records that the transaction identified by txID has closed, whatever
// its outcome. It must be called exactly once for each [WriteCounters.BeginWriter]
// and with the same id, or the writer gauge drifts.
func (c *WriteCounters) EndWriter(txID uint64) { c.stripe(txID).writers.Add(-1) }

// Commit records that the transaction identified by txID published its versions.
func (c *WriteCounters) Commit(txID uint64) { c.stripe(txID).commits.Add(1) }

// Abort records that the transaction identified by txID was refused publication.
//
// It must be called for EVERY refused transaction, including one that versioned
// nothing before it was doomed: Commits and Aborts partition the outcomes, and a
// failure the substrate does not count is a failure an operator cannot see.
func (c *WriteCounters) Abort(txID uint64) { c.stripe(txID).aborts.Add(1) }

// Conflict records one doomed transaction, attributed to store.
//
// It takes the store's INDEX rather than its name so the caller pays the table
// scan once and this function pays nothing; see [ConflictStoreIndex].
func (c *WriteCounters) Conflict(storeIdx int) {
	c.conflicts.Add(1)
	c.byStore[storeIdx].Add(1)
}

// Load sums every stripe and returns the current reading.
//
// It touches all [counterStripes] cache lines, so it belongs off every request
// path — the vacuum's metrics publication and an explicit stats call are its only
// callers. See the file comment for what a striped sum guarantees.
//
// Safe for concurrent use.
func (c *WriteCounters) Load() WriteCounts {
	var w WriteCounts
	for i := range c.stripes {
		s := &c.stripes[i]
		w.Commits += s.commits.Load()
		w.Aborts += s.aborts.Load()
		w.Writers += s.writers.Load()
	}
	w.Conflicts = c.conflicts.Load()
	for i := range c.byStore {
		w.ByStore[i] = c.byStore[i].Load()
	}
	return w
}

// Writers returns just the in-flight write-transaction count.
//
// Separate from [WriteCounters.Load] because it is the one field a caller may
// legitimately want on its own — a shutdown path checking that nothing is still
// writing — and it reads a third of the lines.
//
// Safe for concurrent use.
func (c *WriteCounters) Writers() int64 {
	var n int64
	for i := range c.stripes {
		n += c.stripes[i].writers.Load()
	}
	return n
}
