package cypher

// count_metrics.go — bounded observability for the relationship count-store
// (task #2087, design docs/count-store-design.md §§3, 6, 7). Every emission goes
// through the shared internal/metrics surface: on the default no-op backend a
// counter increment is two atomic loads and a no-op method call, and a latency
// observation adds a single time.Now pair — negligible, and none of it touches
// the bare-CREATE write path (the emissions live behind the same guards the
// count maintenance already uses, so a write that touches no count cell emits
// nothing; verified by BenchmarkEngWriteAutocommit staying flat).
//
// The names share the cypher.countstore.* namespace and render in the Prometheus
// exposition with dots mapped to underscores (see examples/31_metrics_observability).

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

const (
	// countMetricDeltaApplied counts the individual E/D/T cell increments applied
	// on a write transaction's commit fan-out (a counter). It is incremented by
	// the number of deltas the commit actually applies to a live store, so it is
	// zero for a write that buffered no count delta.
	countMetricDeltaApplied = "cypher.countstore.delta.applied"

	// countMetricLookup counts each estExact-provider consultation of the store
	// (a counter): every rel/degree/triple cardinality estimate that reaches a
	// present store increments it once.
	countMetricLookup = "cypher.countstore.lookup"

	// countMetricLookupVeto counts the subset of provider lookups that a dirty
	// X-scoped family forces to estFallback (a counter) — the trustworthiness
	// veto that keeps the planner on its default plan rather than acting on a
	// non-exact degree/triple statistic.
	countMetricLookupVeto = "cypher.countstore.lookup.veto"

	// countMetricRelabelDirtied counts node relabels that marked count cells
	// non-exact (a counter): one increment per relabel that reaches the X-scoped
	// dirty marking (design §3.3.1).
	countMetricRelabelDirtied = "cypher.countstore.relabel.dirtied"

	// countMetricRecompute observes the O(V+E) reopen recompute (a latency
	// histogram): its sample count is the number of recomputes and its sum their
	// total duration (task #2084, design §6).
	countMetricRecompute = "cypher.countstore.recompute"
)

// CountStoreCells reports the number of distinct live relationship count-store
// cells the engine currently holds (design §2.3): the count-store's footprint,
// bounded by observed schema cardinality rather than by |V| or |E|. It is an
// observability accessor for the size indicator the metrics [Backend] cannot
// express as a gauge (task #2087); it returns 0 for an engine without a count
// store. Safe for concurrent use — it reads the store under its own shard read
// locks.
func (e *Engine) CountStoreCells() int {
	if e.countStore == nil {
		return 0
	}
	return e.countStore.Cells()
}

// CountSnapshot returns a point-in-time copy of the relationship count store's
// live cells and dirty markings, for observability and differential testing. It
// returns the zero Snapshot when the engine holds no count store.
//
// The returned maps are freshly allocated and owned by the caller. Their keys are
// the graph's interned label and relationship-type ids
// ([github.com/FlavioCFOliveira/GoGraph/graph/lpg.LabelRegistry]), so a caller
// that wants names resolves them through that registry — and must do so against
// the SAME graph generation, since ids are assigned in intern order and a graph
// rebuilt by recovery re-interns from scratch. The four dirty slices list the
// label ids each family currently holds as non-exact, in unspecified order; a
// dirty cell's value is a lower bound, not an exact count (design
// docs/count-store-design.md §3.3.1).
//
// Only cells the store currently holds appear: a cell is deleted the moment its
// counter returns to zero, so an absent key means zero rather than unknown.
//
// It is an observability accessor deliberately narrower than the store handle:
// exposing [github.com/FlavioCFOliveira/GoGraph/graph/index/count.Store] itself
// would hand callers Apply, MarkDirty and RecomputeReset over the engine's own
// derived state.
//
// CountSnapshot is safe for concurrent use — it reads the store under its own
// shard and dirty read locks, so it may be called alongside writers, which are
// NOT serialised against each other.
func (e *Engine) CountSnapshot() count.Snapshot {
	if e.countStore == nil {
		return count.Snapshot{}
	}
	return e.countStore.Snapshot()
}

// recordCountCommit emits the delta.applied counter for one commit fan-out. It is
// a no-op when no store is active or the buffer applied no delta, so a write that
// touched no count cell — and the store-less engine — pay nothing. It must be
// called BEFORE [exec.CountBuffer.Commit], which resets the buffer.
func recordCountCommit(cs *count.Store, cbuf *exec.CountBuffer) {
	if cs == nil || cbuf == nil {
		return
	}
	if nd := cbuf.NumDeltas(); nd > 0 {
		cmetrics.IncCounter(countMetricDeltaApplied, uint64(nd))
	}
}
