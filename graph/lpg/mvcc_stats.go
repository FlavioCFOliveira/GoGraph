package lpg

// mvcc_stats.go — MVCC P6c (rmp #2286): the bound on version memory, and the
// telemetry that makes it observable.
//
// # Why a bound is owed at all
//
// The module's bounded-resources mandate says every cache, pool and queue
// declares an explicit upper bound and exports its utilisation. Version records
// are a cache in every respect that matters: they are created by writes,
// consumed by readers, and released by a sweep. Without a stated bound the
// honest description of them is "unbounded", which the mandate does not admit.
//
// # The bound, stated
//
// Between sweeps the substrate holds at most [reclaimThreshold] records ABOVE
// whatever the oldest active reader is legitimately holding back. Two
// quantities, and both are reported here:
//
//   - the CHURN component is bounded by construction. A write charges its
//     versions to a debt and the sweep runs once the debt passes the threshold,
//     so the write path cannot outrun it.
//   - the READER component is not bounded by this package, and cannot be: a
//     reader that has begun is entitled to the versions it can still reach, and
//     a reader that never finishes holds them forever. That is the same
//     contract PostgreSQL has with a long transaction and VACUUM, and Memgraph
//     with its oldest active timestamp. What this package owes is that the cost
//     be ATTRIBUTABLE — [MVCCStats.OldestSnapshotAge],
//     [MVCCStats.ActiveSnapshots] and [MVCCStats.ActiveReaders] say who is
//     responsible, and [MVCCStats.UnregisteredSnapshots] says when reclamation has
//     stopped altogether.
//
// # The write side (rmp #2312)
//
// Everything above describes what versioning RETAINS. Once MVCC is the module's
// only concurrency control, an operator also needs to see what PRODUCES it and
// whether it is working: how many writers are in flight, how many transactions
// commit, how many abort, how many are refused for a serialization conflict and in
// which store, and how deep the chains a read must walk actually are. Those are
// [MVCCStats.Write] and [MVCCStats.ChainDepth]; the vacuum's own half is
// [VacuumStats].
//
// # Why there is no back-pressure
//
// The task asked that back-pressure be considered when the bound is approached.
// It is deliberately not implemented, and the reason is that there is nothing
// sound to push back ON. The only way to shed version memory is to make an
// active reader's snapshot unreadable, which is a wrong answer rather than a
// slow one; refusing WRITES would not help either, because the memory is held
// by a reader that has already started and refusing new work does not release
// it. So the module reports the condition instead of pretending it can be
// throttled — which is also what both reference implementations do.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// MVCCStats is a point-in-time picture of what the versioning substrate is
// holding and why.
//
// Every field is read with a plain atomic load, so obtaining it costs nothing
// and disturbs no reader.
type MVCCStats struct {
	// LabelDeltas, PropDeltas, AdjVersions, EdgeSideVersions and NodeLifeRecords
	// are the live record counts per store.
	LabelDeltas      int64
	PropDeltas       int64
	AdjVersions      int64
	EdgeSideVersions int64
	NodeLifeRecords  int64
	// IndexRemovalBacklog is the number of label-index removals waiting for the
	// watermark. They are memory too, and they are the reason a label bitmap can
	// over-report.
	IndexRemovalBacklog int64
	// AdjConflictStamps is the number of nodes carrying an adjacency write-write
	// conflict stamp ([adjVersions]).
	//
	// It is reported separately from Total rather than added to it, because it is
	// not a version: it holds no pre-image, is never read, and takes no part in
	// rollback or in a reader's visibility decision. It is write-side bookkeeping
	// with the same LIFETIME as a version — bounded by the same watermark in the
	// same sweep — so it belongs here to be observable, and folding it into Total
	// would misreport the version memory a reader can hold back.
	AdjConflictStamps int64
	// Total is the sum of every VERSION count above: the memory the substrate is
	// responsible for.
	Total int64
	// Bound is the number of records that may accumulate from CHURN between
	// sweeps in the SETTLED state — that is, the threshold at which the vacuum is
	// woken. Total above it means either a reader is holding versions back, which
	// is legitimate and is what the two fields below explain, or the vacuum has
	// not yet caught up with a burst, which [MVCCStats.Ceiling] bounds.
	Bound int64
	// Ceiling is the instantaneous bound: the debt at which a committer stops
	// merely signalling the vacuum and waits for it.
	//
	// It exists because the sweep is ASYNCHRONOUS (rmp #2308). Bound alone was a
	// true instantaneous bound only while the committer swept before returning; a
	// background sweeper can be outrun, so the module states both numbers — the
	// one churn settles to, and the one it can never exceed for longer than a
	// pass. See [reclaimDebtCeiling].
	Ceiling int64
	// Watermark is the oldest start timestamp among active readers, or zero when
	// reclamation is suspended.
	Watermark uint64
	// Now is the clock's current published instant, so Now-Watermark is how far
	// behind the oldest reader is.
	Now uint64
	// ActiveSnapshots is how many SNAPSHOTS are registered with the horizon.
	//
	// Readers AND writers, and the name says so (rmp #2312). It was ActiveReaders,
	// which was accurate only until rmp #2299 gave a writer a snapshot of its own
	// and registered it with the same horizon — after which a graph with one writer
	// and no reader reported one "reader". The split an operator actually needs is
	// Write.Writers against [MVCCStats.ActiveReaders], both derived from here.
	ActiveSnapshots int
	// UnregisteredSnapshots is how many active readers or writers could not get a
	// horizon slot. While it is non-zero the watermark is zero and NOTHING is
	// reclaimed — the one state in which version memory genuinely has no bound,
	// and the reason this field exists rather than being inferred from
	// unexplained growth.
	UnregisteredSnapshots int64
	// SnapshotCapacity is how many snapshots may be registered at once. Past it
	// reclamation SUSPENDS rather than slowing down, so the utilisation
	// ActiveSnapshots/SnapshotCapacity is the number to alert on.
	SnapshotCapacity int
	// Write is the write side of the substrate: writers in flight, commits,
	// aborts and serialization conflicts by store (rmp #2312).
	//
	// It is the half MVCCStats did not have. Every field above predates
	// multi-writer and describes what versioning RETAINS; these describe what
	// produces it, and the conflict rate is the signal that tells an operator
	// whether their workload is contending at all. See [mvcc.WriteCounters] for
	// why the counters are striped and what a striped sum guarantees.
	Write mvcc.WriteCounts
	// ChainDepth is the distribution of RETAINED version-chain depth: how deep a
	// chain a read arriving now may have to walk, per object.
	//
	// A distribution rather than a mean, because chain depth IS read cost and the
	// quantity that matters is the tail — one object with a chain of 200 is a
	// latency spike a mean over a million short chains reports as 1.0002. See
	// [mvcc.DepthHist] for the bucketing, and for why the reading describes each
	// store's most recent complete sweep rather than one instant.
	ChainDepth mvcc.Depths
	// InFlightCommits is how many commit timestamps have been allocated but
	// have not finished: the distance between the instant a reader starts at
	// and the newest timestamp handed out.
	//
	// It is the quantity to look at when readers appear stale, because the
	// frontier is CONTIGUOUS — one commit stuck between allocation and
	// publication holds it for every reader, however many later commits have
	// already published (rmp #2298). It is also what the commit log retains, so
	// a value that does not return to zero is both the staleness and the memory
	// growth, named once.
	InFlightCommits uint64
}

