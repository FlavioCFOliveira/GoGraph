package cypher

// stats_metrics_test.go — task #2102. Verifies that the planner statistics'
// bounded observability metrics fire on their wiring paths: the refresh counter
// and refresh latency on a successful RefreshStatistics run, the lookup counter
// on every estimate-provider consultation of a present collector, the
// lookup.fallback counter when a provider returns estFallback, and the
// StatsTrackedPairs size indicator.
//
// The global metrics backend is process-global, so this test — like the other
// backend-installing tests in the package — MUST NOT run in parallel. Race-clean;
// short layer.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// statsMetricProbe records the statistics metric events routed to it. It keeps
// only the cypher.stats.* namespace so unrelated emissions (plan-cache, RunInTx
// counters, count-store) do not clutter the assertions.
type statsMetricProbe struct {
	mu       sync.Mutex
	counters map[string]uint64
	latency  map[string]int
}

func newStatsMetricProbe() *statsMetricProbe {
	return &statsMetricProbe{counters: map[string]uint64{}, latency: map[string]int{}}
}

func (p *statsMetricProbe) IncCounter(name string, delta uint64) {
	if !strings.HasPrefix(name, "cypher.stats.") {
		return
	}
	p.mu.Lock()
	p.counters[name] += delta
	p.mu.Unlock()
}

func (p *statsMetricProbe) SetGauge(string, float64) {}

func (p *statsMetricProbe) ObserveLatency(name string, _ time.Duration) {
	if !strings.HasPrefix(name, "cypher.stats.") {
		return
	}
	p.mu.Lock()
	p.latency[name]++
	p.mu.Unlock()
}

func (p *statsMetricProbe) counter(name string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counters[name]
}

func (p *statsMetricProbe) samples(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latency[name]
}

// TestStatsMetrics_FireOnWiringPaths installs a probe, drives each statistics
// maintenance and provider path once, and asserts the paired metric fired.
func TestStatsMetrics_FireOnWiringPaths(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	const n = 2000
	eng, heavyCount, _ := seedPersonGraph(t, n, 0.40)
	src := statsTestSource(eng)

	// A provider call BEFORE any refresh must be zero-cost: the lazy collector is
	// unallocated, so the stats-free gate returns estFallback and emits NOTHING
	// (neither a lookup nor a fallback).
	if e := statsEqualityEstimate(src, "Person", "age", expr.IntegerValue(30)); e.source != estFallback {
		t.Fatalf("before refresh: source = %v, want fallback", e.source)
	}
	if got := probe.counter(statsMetricLookup); got != 0 {
		t.Fatalf("lookup counter = %d before refresh, want 0 (stats-free engine emits nothing)", got)
	}
	if got := probe.counter(statsMetricLookupFallback); got != 0 {
		t.Fatalf("lookup.fallback counter = %d before refresh, want 0", got)
	}
	if got := eng.StatsTrackedPairs(); got != 0 {
		t.Fatalf("StatsTrackedPairs before refresh = %d, want 0 (unallocated collector)", got)
	}

	// refresh + refresh.latency: one successful RefreshStatistics run.
	if err := eng.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	if got := probe.counter(statsMetricRefresh); got != 1 {
		t.Errorf("refresh counter = %d, want 1 after one successful rebuild", got)
	}
	if got := probe.samples(statsMetricRefreshLatency); got != 1 {
		t.Errorf("refresh.latency samples = %d, want 1 after one successful rebuild", got)
	}

	// StatsTrackedPairs: the seed populated (Person, age) and (Person, name), so
	// the size indicator is exactly 2 distinct (label, property) pairs.
	if got := eng.StatsTrackedPairs(); got != 2 {
		t.Errorf("StatsTrackedPairs after refresh = %d, want 2 ((Person,age)+(Person,name))", got)
	}

	// lookup with NO fallback: the heavy value 30 is an MCV hit (estExact).
	lk0, fb0 := probe.counter(statsMetricLookup), probe.counter(statsMetricLookupFallback)
	if e := statsEqualityEstimate(src, "Person", "age", expr.IntegerValue(30)); e.source != estExact {
		t.Errorf("heavy value 30: source = %v, want exact", e.source)
	}
	if lk := probe.counter(statsMetricLookup); lk != lk0+1 {
		t.Errorf("lookup = %d, want %d after one present-collector equality lookup", lk, lk0+1)
	}
	if fb := probe.counter(statsMetricLookupFallback); fb != fb0 {
		t.Errorf("lookup.fallback = %d, want unchanged %d on an estExact lookup", fb, fb0)
	}

	// lookup WITH fallback: an untracked (label, property) is present-collector
	// lookup + a fallback (the collector is present but holds no such bundle).
	lk1, fb1 := probe.counter(statsMetricLookup), probe.counter(statsMetricLookupFallback)
	if e := statsEqualityEstimate(src, "Person", "unseen_prop", expr.IntegerValue(1)); e.source != estFallback {
		t.Errorf("untracked property: source = %v, want fallback", e.source)
	}
	if lk := probe.counter(statsMetricLookup); lk != lk1+1 {
		t.Errorf("lookup = %d, want %d after one untracked-property lookup", lk, lk1+1)
	}
	if fb := probe.counter(statsMetricLookupFallback); fb != fb1+1 {
		t.Errorf("lookup.fallback = %d, want %d after one untracked-property lookup", fb, fb1+1)
	}

	// A range lookup on a tracked numeric column is a present-collector lookup and
	// (fresh, so) NOT a fallback: it fires lookup once, fallback zero times.
	lk2, fb2 := probe.counter(statsMetricLookup), probe.counter(statsMetricLookupFallback)
	if e, _ := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.IntegerValue(30)); e.source != estStats {
		t.Errorf("range age<30: source = %v, want stats (fresh histogram)", e.source)
	}
	if lk := probe.counter(statsMetricLookup); lk != lk2+1 {
		t.Errorf("lookup = %d, want %d after one present-collector range lookup", lk, lk2+1)
	}
	if fb := probe.counter(statsMetricLookupFallback); fb != fb2 {
		t.Errorf("lookup.fallback = %d, want unchanged %d on a fresh estStats range", fb, fb2)
	}
	_ = heavyCount
}
