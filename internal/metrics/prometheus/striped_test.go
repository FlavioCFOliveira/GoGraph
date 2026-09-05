package prometheus

// striped_test.go — the guarantees the per-core promotion must not break
// (rmp #2698). Layer: short.
//
// Three things are asserted here and each is a separate way the change could
// have been wrong:
//
//  1. No increment or observation is lost across the promotion boundary. The
//     promotion happens WHILE goroutines are emitting, so a delta can land on
//     the unpromoted value, on a shard, or on either side of the swap that
//     detects the contention.
//  2. A series that is never contended never allocates shards. That is the
//     Compliance Mandate 4 half of the design; without it every one of the
//     module's ~490 series would carry 4 KiB of mostly-idle padding.
//  3. The exposition is byte-for-byte what it was. Promotion is an internal
//     representation change and an operator's dashboard must not be able to
//     tell it happened.

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCounter_PromotionLosesNoIncrements(t *testing.T) {
	const (
		goroutines = 16
		each       = 20000
	)
	r := New()
	const name = "striped.counter.total"

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < each; j++ {
				r.IncCounter(name, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	got := r.getOrCreateCounter(name).load()
	if want := uint64(goroutines * each); got != want {
		t.Fatalf("counter total = %d, want %d (lost %d increments across promotion)", got, want, int64(want)-int64(got))
	}
}

// TestCounter_PromotionLosesNoIncrementsWithDelta uses a delta other than 1 so
// a bug that counted CALLS rather than summing DELTAS cannot pass.
func TestCounter_PromotionLosesNoIncrementsWithDelta(t *testing.T) {
	const (
		goroutines = 8
		each       = 10000
		delta      = 7
	)
	r := New()
	const name = "striped.counter.delta"

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.IncCounter(name, delta)
			}
		}()
	}
	wg.Wait()

	got := r.getOrCreateCounter(name).load()
	if want := uint64(goroutines * each * delta); got != want {
		t.Fatalf("counter total = %d, want %d", got, want)
	}
}

func TestHistogram_PromotionLosesNoObservations(t *testing.T) {
	const (
		goroutines = 16
		each       = 5000
	)
	r := New()
	const name = "striped.hist.latency"
	// A duration that lands in a known bucket (<= 100µs is bucket 0).
	const d = 50 * time.Microsecond

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.ObserveLatency(name, d)
			}
		}()
	}
	wg.Wait()

	buckets, inf, sumNs := r.getOrCreateHistogram(name).snapshot()
	total := uint64(goroutines * each)
	if inf != total {
		t.Errorf("inf = %d, want %d", inf, total)
	}
	if buckets[0] != total {
		t.Errorf("buckets[0] = %d, want %d", buckets[0], total)
	}
	if want := int64(total) * int64(d); sumNs != want {
		t.Errorf("sumNs = %d, want %d", sumNs, want)
	}
	for i := 1; i < len(buckets); i++ {
		if buckets[i] != 0 {
			t.Errorf("buckets[%d] = %d, want 0", i, buckets[i])
		}
	}
}

// TestHistogram_PromotionKeepsOverflowOutOfBuckets drives durations ABOVE every
// upper bound, which must reach +Inf and the sum but no bucket, on both sides
// of the promotion.
func TestHistogram_PromotionKeepsOverflowOutOfBuckets(t *testing.T) {
	const (
		goroutines = 8
		each       = 4000
	)
	r := New()
	const name = "striped.hist.overflow"
	const d = 10 * time.Second // > the 5s top bucket

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.ObserveLatency(name, d)
			}
		}()
	}
	wg.Wait()

	buckets, inf, _ := r.getOrCreateHistogram(name).snapshot()
	if want := uint64(goroutines * each); inf != want {
		t.Errorf("inf = %d, want %d", inf, want)
	}
	for i, b := range buckets {
		if b != 0 {
			t.Errorf("buckets[%d] = %d, want 0 (overflow belongs only to +Inf)", i, b)
		}
	}
}

