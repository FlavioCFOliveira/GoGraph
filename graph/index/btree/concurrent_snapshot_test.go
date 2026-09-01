package btree

// concurrent_snapshot_test.go — the acceptance gate for the copy-on-write,
// lock-free-read redesign (task #2683).
//
// The redesign removed the single RWMutex that used to make every operation
// trivially atomic. Reads now traverse an immutable snapshot with no lock on
// the structure at all, and writers mutate a key's node-set under that key's
// own lock. Two properties have to be shown, and neither is visible to a
// single-goroutine test:
//
//  1. IDENTITY. After a concurrent workload quiesces, the index must hold
//     exactly what a serial reference model holds — same keys, same node sets,
//     same answers to every read API, including the float64 total-order corner
//     cases (NaN as one leading key, ±0.0 as one key).
//     [TestConcurrentRangeSeekMatchesReference]
//
//  2. BRACKETING. Every scan taken WHILE writers run must be a superset of the
//     population that was already there when the scan could have started and a
//     subset of the population that exists when it could have finished. That is
//     what rules out a torn read of a splitting spine: a well-formedness check
//     (ascending, no duplicates) would pass on a scan that silently skipped a
//     whole subtree, so the oracle encodes IDENTITY of the observed ids, not
//     the shape of the result. The oracle is made non-vacuous by construction —
//     one writer is paced against the readers so intermediate states are
//     guaranteed to be observed, and the test FAILS if none were.
//     [TestConcurrentRangeObservationsAreBracketed]

import (
	"cmp"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// --- the serial reference model -------------------------------------------

// refEntry is one live key of the reference model with its node ids ascending.
type refEntry struct {
	key   float64
	nodes []uint64
}

// refModel is a deliberately naive, fully serial model of the index: one
// mutex, one map from normalised key identity to a node set. It is slow and
// obviously correct, which is exactly what an oracle has to be.
type refModel struct {
	keys map[uint64]map[uint64]struct{}
	mu   sync.Mutex
}

func newRefModel() *refModel {
	return &refModel{keys: make(map[uint64]map[uint64]struct{})}
}

// normBits maps a float64 to the key identity the index's total order uses:
// every NaN bit pattern is ONE key and -0.0 is the same key as +0.0. A Go map
// keyed by the raw float64 would do neither — NaN != NaN would create a fresh
// entry per insert, and the model would disagree with the index for reasons
// that have nothing to do with concurrency.
func normBits(v float64) uint64 {
	switch {
	case math.IsNaN(v):
		return math.Float64bits(math.NaN())
	case v == 0:
		return math.Float64bits(0)
	default:
		return math.Float64bits(v)
	}
}

func (m *refModel) insert(v float64, n uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := normBits(v)
	s := m.keys[b]
	if s == nil {
		s = make(map[uint64]struct{})
		m.keys[b] = s
	}
	s[n] = struct{}{}
}

func (m *refModel) remove(v float64, n uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := normBits(v)
	s := m.keys[b]
	if s == nil {
		return
	}
	delete(s, n)
	if len(s) == 0 {
		delete(m.keys, b)
	}
}

// snapshot returns every live key in the index's total order, each with its
// node ids ascending. Every reference answer below is derived from it.
func (m *refModel) snapshot() []refEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]refEntry, 0, len(m.keys))
	for b, s := range m.keys {
		nodes := make([]uint64, 0, len(s))
		for n := range s {
			nodes = append(nodes, n)
		}
		slices.Sort(nodes)
		out = append(out, refEntry{key: math.Float64frombits(b), nodes: nodes})
	}
	slices.SortFunc(out, func(a, b refEntry) int { return cmp.Compare(a.key, b.key) })
	return out
}

// inRange reports whether k falls in the inclusive interval under the TOTAL
// order — the same predicate [Index.Range] applies, so a NaN lower bound
// admits the NaN key and any other lower bound excludes it.
func inRange(k, lo, hi float64) bool {
	return cmp.Compare(k, lo) >= 0 && cmp.Compare(k, hi) <= 0
}

