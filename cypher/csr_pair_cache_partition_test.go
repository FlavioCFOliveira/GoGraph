package cypher

// csr_pair_cache_partition_test.go — the non-vacuity gate for the CSR pair
// cache's consultation counters (rmp #2866).
//
// The cache emitted `cypher.csr_pair_cache.hits` and `…misses` from inside
// [csrPairCache.getWithColumn], which sees only the consultations that REACH the
// map. The guards that turn a consultation away before the lookup — no cache, and
// the rmp #2446 own-writes exclusion — emitted nothing, so a hit rate computed as
// hits/(hits+misses) reads 100% for a workload in which every consultation was
// bypassed. [countCSRPairBypass] closes that.
//
// The counters are pinned by their INVARIANTS rather than by absolute numbers,
// because a process-global counter cannot be owned by one test:
//
//   - every bucket must be REACHABLE. A bucket no drive can move is
//     indistinguishable from one that is never incremented, and a rate computed
//     from it would be uninterpretable rather than merely wrong;
//   - misses + bypasses must equal the delta of [csrPairUncachedBuildCount], a
//     counter maintained elsewhere by different code. Two independent instruments
//     agreeing is what makes an over-count visible: inflate a bypass bucket and
//     the identity breaks, whereas the hit RATE alone would merely look worse and
//     nothing in the module would notice.
//
// Neither test is parallel, and they must stay that way: they install a
// process-global metrics backend. Go releases top-level parallel tests only after
// every serial one has returned, so a serial test observes no concurrent
// contribution to the counters it reads.
//
// Layer: short.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// csrPairMetricProbe is a [metrics.Backend] that totals counters by name.
type csrPairMetricProbe struct {
	mu sync.Mutex
	c  map[string]uint64
}

func newCSRPairMetricProbe() *csrPairMetricProbe {
	return &csrPairMetricProbe{c: make(map[string]uint64)}
}

func (p *csrPairMetricProbe) IncCounter(name string, delta uint64) {
	p.mu.Lock()
	p.c[name] += delta
	p.mu.Unlock()
}
func (p *csrPairMetricProbe) ObserveLatency(string, time.Duration) {}
func (p *csrPairMetricProbe) SetGauge(string, float64)             {}

func (p *csrPairMetricProbe) get(name string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.c[name]
}

// bypasses totals the two bypass buckets.
func (p *csrPairMetricProbe) bypasses() uint64 {
	return p.get("cypher.csr_pair_cache.bypasses_no_cache") +
		p.get("cypher.csr_pair_cache.bypasses_own_writes")
}

// installCSRPairProbe makes the counters readable for the duration of t.
func installCSRPairProbe(t *testing.T) *csrPairMetricProbe {
	t.Helper()
	probe := newCSRPairMetricProbe()
	var b metrics.Backend = probe
	metrics.SetBackend(b)
	t.Cleanup(func() { metrics.SetBackend(nil) })
	return probe
}