// OldestSnapshotAge returns how far behind the current instant the oldest active
// snapshot is, in commit timestamps.
//
// It is the quantity to look at when [MVCCStats.Total] exceeds
// [MVCCStats.Bound]: a large value names a long-running transaction as the cause,
// and [MVCCStats.ActiveReaders] against [MVCCStats.Write] says whether it is a
// reader or a writer.
//
// # It is also the watermark age, and there is only one series for both
//
// [MVCCStats.Watermark] IS the oldest active snapshot's start timestamp, so its
// age and the oldest snapshot's age are the same number. They are published once
// (rmp #2312). Publishing them twice under two names would give an operator two
// names for one quantity, which this module has already had to correct once — see
// the note in [Graph.publishVacuumMetrics] on the reader count.
func (s *MVCCStats) OldestSnapshotAge() uint64 {
	if s.Watermark == 0 || s.Now < s.Watermark {
		return 0
	}
	return s.Now - s.Watermark
}

// ActiveReaders returns how many of the registered snapshots belong to READERS
// rather than to write transactions.
//
// Derived rather than counted, because the horizon does not distinguish them and
// giving it a second counter to do so would put a write on the registration path
// that every read also takes. It can read one low under concurrency — the two
// quantities are sampled a few nanoseconds apart — and is clamped at zero rather
// than reported negative.
func (s *MVCCStats) ActiveReaders() int {
	n := s.ActiveSnapshots - int(s.Write.Writers)
	if n < 0 {
		return 0
	}
	return n
}