// TestSeries_UncontendedNeverAllocatesShards is the Mandate 4 guarantee: a
// series hammered from ONE goroutine must stay at its single-atomic size, so
// the module's cold series never pay for the shards.
func TestSeries_UncontendedNeverAllocatesShards(t *testing.T) {
	r := New()
	const cname = "striped.cold.counter"
	const hname = "striped.cold.hist"
	for i := 0; i < 200000; i++ {
		r.IncCounter(cname, 1)
		r.ObserveLatency(hname, time.Millisecond)
	}
	if s := r.getOrCreateCounter(cname).shards.Load(); s != nil {
		t.Errorf("counter promoted without contention: an uncontended series must not allocate %d shards", shardCount)
	}
	if s := r.getOrCreateHistogram(hname).shards.Load(); s != nil {
		t.Errorf("histogram promoted without contention: an uncontended series must not allocate %d shards", shardCount)
	}
	if got := r.getOrCreateCounter(cname).load(); got != 200000 {
		t.Errorf("uncontended counter total = %d, want 200000", got)
	}
}

// TestPromote_Idempotent asserts a second promotion cannot replace the shard
// array and discard what it holds.
func TestPromote_Idempotent(t *testing.T) {
	r := New()
	const name = "promote.idempotent"
	r.IncCounter(name, 0) // establish the series
	c := r.getOrCreateCounter(name)
	c.promote()
	first := c.shards.Load()
	if first == nil {
		t.Fatal("promote did not install shards")
	}
	first.slots[3].n.Store(41)
	c.promote()
	if got := c.shards.Load(); got != first {
		t.Fatal("second promote replaced the shard array, discarding its counts")
	}
	r.IncCounter(name, 1)
	if got := c.load(); got != 42 {
		t.Errorf("load = %d, want 42", got)
	}

	h := &histogram{}
	h.promote()
	hFirst := h.shards.Load()
	h.promote()
	if got := h.shards.Load(); got != hFirst {
		t.Fatal("second histogram promote replaced the shard array")
	}
}

// TestExposition_IdenticalWhetherPromoted is acceptance criterion 5: the text a
// scrape sees must not reveal whether a series was promoted. Two registries are
// given identical data; one is forced to the striped representation, the other
// is left unpromoted. The bytes must match exactly — same names, same labels,
// same types, same values, same order.
func TestExposition_IdenticalWhetherPromoted(t *testing.T) {
	type sample struct {
		name string
		d    time.Duration
	}
	samples := []sample{
		{"search.dijkstra", 50 * time.Microsecond},
		{"search.dijkstra", 3 * time.Millisecond},
		{"search.dijkstra", 9 * time.Second},
		{"store.wal.Append", 700 * time.Microsecond},
		{"store.wal.Append", 2 * time.Second},
	}
	counters := []struct {
		name string
		n    uint64
	}{
		{"search.dijkstra.errors", 3},
		{"store.wal.Append.errors", 11},
		{"cypher.exec.rows", 900001},
	}
	gauges := []struct {
		name string
		v    float64
	}{
		{"graph.lpg.mvcc.versions", 1234.5},
		{"store.wal.segments", 7},
	}

	build := func(promote bool) string {
		r := New()
		// Establish every series first, then promote, so the promoted registry
		// also exercises the unpromoted-then-striped path the real one takes.
		for _, s := range samples {
			r.ObserveLatency(s.name, s.d)
		}
		for _, c := range counters {
			r.IncCounter(c.name, 1)
		}
		if promote {
			for _, s := range samples {
				r.getOrCreateHistogram(s.name).promote()
			}
			for _, c := range counters {
				r.getOrCreateCounter(c.name).promote()
			}
		}
		// The remaining volume lands on whichever representation is installed.
		for _, s := range samples {
			r.ObserveLatency(s.name, s.d)
		}
		for _, c := range counters {
			r.IncCounter(c.name, c.n-1)
		}
		for _, g := range gauges {
			r.SetGauge(g.name, g.v)
		}
		var buf bytes.Buffer
		if err := r.WriteText(&buf); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		return buf.String()
	}

	plain, promoted := build(false), build(true)
	if plain != promoted {
		t.Fatalf("exposition differs once a series is promoted.\n--- unpromoted ---\n%s\n--- promoted ---\n%s", plain, promoted)
	}
	// Guard against both arms being empty or degenerate.
	for _, want := range []string{
		"# TYPE search_dijkstra histogram",
		"# TYPE search_dijkstra_errors counter",
		"search_dijkstra_errors 3",
		"# TYPE graph_lpg_mvcc_versions gauge",
		`search_dijkstra_bucket{le="+Inf"} 6`,
		"search_dijkstra_count 6",
		"cypher_exec_rows 900001",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("exposition missing %q; got:\n%s", want, plain)
		}
	}
}

