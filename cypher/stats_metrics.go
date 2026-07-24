package cypher

// stats_metrics.go — bounded observability for the planner statistics (task
// #2102, design docs/statistics-design.md). It mirrors the relationship
// count-store's observability surface (count_metrics.go): every emission goes
// through the shared internal/metrics surface, so on the default no-op backend a
// counter increment is two atomic loads and a no-op method call, and a latency
// observation adds a single time.Now pair — negligible.
//
// None of it touches the write path. The statistics are maintained OFF the write
// path — [Engine.RefreshStatistics] is the sole, caller-driven rebuild — and the
// estimate providers are display-only (consulted by EXPLAIN, never by the
// executed plan), so a write on an engine that never refreshed statistics (the
// lazy collector stays unallocated) emits nothing; verified by
// BenchmarkEngWriteAutocommit staying flat at 34 allocs/op.
//
// The names share the cypher.stats.* namespace and render in the Prometheus
// exposition with dots mapped to underscores (see examples/31_metrics_observability).

const (
	// statsMetricRefresh counts each successful [Engine.RefreshStatistics] run
	// that published a fresh snapshot (a counter). A cancelled or failed rebuild
	// does not publish and is not counted.
	statsMetricRefresh = "cypher.stats.refresh"

	// statsMetricRefreshLatency observes the wall-clock duration of one successful
	// [Engine.RefreshStatistics] run (a latency histogram): the consistent read
	// scan plus the estimator build and atomic publish. Its sample count is the
	// number of successful rebuilds and its sum their total duration.
	statsMetricRefreshLatency = "cypher.stats.refresh.latency"

	// statsMetricLookup counts each statistics-provider consultation of a PRESENT
	// collector (a counter): every equality or range cardinality estimate that
	// reaches a live statistics collector increments it once. A stats-free engine
	// (no collector) never reaches here, keeping the surface zero-cost when unused.
	statsMetricLookup = "cypher.stats.lookup"

	// statsMetricLookupFallback counts the subset of provider lookups that yield
	// estFallback (a counter): an absent statistic for the (label, property), or a
	// range estimate demoted by staleness — the cases where the trustworthiness
	// veto keeps the planner on its default plan rather than acting on a missing
	// or non-fresh statistic.
	statsMetricLookupFallback = "cypher.stats.lookup.fallback"
)

// StatsTrackedPairs reports the number of distinct (label, property) pairs the
// engine currently holds planner statistics for (task #2102): the statistics
// footprint, bounded by observed schema cardinality rather than by |V|. It is an
// observability accessor for the size indicator the metrics [Backend] cannot
// express as a gauge (mirroring [Engine.CountStoreCells]); it returns 0 for an
// engine that never refreshed statistics (the lazy collector is unallocated).
// Safe for concurrent use — it reads the collector's atomic-pointer snapshot.
func (e *Engine) StatsTrackedPairs() int {
	if c := e.statsCollector.Load(); c != nil {
		return c.Size()
	}
	return 0
}