func TestCSRPairCache_ConsultationsPartitionExactly(t *testing.T) {
	probe := installCSRPairProbe(t)
	ctx := context.Background()
	eng := NewEngine(relDirCypherFixture(t))

	buildsBefore := csrPairUncachedBuildCount.Load()

	// 1. A pure READ, repeated on one Engine with nothing committed in between:
	//    the first consultation misses and the rest must hit.
	const read = `MATCH (n:N {k:'a'})-[r:T]->(m) RETURN count(r) AS c`
	for i := 0; i < 4; i++ {
		relDirRunRows(t, eng, read)
	}
	if got := probe.get("cypher.csr_pair_cache.misses"); got == 0 {
		t.Fatal("no miss over a cold Engine: the miss bucket is unreachable, so nothing " +
			"this test asserts about the hit rate is evidence")
	}
	if got := probe.get("cypher.csr_pair_cache.hits"); got == 0 {
		t.Fatal("no hit over four identical reads on one Engine: the hit bucket is " +
			"unreachable, so a 0% hit rate measured elsewhere would be uninterpretable")
	}
	if got := probe.bypasses(); got != 0 {
		t.Errorf("a pure read bypassed the cache %d time(s): a reader is being turned "+
			"away, which would make every measured rate a measurement of the wrong thing", got)
	}

	// 2. A WRITE transaction that also expands must not be served, and must not
	//    serve. Which bypass bucket it lands in is deliberately NOT asserted: the
	//    engine's write build threads no cache at all today, so it books
	//    `no_cache` rather than the rmp #2446 `own_writes` — measured, and recorded
	//    as a finding on rmp #2866 rather than pinned here, since either bucket
	//    satisfies the rule this test is about.
	hitsBefore := probe.get("cypher.csr_pair_cache.hits")
	bypassBefore := probe.bypasses()
	res, err := eng.RunAny(ctx, `MATCH (n:N {k:'a'})-[r:T]->(m) SET m.seen = true`, nil)
	if err != nil {
		t.Fatalf("write query: %v", err)
	}
	for res.Next() {
	}
	res.Close()
	if probe.bypasses() == bypassBefore {
		t.Error("a write transaction's expand recorded no bypass at all: it was either " +
			"served from the shared cache, which rmp #2446 forbids, or the counter does " +
			"not observe the route it took")
	}
	if got := probe.get("cypher.csr_pair_cache.hits"); got != hitsBefore {
		t.Errorf("a write transaction's expand was served %d cached pair(s): rmp #2446 "+
			"forbids sharing a view that sees its own uncommitted writes", got-hitsBefore)
	}

	// 3. The cross-instrument identity. Every route into an O(V+E) build funnels
	//    through csrPairFromGraphAt, and production reaches no other one, so the
	//    builds this drive caused must be exactly its misses plus its bypasses.
	builds := csrPairUncachedBuildCount.Load() - buildsBefore
	misses := probe.get("cypher.csr_pair_cache.misses")
	bypasses := probe.bypasses()
	if builds != misses+bypasses {
		t.Errorf("csrPairUncachedBuildCount moved by %d while the consultation counters "+
			"account for %d (misses %d + bypasses %d): the two instruments disagree, so at "+
			"least one of them is miscounting",
			builds, misses+bypasses, misses, bypasses)
	}
}

// TestCSRPairCache_BypassBucketsAreDistinguishable pins that [countCSRPairBypass]
// reports the REASON, not merely the fact, and that BOTH reasons are reachable.
//
// The reason is the whole diagnostic value. A no-cache bypass is a WIRING
// property — the build never had a cache to consult — while an own-writes bypass
// is the rmp #2446 isolation rule working as designed. They call for opposite
// responses, and a counter that could not tell them apart would leave the #2866
// rebuild cost exactly as unattributable as it was before it existed.
func TestCSRPairCache_BypassBucketsAreDistinguishable(t *testing.T) {
	probe := installCSRPairProbe(t)
	g := relDirCypherFixture(t)

	// No cache at all — the route a build with no Engine behind it takes.
	csrPairCachedAt(nil, g.ReadAt(nil))
	if got := probe.get("cypher.csr_pair_cache.bypasses_no_cache"); got != 1 {
		t.Errorf("no_cache bucket = %d, want 1", got)
	}
	if got := probe.get("cypher.csr_pair_cache.bypasses_own_writes"); got != 0 {
		t.Errorf("a nil cache was booked as an own-writes bypass %d time(s)", got)
	}

	// A cache present, consulted through a WRITE transaction's own view — the
	// rmp #2446 case. [lpg.Graph.WriterView] is bound to the open transaction's
	// snapshot, which carries its id, so viewCarriesOwnWrites must refuse it.
	cache := newCSRPairCache()
	if err := g.ApplyVersioned(func(lpg.WriteTx) error {
		view := g.WriterView()
		if view.Snapshot() == nil || view.Snapshot().TxID() == 0 {
			t.Fatal("WriterView carries no transaction id: this test is not exercising " +
				"the own-writes case it claims to")
		}
		csrPairCachedAt(cache, view)
		return nil
	}); err != nil {
		t.Fatalf("write bracket: %v", err)
	}
	if got := probe.get("cypher.csr_pair_cache.bypasses_own_writes"); got != 1 {
		t.Errorf("own_writes bucket = %d, want 1: the rmp #2446 guard did not fire, or "+
			"the counter does not observe it", got)
	}
	if got := probe.get("cypher.csr_pair_cache.bypasses_no_cache"); got != 1 {
		t.Errorf("the own-writes bypass was booked against the no_cache bucket, which now "+
			"reads %d: the two reasons are not distinguishable", got)
	}
	if got := probe.get("cypher.csr_pair_cache.hits"); got != 0 {
		t.Errorf("a write transaction's view was served %d cached pair(s)", got)
	}
}