// TestExposition_PromotedUnderConcurrencyMatchesTotals closes the loop between
// the two halves: a series really promoted by real contention must expose the
// same totals the emitters applied.
func TestExposition_PromotedUnderConcurrencyMatchesTotals(t *testing.T) {
	const (
		goroutines = 12
		each       = 8000
	)
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.IncCounter("hot.counter", 2)
				r.ObserveLatency("hot.hist", time.Millisecond)
			}
		}()
	}
	wg.Wait()

	if s := r.getOrCreateCounter("hot.counter").shards.Load(); s == nil {
		t.Error("counter was never promoted despite 12 goroutines contending; the contention signal did not fire")
	}
	if s := r.getOrCreateHistogram("hot.hist").shards.Load(); s == nil {
		t.Error("histogram was never promoted despite 12 goroutines contending; the contention signal did not fire")
	}
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"hot_counter 192000",   // 12 * 8000 * 2
		"hot_hist_count 96000", // 12 * 8000
		`hot_hist_bucket{le="+Inf"} 96000`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q; got:\n%s", want, out)
		}
	}
}

// TestShardIndex_SpreadsAcrossShards is the guard that a wrong [minStackShift]
// would trip.
//
// Nothing else in this file can catch that mistake: a shard index that sends
// every goroutine to the same slot is still CORRECT — no count is lost, the
// exposition is exact, the race detector is silent — it simply does not remove
// the contention it exists to remove. The first version of this package shifted
// by 13, and 8 goroutines landed on 2 of the 32 shards. Every other test passed.
//
// The bounds are deliberately loose. Stack addresses come from the runtime's
// allocator and the exact assignment is not contractual; what is asserted is
// that the index DISTINGUISHES goroutines at all, which is the property the
// design depends on.
func TestShardIndex_SpreadsAcrossShards(t *testing.T) {
	for _, tc := range []struct{ n, wantOccupied int }{
		{8, 5},
		{64, 16},
	} {
		occupied := make([]bool, shardCount)
		var mu sync.Mutex
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < tc.n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				idx := shardIndex()
				mu.Lock()
				occupied[idx] = true
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()

		got := 0
		for _, o := range occupied {
			if o {
				got++
			}
		}
		if got < tc.wantOccupied {
			t.Errorf("%d goroutines occupied only %d of %d shards, want at least %d: "+
				"shardIndex is not distinguishing goroutines, so the shards do not remove the contention",
				tc.n, got, shardCount, tc.wantOccupied)
		} else {
			t.Logf("%d goroutines occupied %d of %d shards", tc.n, got, shardCount)
		}
	}
}

// TestShardIndex_StableWithinAGoroutine asserts the affinity the design needs:
// a goroutine must keep choosing the SAME shard, because a goroutine that
// migrates between shards drags every line it visits through every core's cache
// — which is what random selection did, measured at 0.40x scaling.
func TestShardIndex_StableWithinAGoroutine(t *testing.T) {
	const samples = 100000
	first := shardIndex()
	changed := 0
	for i := 0; i < samples; i++ {
		if shardIndex() != first {
			changed++
		}
	}
	if changed != 0 {
		t.Errorf("shardIndex changed on %d of %d samples within one goroutine at a fixed call depth; affinity is not stable", changed, samples)
	}
}