func (m *refModel) rangeNodes(lo, hi float64) []uint64 {
	var out []uint64
	if cmp.Less(hi, lo) {
		return out
	}
	for _, e := range m.snapshot() {
		if inRange(e.key, lo, hi) {
			out = append(out, e.nodes...)
		}
	}
	slices.Sort(out)
	return out
}

func (m *refModel) rangeFromNodes(lo float64) []uint64 {
	var out []uint64
	for _, e := range m.snapshot() {
		if cmp.Compare(e.key, lo) >= 0 {
			out = append(out, e.nodes...)
		}
	}
	slices.Sort(out)
	return out
}

func (m *refModel) rangeFirst(lo, hi float64) (float64, uint64, bool) {
	if cmp.Less(hi, lo) {
		return 0, 0, false
	}
	for _, e := range m.snapshot() {
		if inRange(e.key, lo, hi) {
			return e.key, e.nodes[0], true
		}
	}
	return 0, 0, false
}

func (m *refModel) lookup(v float64) []uint64 {
	for _, e := range m.snapshot() {
		if normBits(e.key) == normBits(v) {
			return e.nodes
		}
	}
	return nil
}

func (m *refModel) distinct() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.keys)
}

// --- shared fixtures -------------------------------------------------------

// concurrentValuePool is the key space the concurrent writers draw from. It
// deliberately mixes ordinary finite values with every float the total order
// treats specially: four DISTINCT NaN bit patterns (which must collapse into
// one key), both signed zeros (one key), and both infinities.
func concurrentValuePool() []float64 {
	pool := []float64{
		math.Float64frombits(0x7FF8000000000001), // quiet NaN, math.NaN()'s pattern
		math.Float64frombits(0x7FF8000000000002), // quiet NaN, different payload
		math.Float64frombits(0xFFF8000000000003), // NEGATIVE quiet NaN
		math.Float64frombits(0x7FF0000000000001), // signalling NaN
		math.Inf(-1),
		-1e300,
		-1.5,
		math.Copysign(0, -1), // -0.0
		0.0,                  // +0.0 — the same key as -0.0
		1.5,
		1e300,
		math.Inf(1),
	}
	// A spread of ordinary values, wide enough that the tree must split.
	for i := 0; i < 512; i++ {
		pool = append(pool, float64(i)*0.25-64)
	}
	return pool
}

// poolValue picks a pool entry for id deterministically, so a writer's
// (value, node) pairs are reproducible and every writer spreads over the whole
// key space (hitting both the fast in-place path and the structural path).
func poolValue(pool []float64, id uint64) float64 {
	return pool[(id*2654435761)%uint64(len(pool))]
}

// bitmapToSorted materialises a roaring bitmap as an ascending slice so it can
// be compared against the reference model with slices.Equal.
func bitmapToSorted(bm *roaring64.Bitmap) []uint64 {
	if bm.IsEmpty() {
		return nil
	}
	return bm.ToArray()
}

// --- 1. identity against the serial reference ------------------------------