// WithinBound reports whether version memory is at or below the SETTLED churn
// bound — that is, whether nothing is being held back by a reader and the vacuum
// has caught up.
//
// Because the sweep is asynchronous it is a property of the settled state, not an
// invariant of every instant: a caller sampling it in the middle of a write burst
// should expect it to be false and [MVCCStats.WithinCeiling] to be true.
func (s *MVCCStats) WithinBound() bool { return s.Total <= s.Bound }

// WithinCeiling reports whether version memory is at or below the instantaneous
// bound, counting what a reader is legitimately holding back.
//
// Unlike [MVCCStats.WithinBound] this is meant to hold at every instant of a
// churn-only workload. A false value with no active reader is the bounded-resources
// mandate being violated; a false value with an old reader is that reader's cost,
// which [MVCCStats.OldestReaderAge] attributes.
func (s *MVCCStats) WithinCeiling() bool { return s.Total <= s.Ceiling }

// MVCCStats returns the current state of the versioning substrate.
//
// Safe for concurrent use.
func (g *Graph[N, W]) MVCCStats() MVCCStats {
	s := MVCCStats{
		LabelDeltas:           g.labelDeltaActive.Load(),
		PropDeltas:            g.propDeltaActive.Load(),
		AdjVersions:           g.adj.VersionCount(),
		EdgeSideVersions:      g.EdgeSideVersionCount(),
		NodeLifeRecords:       g.nodeLifeActive.Load(),
		IndexRemovalBacklog:   g.idxPendingActive.Load(),
		AdjConflictStamps:     int64(g.adjVer.len()),
		Bound:                 reclaimThreshold,
		Ceiling:               reclaimDebtCeiling,
		Now:                   g.mvccClock.ReadTS(),
		ActiveSnapshots:       g.horizon.Active(),
		UnregisteredSnapshots: g.horizon.Unregistered(),
		SnapshotCapacity:      mvcc.HorizonCapacity,
		Write:                 g.writeCounts.Load(),
		ChainDepth:            g.ChainDepths(),
		InFlightCommits:       g.mvccClock.InFlightCommits(),
	}
	s.Watermark = g.horizon.Oldest(s.Now)
	s.Total = s.LabelDeltas + s.PropDeltas + s.AdjVersions +
		s.EdgeSideVersions + s.NodeLifeRecords + s.IndexRemovalBacklog
	return s
}

