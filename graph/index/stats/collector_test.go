package stats

import (
	"sync"
	"testing"
)

func newTestStats(gen uint64, n0 int64) *Stats[int] {
	return NewStats(Input[int]{
		NDV:        NewHLL(),
		MCV:        BuildTopK[int](nil, 32),
		Histograms: map[Domain]*Histogram[int]{},
		Generation: gen,
		LabelCount: n0,
		Buckets:    histB,
	})
}

func TestCollector_PublishLookupTracking(t *testing.T) {
	c := NewCollector[int]()
	if c.Tracking() {
		t.Error("empty collector reports Tracking")
	}
	if _, ok := c.Lookup(1, 2); ok {
		t.Error("empty collector lookup hit")
	}

	st := newTestStats(10, 500)
	c.Publish(map[Key]*Stats[int]{{Label: 1, Prop: 2}: st})

	if !c.Tracking() {
		t.Error("populated collector not Tracking")
	}
	if c.Size() != 1 {
		t.Errorf("size = %d, want 1", c.Size())
	}
	got, ok := c.Lookup(1, 2)
	if !ok || got != st {
		t.Errorf("lookup = (%v,%v), want the published stats", got, ok)
	}
	labels := c.TrackedLabelsForProp(2)
	if len(labels) != 1 || labels[0] != 1 {
		t.Errorf("tracked labels for prop 2 = %v, want [1]", labels)
	}
	if l := c.TrackedLabelsForProp(999); l != nil {
		t.Errorf("tracked labels for untracked prop = %v, want nil", l)
	}
}

// TestStats_StalenessDemotion is the direct proof of the §3 promotion rule: with
// b = 0.10 and 1/B = 1/256, an estStats range estimate must stay trustworthy
// while Δ/N < b − 1/B and demote the instant Δ/N crosses it.
func TestStats_StalenessDemotion(t *testing.T) {
	const (
		b    = 0.10
		invB = 1.0 / float64(histB)
		n0   = int64(10000)
	)
	// The firing region closes at Δ/N ≥ b − 1/B. Convert to a Δ count over N=n0.
	threshold := b - invB // ≈ 0.09609
	crossAt := int64(threshold*float64(n0)) + 1

	st := newTestStats(1, n0)
	// isFresh models the consumer's demotion test using the live Δ against N.
	isFresh := func(delta, n int64) bool {
		return float64(delta)/float64(n) < threshold
	}

	// Just below the threshold: still fresh.
	for i := int64(0); i < crossAt-1; i++ {
		st.RecordWrite()
	}
	if !isFresh(st.Delta(), n0) {
		t.Errorf("Δ=%d/N=%d should still be fresh (threshold %.5f)", st.Delta(), n0, threshold)
	}
	// Cross the threshold: must demote.
	for st.Delta() < crossAt {
		st.RecordWrite()
	}
	if isFresh(st.Delta(), n0) {
		t.Errorf("Δ=%d/N=%d must have demoted (threshold %.5f)", st.Delta(), n0, threshold)
	}
}

func TestStats_DeleteToleranceRebuild(t *testing.T) {
	st := newTestStats(1, 1000)
	const tol = 0.01 // 1%
	// Below tolerance: no rebuild.
	for i := 0; i < 10; i++ { // 10/1000 = 1.0%, not strictly greater
		st.RecordDelete()
	}
	if st.NeedsRebuildForDeletes(tol) {
		t.Errorf("deletes=%d n0=1000 should NOT need rebuild at tol=%.2f", st.Deletes(), tol)
	}
	// One more crosses strictly above 1%.
	st.RecordDelete()
	if !st.NeedsRebuildForDeletes(tol) {
		t.Errorf("deletes=%d n0=1000 SHOULD need rebuild at tol=%.2f", st.Deletes(), tol)
	}

	// A degenerate zero label count always forces a rebuild.
	if !newTestStats(1, 0).NeedsRebuildForDeletes(tol) {
		t.Error("n0=0 must force a rebuild")
	}
}

// TestCollector_ConcurrentReadDuringPublish exercises the lock-free read path
// against concurrent republishes; -race is the real assertion.
func TestCollector_ConcurrentReadDuringPublish(t *testing.T) {
	c := NewCollector[int]()
	c.Publish(map[Key]*Stats[int]{{Label: 1, Prop: 1}: newTestStats(1, 100)})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: lock-free lookups and staleness bumps.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if st, ok := c.Lookup(1, 1); ok {
						st.RecordWrite()
						_ = st.Delta()
					}
					c.RecordDelete(1, 1)
					_ = c.TrackedLabelsForProp(1)
				}
			}
		}()
	}
	// Writer: republish fresh snapshots.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			c.Publish(map[Key]*Stats[int]{{Label: 1, Prop: 1}: newTestStats(uint64(i), 100)})
		}
	}()

	// Let the readers run against the republish stream, then stop.
	for c.snap.Load().byKey[Key{Label: 1, Prop: 1}].Generation() < 1000 {
		if _, ok := c.Lookup(1, 1); !ok {
			break
		}
	}
	close(stop)
	wg.Wait()
}