// TestConcurrentRangeSeekMatchesReference drives concurrent Inserts (brand-new
// keys, already-present keys, and NaN keys) against concurrent Range /
// RangeFirst / RangeFrom / Lookup / Cardinality readers, then asserts that once
// the workload has quiesced the index answers IDENTICALLY to a serial
// reference model for a spread of ranges — including a NaN lower bound (which
// must admit the NaN key) and a non-NaN lower bound (which must exclude it).
//
// Every (value, node) pair is owned by exactly one writer goroutine, which
// inserts it and may later delete it, in that program order. The final state is
// therefore independent of how the goroutines interleave, which is what makes
// an exact identity assertion legitimate rather than flaky.
func TestConcurrentRangeSeekMatchesReference(t *testing.T) {
	t.Parallel()

	const (
		writers      = 8
		readers      = 8
		opsPerWriter = 3000
		initialKeys  = 400
		initialFanIn = 3
	)

	pool := concurrentValuePool()
	idx := New[float64]()
	ref := newRefModel()

	// Seed a stable population the readers can always see. Node ids 1..N are
	// reserved for it and are never deleted.
	var nextInitial uint64 = 1
	for k := 0; k < initialKeys; k++ {
		v := poolValue(pool, uint64(k)*7+1)
		for f := 0; f < initialFanIn; f++ {
			idx.Insert(v, graph.NodeID(nextInitial))
			ref.insert(v, nextInitial)
			nextInitial++
		}
	}
	// Writer ids start well above every initial id, and each writer owns a
	// disjoint, contiguous band of them.
	const writerBase uint64 = 1 << 20

	// Two groups: writersWG so the readers can be stopped exactly when the
	// writers are done, and wg so nothing outlives the test (goleak).
	var wg, writersWG sync.WaitGroup
	done := make(chan struct{})

	for w := 0; w < writers; w++ {
		wg.Add(1)
		writersWG.Add(1)
		go func(w int) {
			defer wg.Done()
			defer writersWG.Done()
			base := writerBase + uint64(w)*opsPerWriter
			for j := 0; j < opsPerWriter; j++ {
				id := base + uint64(j)
				v := poolValue(pool, id)
				idx.Insert(v, graph.NodeID(id))
				ref.insert(v, id)
			}
			// Delete every third id this writer created. Some of these empty a
			// key entirely (forcing the structural detach and its resurrect
			// race), others only shrink a set.
			for j := 0; j < opsPerWriter; j += 3 {
				id := base + uint64(j)
				v := poolValue(pool, id)
				idx.Delete(v, graph.NodeID(id))
				ref.remove(v, id)
			}
		}(w)
	}

	// Readers run for the duration purely to put the lock-free read paths under
	// the race detector while the spine is being rebuilt. Their light invariant
	// checks cannot substitute for the identity assertions below; the
	// bracketing oracle is TestConcurrentRangeObservationsAreBracketed.
	var readerOps atomic.Uint64
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			probe := poolValue(pool, uint64(r)*7+1)
			for {
				select {
				case <-done:
					return
				default:
				}
				if v, _, ok := idx.RangeFirst(-1e308, 1e308); ok {
					if cmp.Less(v, -1e308) || cmp.Less(1e308, v) {
						t.Errorf("RangeFirst returned key %v outside the requested interval", v)
						return
					}
				}
				_ = idx.Range(-100, 100)
				_ = idx.RangeFrom(0)
				card := idx.Cardinality(probe)
				if got := uint64(idx.Lookup(probe).GetCardinality()); got != card {
					// Both are single-key reads and both are atomic with
					// respect to that key, but they are two separate calls, so
					// they may straddle a write. Only an ordering violation
					// within one call would be a defect, which the race
					// detector catches; nothing to assert here.
					_ = got
				}
				_ = idx.LookupAppend(probe, nil)
				_ = idx.DistinctValues()
				readerOps.Add(1)
			}
		}(r)
	}

	// --- the detach / resurrect race, CONSTRUCTED not hoped for -------------
	//
	// The pool workload above never empties a key — every key keeps several
	// node ids — so on its own it exercises the structural DETACH barely at
	// all, and the insert-versus-detach race not at once. That was measured:
	// with the detach's emptiness re-check deleted, the pool workload alone
	// stayed green over three runs.
	//
	// So the race is built explicitly. Each pair gets a private key holding
	// exactly ONE node id. Two goroutines are released together on a barrier:
	// one DELETES that id (emptying the key, which forces the structural
	// detach) while the other INSERTS a second id that is never removed. Both
	// orders are legal and both must end with the key holding exactly the
	// second id:
	//
	//   - the inserter takes the entry lock first  -> the detach must ABORT,
	//   - the detach takes it first and marks the entry dead -> the inserter
	//     must notice and re-create the key.
	//
	// A protocol that drops either check loses the surviving id, and because
	// that id is never deleted the loss is still visible in the final identity
	// comparison. The pool writers above are still running, so i.mu is under
	// contention throughout, which is what stretches the window between the
	// set emptying and the detach acquiring i.mu.
	const (
		raceRounds = 200
		racePairs  = 32
		delBase    = uint64(1) << 40
		permBase   = uint64(1) << 41
	)
	raceValue := func(n int) float64 { return 2000.0 + float64(n)*0.5 }
	for round := 0; round < raceRounds; round++ {
		start := make(chan struct{})
		var pairs sync.WaitGroup
		for p := 0; p < racePairs; p++ {
			n := round*racePairs + p
			v := raceValue(n)
			victim := delBase + uint64(n)
			survivor := permBase + uint64(n)
			// Seed the key with exactly one id, so deleting it empties the key.
			idx.Insert(v, graph.NodeID(victim))
			ref.insert(v, victim)
			pairs.Add(2)
			go func() {
				defer pairs.Done()
				<-start
				idx.Delete(v, graph.NodeID(victim))
				ref.remove(v, victim)
			}()
			go func() {
				defer pairs.Done()
				<-start
				idx.Insert(v, graph.NodeID(survivor))
				ref.insert(v, survivor)
			}()
		}
		close(start)
		pairs.Wait()
	}

	// Quiesce: the writers finish on their own, the readers stop when told.
	writersWG.Wait()
	close(done)
	wg.Wait()

	if readerOps.Load() == 0 {
		t.Fatal("no reader observations were taken: the concurrent phase was vacuous")
	}

	// --- the workload really happened -------------------------------------
	snap := ref.snapshot()
	if len(snap) < 100 {
		t.Fatalf("reference model holds only %d keys; the workload was too small to be meaningful", len(snap))
	}
	var refNodes int
	sawNaN := false
	for _, e := range snap {
		refNodes += len(e.nodes)
		if math.IsNaN(e.key) {
			sawNaN = true
		}
	}
	if refNodes < 1000 {
		t.Fatalf("reference model holds only %d node ids; the workload was too small", refNodes)
	}
	if !sawNaN {
		t.Fatal("reference model holds no NaN key: the NaN corner of the total order went untested")
	}

	// --- identity ----------------------------------------------------------
	if got, want := idx.DistinctValues(), ref.distinct(); got != want {
		t.Errorf("DistinctValues = %d, want %d", got, want)
	}

	nan := math.NaN()
	ranges := []struct {
		name   string
		lo, hi float64
	}{
		{"nan-lower-bound-admits-nan", nan, math.Inf(1)},
		{"nan-to-zero", nan, 0},
		{"infinite-non-nan-excludes-nan", math.Inf(-1), math.Inf(1)},
		{"finite-wide", -1e308, 1e308},
		{"narrow-negative", -20, -5},
		{"narrow-positive", 3.25, 17.75},
		{"across-zero", -0.5, 0.5},
		{"degenerate-point", 1.5, 1.5},
		{"inverted", 10, -10},
		{"above-everything", 1e307, 1e308},
	}
	for _, rg := range ranges {
		t.Run("Range/"+rg.name, func(t *testing.T) {
			got := bitmapToSorted(idx.Range(rg.lo, rg.hi))
			want := ref.rangeNodes(rg.lo, rg.hi)
			if !slices.Equal(got, want) {
				t.Fatalf("Range(%v,%v): %d ids, want %d (first divergence at %s)",
					rg.lo, rg.hi, len(got), len(want), firstDiff(got, want))
			}
			// RangeCount must agree with the union it gates (the sets are
			// pairwise disjoint, so the sum IS the union cardinality).
			cnt, exact := idx.RangeCount(rg.lo, rg.hi, math.MaxUint64)
			if !exact || cnt != uint64(len(want)) {
				t.Fatalf("RangeCount(%v,%v) = (%d,%v), want (%d,true)", rg.lo, rg.hi, cnt, exact, len(want))
			}
			gotV, gotN, gotOK := idx.RangeFirst(rg.lo, rg.hi)
			wantV, wantN, wantOK := ref.rangeFirst(rg.lo, rg.hi)
			if gotOK != wantOK || uint64(gotN) != wantN || normBits(gotV) != normBits(wantV) {
				t.Fatalf("RangeFirst(%v,%v) = (%v,%d,%v), want (%v,%d,%v)",
					rg.lo, rg.hi, gotV, gotN, gotOK, wantV, wantN, wantOK)
			}
		})
	}

	// The NaN key must be reachable ONLY through a NaN lower bound.
	nanIDs := ref.lookup(nan)
	if len(nanIDs) == 0 {
		t.Fatal("reference model lost the NaN key")
	}
	fromNaN := idx.Range(nan, math.Inf(1))
	for _, id := range nanIDs {
		if !fromNaN.Contains(id) {
			t.Fatalf("Range(NaN, +Inf) omits NaN-keyed node %d", id)
		}
	}
	fromNegInf := idx.Range(math.Inf(-1), math.Inf(1))
	for _, id := range nanIDs {
		if fromNegInf.Contains(id) {
			t.Fatalf("Range(-Inf, +Inf) returned NaN-keyed node %d; NaN sorts below -Inf", id)
		}
	}

	for _, lo := range []float64{nan, math.Inf(-1), -64, 0, 17.5} {
		got := bitmapToSorted(idx.RangeFrom(lo))
		want := ref.rangeFromNodes(lo)
		if !slices.Equal(got, want) {
			t.Fatalf("RangeFrom(%v): %d ids, want %d (first divergence at %s)",
				lo, len(got), len(want), firstDiff(got, want))
		}
		cnt, exact := idx.RangeCountFrom(lo, math.MaxUint64)
		if !exact || cnt != uint64(len(want)) {
			t.Fatalf("RangeCountFrom(%v) = (%d,%v), want (%d,true)", lo, cnt, exact, len(want))
		}
	}

	// Every key the reference holds must answer identically to a point read,
	// and every pool value the reference does NOT hold must read as absent.
	for _, e := range snap {
		got := bitmapToSorted(idx.Lookup(e.key))
		if !slices.Equal(got, e.nodes) {
			t.Fatalf("Lookup(%v) = %v, want %v", e.key, got, e.nodes)
		}
		if c := idx.Cardinality(e.key); c != uint64(len(e.nodes)) {
			t.Fatalf("Cardinality(%v) = %d, want %d", e.key, c, len(e.nodes))
		}
		if app := idx.LookupAppend(e.key, nil); !slices.Equal(app, e.nodes) {
			t.Fatalf("LookupAppend(%v) = %v, want %v", e.key, app, e.nodes)
		}
	}
	live := make(map[uint64]struct{}, len(snap))
	for _, e := range snap {
		live[normBits(e.key)] = struct{}{}
	}
	absent := 0
	for _, v := range pool {
		if _, ok := live[normBits(v)]; ok {
			continue
		}
		absent++
		if c := idx.Cardinality(v); c != 0 {
			t.Fatalf("Cardinality(%v) = %d for a value the reference does not hold", v, c)
		}
		if bm := idx.Lookup(v); !bm.IsEmpty() {
			t.Fatalf("Lookup(%v) returned %d ids for a value the reference does not hold", v, bm.GetCardinality())
		}
	}

	// Every constructed detach/resurrect race must have ended with the
	// surviving id present and the victim gone. This is the assertion the pool
	// workload cannot make, stated per race so a failure names the pair.
	for n := 0; n < raceRounds*racePairs; n++ {
		v := raceValue(n)
		want := []uint64{permBase + uint64(n)}
		if got := bitmapToSorted(idx.Lookup(v)); !slices.Equal(got, want) {
			t.Fatalf("detach/resurrect race %d on key %v: Lookup = %v, want %v", n, v, got, want)
		}
	}

	if !sortedInvariantHolds(idx) {
		t.Fatal("sorted invariant broken after the concurrent workload")
	}
}

