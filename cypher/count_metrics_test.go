package cypher

// count_metrics_test.go — task #2087. Verifies that the relationship
// count-store's bounded observability metrics fire on their wiring paths:
// the delta.applied counter on the commit fan-out, the relabel.dirtied
// counter on a relabel, the recompute latency at construction, and the
// lookup / lookup.veto counters on the estExact-provider path.
//
// The global metrics backend is process-global, so this test — like the
// other backend-installing tests in the package — MUST NOT run in parallel.
// Race-clean; short layer.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// countMetricProbe records the count-store metric events routed to it. It keeps
// only the cypher.countstore.* namespace so unrelated emissions (plan-cache,
// RunInTx counters) do not clutter the assertions.
type countMetricProbe struct {
	mu       sync.Mutex
	counters map[string]uint64
	latency  map[string]int
}

func newCountMetricProbe() *countMetricProbe {
	return &countMetricProbe{counters: map[string]uint64{}, latency: map[string]int{}}
}

func (p *countMetricProbe) IncCounter(name string, delta uint64) {
	if !strings.HasPrefix(name, "cypher.countstore.") {
		return
	}
	p.mu.Lock()
	p.counters[name] += delta
	p.mu.Unlock()
}

func (p *countMetricProbe) SetGauge(string, float64) {}

func (p *countMetricProbe) ObserveLatency(name string, _ time.Duration) {
	if !strings.HasPrefix(name, "cypher.countstore.") {
		return
	}
	p.mu.Lock()
	p.latency[name]++
	p.mu.Unlock()
}

func (p *countMetricProbe) counter(name string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counters[name]
}

func (p *countMetricProbe) samples(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latency[name]
}

// TestCountMetrics_FireOnWiringPaths installs a probe, drives each count-store
// maintenance and provider path once, and asserts the paired metric fired.
func TestCountMetrics_FireOnWiringPaths(t *testing.T) {
	probe := newCountMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	// Recompute fires at construction (over the empty graph the histogram still
	// records one sample: a reopen recompute happened). newCountEngine builds the
	// engine via NewEngineWithOptions, which calls recomputeCountStore.
	eng, _ := newCountEngine(t, 0)
	if got := probe.samples(countMetricRecompute); got < 1 {
		t.Errorf("recompute latency samples = %d, want >= 1", got)
	}

	// delta.applied: a typed-edge CREATE buffers E/D/T deltas that the commit
	// fan-out applies.
	beforeDeltas := probe.counter(countMetricDeltaApplied)
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	if got := probe.counter(countMetricDeltaApplied); got <= beforeDeltas {
		t.Errorf("delta.applied = %d, want > %d after a typed-edge CREATE", got, beforeDeltas)
	}

	// relabel.dirtied: a SET on a source node that carries an out-edge dirties the
	// IN X-scoped cells exactly once.
	beforeRelabel := probe.counter(countMetricRelabelDirtied)
	mustRun(t, eng, "MATCH (a:A) SET a:X")
	if got := probe.counter(countMetricRelabelDirtied); got != beforeRelabel+1 {
		t.Errorf("relabel.dirtied = %d, want %d after one relabel", got, beforeRelabel+1)
	}

	// lookup / lookup.veto: exercise the estExact provider on a clean cell (lookup,
	// no veto) and a dirty cell (lookup + veto). The prior SET dirtied D(X,*,IN).
	src := resolverFor(eng)

	lk0, vt0 := probe.counter(countMetricLookup), probe.counter(countMetricLookupVeto)
	if e := degreeCardinalityEstimate(src, "X", "R", count.Out); e.source != estExact {
		t.Errorf("D(X,R,OUT) = %+v, want exact (clean)", e)
	}
	if lk := probe.counter(countMetricLookup); lk != lk0+1 {
		t.Errorf("lookup = %d, want %d after one clean provider call", lk, lk0+1)
	}
	if vt := probe.counter(countMetricLookupVeto); vt != vt0 {
		t.Errorf("lookup.veto = %d, want unchanged %d on a clean lookup", vt, vt0)
	}

	lk1, vt1 := probe.counter(countMetricLookup), probe.counter(countMetricLookupVeto)
	if e := degreeCardinalityEstimate(src, "X", "R", count.In); e.source != estFallback {
		t.Errorf("D(X,R,IN) = %+v, want fallback (dirty)", e)
	}
	if lk := probe.counter(countMetricLookup); lk != lk1+1 {
		t.Errorf("lookup = %d, want %d after one dirty provider call", lk, lk1+1)
	}
	if vt := probe.counter(countMetricLookupVeto); vt != vt1+1 {
		t.Errorf("lookup.veto = %d, want %d after one dirty lookup", vt, vt1+1)
	}
}

// TestCountStoreCells_TracksLiveCells confirms the size-indicator accessor
// reflects the live-cell footprint: zero on an empty store, positive after a
// typed edge, and back toward zero as edges are deleted.
func TestCountStoreCells_TracksLiveCells(t *testing.T) {
	eng, _ := newCountEngine(t, 0)
	if got := eng.CountStoreCells(); got != 0 {
		t.Fatalf("CountStoreCells on empty engine = %d, want 0", got)
	}
	mustRun(t, eng, "CREATE (:A)-[:R]->(:B)")
	if got := eng.CountStoreCells(); got <= 0 {
		t.Fatalf("CountStoreCells after a typed edge = %d, want > 0", got)
	}
	mustRun(t, eng, "MATCH (:A)-[r:R]->(:B) DELETE r")
	if got := eng.CountStoreCells(); got != 0 {
		t.Fatalf("CountStoreCells after deleting the only edge = %d, want 0", got)
	}
}
