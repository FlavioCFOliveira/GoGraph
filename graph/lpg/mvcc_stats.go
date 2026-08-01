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
//     be ATTRIBUTABLE — [MVCCStats.OldestReaderAge] and
//     [MVCCStats.ActiveReaders] say which readers are responsible, and
//     [MVCCStats.UnregisteredReaders] says when reclamation has stopped
//     altogether.
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

import "github.com/FlavioCFOliveira/GoGraph/internal/metrics"

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
	// Total is the sum of everything above: the memory the substrate is
	// responsible for.
	Total int64
	// Bound is the number of records that may accumulate from CHURN between
	// sweeps. Total above it means a reader is holding versions back, which is
	// legitimate and is what the two fields below explain.
	Bound int64
	// Watermark is the oldest start timestamp among active readers, or zero when
	// reclamation is suspended.
	Watermark uint64
	// Now is the clock's current published instant, so Now-Watermark is how far
	// behind the oldest reader is.
	Now uint64
	// ActiveReaders is how many readers are registered with the horizon.
	ActiveReaders int
	// UnregisteredReaders is how many active readers could not get a horizon
	// slot. While it is non-zero the watermark is zero and NOTHING is
	// reclaimed — the one state in which version memory genuinely has no bound,
	// and the reason this field exists rather than being inferred from
	// unexplained growth.
	UnregisteredReaders int64
}

// OldestReaderAge returns how far behind the current instant the oldest active
// reader is, in commit timestamps.
//
// It is the quantity to look at when [MVCCStats.Total] exceeds
// [MVCCStats.Bound]: a large value names a long-running read as the cause.
func (s *MVCCStats) OldestReaderAge() uint64 {
	if s.Watermark == 0 || s.Now < s.Watermark {
		return 0
	}
	return s.Now - s.Watermark
}

// WithinBound reports whether version memory is at or below the churn bound —
// that is, whether nothing is being held back by a reader.
func (s *MVCCStats) WithinBound() bool { return s.Total <= s.Bound }

// MVCCStats returns the current state of the versioning substrate.
//
// Safe for concurrent use.
func (g *Graph[N, W]) MVCCStats() MVCCStats {
	s := MVCCStats{
		LabelDeltas:         g.labelDeltaActive.Load(),
		PropDeltas:          g.propDeltaActive.Load(),
		AdjVersions:         g.adj.VersionCount(),
		EdgeSideVersions:    g.EdgeSideVersionCount(),
		NodeLifeRecords:     g.nodeLifeActive.Load(),
		IndexRemovalBacklog: g.idxPendingActive.Load(),
		Bound:               reclaimThreshold,
		Now:                 g.mvccClock.ReadTS(),
		ActiveReaders:       g.horizon.Active(),
		UnregisteredReaders: g.horizon.Unregistered(),
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
	metrics.SetGauge("lpg.mvcc.versions.total", float64(s.Total))
	metrics.SetGauge("lpg.mvcc.versions.bound", float64(s.Bound))
	metrics.SetGauge("lpg.mvcc.watermark", float64(s.Watermark))
	metrics.SetGauge("lpg.mvcc.oldest_reader_age", float64(s.OldestReaderAge()))
	metrics.SetGauge("lpg.mvcc.readers.active", float64(s.ActiveReaders))
	metrics.SetGauge("lpg.mvcc.readers.unregistered", float64(s.UnregisteredReaders))
}