// firstDiff renders the first position at which two ascending id slices
// diverge, so a failure names the id rather than dumping thousands of them.
func firstDiff(got, want []uint64) string {
	n := min(len(got), len(want))
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
	switch {
	case len(got) == len(want):
		return "no divergence"
	case len(got) < len(want):
		return fmt.Sprintf("index %d: got nothing, want %d", n, want[n])
	default:
		return fmt.Sprintf("index %d: got %d, want nothing", n, got[n])
	}
}

// --- 2. the bracketing oracle ----------------------------------------------

// TestConcurrentRangeObservationsAreBracketed checks every scan taken DURING
// the concurrent phase against the two populations that bound it: the initial
// one, which is present for the whole run and must therefore appear in every
// observation, and the final one, outside which no observation may stray.
//
// This is the oracle that can actually catch a torn read. A scan that lost a
// whole subtree to a concurrent split still looks perfectly well formed —
// ascending, no duplicates, keys inside the interval — so a shape check would
// pass on it. Missing an initial id would not.
//
// The oracle is made non-vacuous BY CONSTRUCTION rather than by hoping the
// scheduler interleaves: one writer is paced against the readers, publishing
// one id and then waiting for a reader observation before publishing the next.
// The test fails if it nevertheless ends without having seen a strictly
// intermediate population, because that would mean the whole check ran against
// either the initial or the final state and proved nothing about concurrency.
func TestConcurrentRangeObservationsAreBracketed(t *testing.T) {
	t.Parallel()

	const (
		lo             = -1000.0
		hi             = 1000.0
		initialKeys    = 300
		pacedInserts   = 250
		freeWriters    = 4
		freePerWriter  = 1500
		readers        = 6
		paceSpinBudget = 1 << 22
	)

	idx := New[float64]()

	// Initial population: keys strictly inside [lo,hi], node ids 1..initialKeys.
	// Never deleted, so every observation must contain all of them.
	initialBM := roaring64.New()
	for k := 0; k < initialKeys; k++ {
		v := float64(k) - 150.0
		id := uint64(k + 1)
		idx.Insert(v, graph.NodeID(id))
		initialBM.Add(id)
	}

	// The universe the writers will add, all inside [lo,hi]. Every id is known
	// up front, so an observation may be checked against the FINAL population
	// before the writers have finished producing it.
	finalBM := initialBM.Clone()
	const pacedBase uint64 = 100_000
	pacedValue := func(j int) float64 { return float64(j)*0.5 - 400.0 }
	for j := 0; j < pacedInserts; j++ {
		finalBM.Add(pacedBase + uint64(j))
	}
	const freeBase uint64 = 1_000_000
	freeValue := func(w, j int) float64 {
		return float64((w*freePerWriter+j)%1800)*0.5 - 450.0
	}
	for w := 0; w < freeWriters; w++ {
		for j := 0; j < freePerWriter; j++ {
			finalBM.Add(freeBase + uint64(w*freePerWriter+j))
		}
	}
	initialCard := initialBM.GetCardinality()
	finalCard := finalBM.GetCardinality()

	var (
		observations  atomic.Uint64
		intermediate  atomic.Uint64
		atFinal       atomic.Uint64
		countProbes   atomic.Uint64
		liveReaders   atomic.Int64
		wg            sync.WaitGroup
		done          = make(chan struct{})
		writersActive sync.WaitGroup
	)

	failf := func(format string, args ...any) {
		t.Helper()
		t.Errorf(format, args...)
	}

	liveReaders.Store(readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The paced writer waits on these observations; tell it when this
			// reader stops so a failing run cannot leave it spinning out its
			// whole budget with nobody left to observe.
			defer liveReaders.Add(-1)
			for {
				select {
				case <-done:
					return
				default:
				}
				obs := idx.Range(lo, hi)

				// Lower bracket: the initial population is present for the
				// whole run, so no observation may miss any of it.
				if missing := roaring64.AndNot(initialBM, obs); !missing.IsEmpty() {
					failf("Range(%v,%v) lost %d id(s) of the always-present initial population (first: %d)",
						lo, hi, missing.GetCardinality(), missing.Minimum())
					return
				}
				// Upper bracket: nothing may appear that the workload never
				// inserts.
				if extra := roaring64.AndNot(obs, finalBM); !extra.IsEmpty() {
					failf("Range(%v,%v) returned %d id(s) outside the final population (first: %d)",
						lo, hi, extra.GetCardinality(), extra.Minimum())
					return
				}
				switch c := obs.GetCardinality(); {
				case c > initialCard && c < finalCard:
					intermediate.Add(1)
				case c == finalCard:
					atFinal.Add(1)
				}

				// The same bracket, on the count path, which walks the same
				// cursor without materialising a union.
				cnt, exact := idx.RangeCount(lo, hi, math.MaxUint64)
				if !exact {
					failf("RangeCount(%v,%v) reported inexact under an unbounded budget", lo, hi)
					return
				}
				if cnt < initialCard || cnt > finalCard {
					failf("RangeCount(%v,%v) = %d, outside the bracket [%d,%d]", lo, hi, cnt, initialCard, finalCard)
					return
				}
				countProbes.Add(1)
				observations.Add(1)
			}
		}()
	}

	// The paced writer: publish one id, then wait for at least one reader
	// observation before publishing the next. This is what turns "the readers
	// will probably catch an intermediate state" into "they are guaranteed to".
	writersActive.Add(1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer writersActive.Done()
		for j := 0; j < pacedInserts; j++ {
			idx.Insert(pacedValue(j), graph.NodeID(pacedBase+uint64(j)))
			start := observations.Load()
			for spin := 0; spin < paceSpinBudget; spin++ {
				if observations.Load() != start || liveReaders.Load() == 0 {
					break
				}
				runtime.Gosched()
			}
		}
	}()

	// Free writers: unpaced structural churn so the paced writer is not the
	// only thing rebuilding the spine.
	for w := 0; w < freeWriters; w++ {
		writersActive.Add(1)
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			defer writersActive.Done()
			for j := 0; j < freePerWriter; j++ {
				idx.Insert(freeValue(w, j), graph.NodeID(freeBase+uint64(w*freePerWriter+j)))
			}
		}(w)
	}

	writersActive.Wait()
	close(done)
	wg.Wait()

	if t.Failed() {
		return
	}
	if observations.Load() == 0 {
		t.Fatal("no observations were taken: the bracket oracle never ran")
	}
	if countProbes.Load() == 0 {
		t.Fatal("no RangeCount observations were taken")
	}
	if intermediate.Load() == 0 {
		t.Fatalf("every one of the %d observations saw either the initial or the final population; "+
			"the oracle never witnessed a concurrent state and therefore proved nothing "+
			"(paced observations: %d at final)", observations.Load(), atFinal.Load())
	}
	t.Logf("bracket oracle: %d observations, %d of them strictly between the initial (%d ids) "+
		"and final (%d ids) populations", observations.Load(), intermediate.Load(), initialCard, finalCard)

	// After quiescing, the index must hold exactly the final population.
	final := idx.Range(lo, hi)
	if !final.Equals(finalBM) {
		missing := roaring64.AndNot(finalBM, final)
		extra := roaring64.AndNot(final, finalBM)
		t.Fatalf("post-quiesce Range(%v,%v): %d missing, %d unexpected (want %d ids, got %d)",
			lo, hi, missing.GetCardinality(), extra.GetCardinality(), finalCard, final.GetCardinality())
	}
	if !sortedInvariantHolds(idx) {
		t.Fatal("sorted invariant broken after the bracketed workload")
	}
}