// publishMVCCMetrics exports the current state as gauges.
//
// Called from the reclamation sweep rather than from a reader or a writer: the
// sweep is where the numbers change most and it is already off the per-row
// path, so the metrics cost nothing to the workload they describe.
func (g *Graph[N, W]) publishMVCCMetrics() {
	s := g.MVCCStats()
	metrics.SetGauge("lpg.mvcc.versions.label", float64(s.LabelDeltas))
	metrics.SetGauge("lpg.mvcc.versions.property", float64(s.PropDeltas))
	metrics.SetGauge("lpg.mvcc.versions.adjacency", float64(s.AdjVersions))
	metrics.SetGauge("lpg.mvcc.versions.edge_side", float64(s.EdgeSideVersions))
	metrics.SetGauge("lpg.mvcc.versions.node_life", float64(s.NodeLifeRecords))
	metrics.SetGauge("lpg.mvcc.index_removal_backlog", float64(s.IndexRemovalBacklog))
	metrics.SetGauge("lpg.mvcc.adjacency_conflict_stamps", float64(s.AdjConflictStamps))
	metrics.SetGauge("lpg.mvcc.versions.total", float64(s.Total))
	metrics.SetGauge("lpg.mvcc.versions.bound", float64(s.Bound))
	metrics.SetGauge("lpg.mvcc.versions.ceiling", float64(s.Ceiling))
	metrics.SetGauge("lpg.mvcc.watermark", float64(s.Watermark))
	// ONE series for the watermark age, which is also the oldest snapshot's age;
	// see [MVCCStats.OldestSnapshotAge] for why it is not published twice.
	metrics.SetGauge("lpg.mvcc.oldest_snapshot_age", float64(s.OldestSnapshotAge()))
	metrics.SetGauge("lpg.mvcc.in_flight_commits", float64(s.InFlightCommits))
	metrics.SetGauge("lpg.mvcc.snapshots.active", float64(s.ActiveSnapshots))
	metrics.SetGauge("lpg.mvcc.snapshots.unregistered", float64(s.UnregisteredSnapshots))
	metrics.SetGauge("lpg.mvcc.snapshots.capacity", float64(s.SnapshotCapacity))
	metrics.SetGauge("lpg.mvcc.readers.active", float64(s.ActiveReaders()))
	// ── the write side (rmp #2312) ──────────────────────────────────────────────
	metrics.SetGauge("lpg.mvcc.writers.active", float64(s.Write.Writers))
	metrics.SetGauge("lpg.mvcc.commits", float64(s.Write.Commits))
	metrics.SetGauge("lpg.mvcc.aborts", float64(s.Write.Aborts))
	// The conflict TOTALS are gauges here as well as counters at the detection site.
	// They are not a second name for one quantity: the counter is the event, and this
	// is the level the substrate is holding, published from the same sample as the
	// commit count so an operator can divide the two without racing two scrapes.
	metrics.SetGauge("lpg.mvcc.conflicts.total", float64(s.Write.Conflicts))
	metrics.SetGauge("lpg.mvcc.conflict_rate", s.Write.ConflictRate())
	for i := range s.Write.ByStore {
		// Precomputed names; see mvcc_metricnames.go for the allocation this closed.
		metrics.SetGauge(conflictStoreSeries[i], float64(s.Write.ByStore[i]))
	}
	// ── the retained-chain distribution (rmp #2312) ─────────────────────────────
	for i := range s.ChainDepth.Buckets {
		metrics.SetGauge(chainDepthBucketSeries[i], float64(s.ChainDepth.Buckets[i]))
	}
	metrics.SetGauge("lpg.mvcc.chain_depth.deepest", float64(s.ChainDepth.Deepest))
	metrics.SetGauge("lpg.mvcc.chain_depth.chains", float64(s.ChainDepth.Chains()))
	// Per store as well as in total, because "something holds a chain of 300" is only
	// actionable once an operator knows WHICH structure holds it.
	for i := range g.chainDepth {
		d := g.chainDepth[i].Load()
		metrics.SetGauge(chainDepthStoreDeepest[i], float64(d.Deepest))
		metrics.SetGauge(chainDepthStoreChains[i], float64(d.Chains()))
	}
}
