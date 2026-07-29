package cypher_test

// csr_pair_cache_test.go — invalidation correctness for the cross-query CSR pair
// cache (rmp #2143).
//
// A cache over a CSR is a correctness hazard before it is a performance feature:
// serve a stale pair and the query returns rows for a topology that no longer
// exists. The cache is keyed on lpg.Graph.TopoGeneration, so every one of these
// tests asserts a RESULT CHANGE across a mutation rather than inspecting cache
// internals — that is the property that matters, and it holds regardless of how
// the key is implemented.
//
// The tombstone case is the load-bearing one: csrPairFromGraph builds
// LIVE-FILTERED (#1790), and RemoveNode did not bump TopoGeneration before this
// change, so once a tombstone stripped a node's arcs a cached pair could keep
// serving them. The TopoGeneration assertions in
// TestCSRPairCache_TransparentAcrossTombstoneTransitions fail on the pre-change lpg.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// cacheTestEngine builds a directed multigraph engine with a small chain plus a
// hub, over which every test below runs its queries on ONE shared Engine so the
// cache is genuinely exercised across queries.
func cacheTestEngine(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	mustRunWrite(t, eng, `CREATE (a:N {key:'a'})`)
	for i := 0; i < 5; i++ {
		mustRunWrite(t, eng, fmt.Sprintf(`CREATE (b:N {key:'b%d'})`, i))
		mustRunWrite(t, eng, fmt.Sprintf(
			`MATCH (a:N {key:'a'}),(b:N {key:'b%d'}) CREATE (a)-[:R]->(b)`, i))
	}
	return eng, g
}

const cacheCountQuery = `MATCH (:N {key:'a'})-[r:R]->() RETURN count(r) AS c`

func TestCSRPairCache_EdgeAddInvalidates(t *testing.T) {
	t.Parallel()
	eng, _ := cacheTestEngine(t)

	if got := countScalar(t, eng, cacheCountQuery); got != 5 {
		t.Fatalf("initial count = %d, want 5", got)
	}
	// Same Engine, so the second query would hit a cached pair if the epoch had
	// not advanced. Adding an edge must advance it.
	mustRunWrite(t, eng, `CREATE (b:N {key:'b9'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'a'}),(b:N {key:'b9'}) CREATE (a)-[:R]->(b)`)
	if got := countScalar(t, eng, cacheCountQuery); got != 6 {
		t.Errorf("count after adding an edge = %d, want 6: the CSR pair cache served a stale topology", got)
	}
}

func TestCSRPairCache_EdgeDeleteInvalidates(t *testing.T) {
	t.Parallel()
	eng, _ := cacheTestEngine(t)

	if got := countScalar(t, eng, cacheCountQuery); got != 5 {
		t.Fatalf("initial count = %d, want 5", got)
	}
	mustRunWrite(t, eng, `MATCH (:N {key:'a'})-[r:R]->(:N {key:'b0'}) DELETE r`)
	if got := countScalar(t, eng, cacheCountQuery); got != 4 {
		t.Errorf("count after deleting an edge = %d, want 4: the CSR pair cache served a stale topology", got)
	}
}

// TestCSRPairCache_TransparentAcrossTombstoneTransitions pins the property that
// actually matters for a cache: TRANSPARENCY. Whatever the engine answers for a
// given graph state, a shared Engine carrying a warm cache must answer identically
// to a FRESH Engine that has none.
//
// It is written this way deliberately. An earlier draft asserted a specific count
// after a bare lpg.Graph.RemoveNode and was WRONG: RemoveNode only tombstones, and
// its own contract says callers "should also strip labels / properties / incident
// edges" first, so the engine legitimately still reports the un-stripped edge —
// verified by bypassing the cache entirely and observing the same answer. Pinning
// the guessed count would have asserted behaviour the engine never had; pinning
// cached-equals-uncached catches staleness without needing to settle what the
// right count is.
//
// Both tombstone transitions are covered because the tombstone COUNT is identical
// before and after a remove-then-revive pair while the live set differs, so a cache
// keyed on that count instead of a monotonic generation would wrongly hit.
func TestCSRPairCache_TransparentAcrossTombstoneTransitions(t *testing.T) {
	t.Parallel()
	eng, g := cacheTestEngine(t)

	// Warm the cache.
	if got := countScalar(t, eng, cacheCountQuery); got != 5 {
		t.Fatalf("initial count = %d, want 5", got)
	}

	assertTransparent := func(stage string) {
		t.Helper()
		cached := countScalar(t, eng, cacheCountQuery)
		fresh := countScalar(t, cypher.NewEngine(g), cacheCountQuery)
		if cached != fresh {
			t.Errorf("%s: warm-cache engine returned %d but a fresh engine returned %d — "+
				"the CSR pair cache is serving a stale topology", stage, cached, fresh)
		}
	}

	// The engine keys nodes by an internal identifier; {key:'b0'} is a PROPERTY, so
	// the node's key has to be resolved before it can be removed. Passing "b0"
	// directly makes RemoveNode a silent no-op.
	b0 := keyNode(t, g, "b0")
	before := g.TopoGeneration()
	g.RemoveNode(b0)
	if after := g.TopoGeneration(); after == before {
		t.Error("RemoveNode did not advance TopoGeneration, so a CSR pair cache keyed " +
			"on it cannot notice a tombstone")
	}
	assertTransparent("after RemoveNode")

	before = g.TopoGeneration()
	if err := g.AddNode(b0); err != nil { // revives the tombstone
		t.Fatal(err)
	}
	if after := g.TopoGeneration(); after == before {
		t.Error("reviving a tombstoned node did not advance TopoGeneration")
	}
	assertTransparent("after reviving")
}

// TestCSRPairCache_PropertyWriteDoesNotBreakResults pins the other side of the
// invariant: a NON-topological write must not corrupt results. Whether it
// invalidates is an implementation choice; returning the right answer is not.
func TestCSRPairCache_PropertyWriteDoesNotBreakResults(t *testing.T) {
	t.Parallel()
	eng, _ := cacheTestEngine(t)

	if got := countScalar(t, eng, cacheCountQuery); got != 5 {
		t.Fatalf("initial count = %d, want 5", got)
	}
	mustRunWrite(t, eng, `MATCH (a:N {key:'a'}) SET a.tag = 'x'`)
	if got := countScalar(t, eng, cacheCountQuery); got != 5 {
		t.Errorf("count after a property write = %d, want 5", got)
	}
}

// TestCSRPairCache_ConcurrentQueriesAgree drives the cache from many goroutines at
// once, interleaved with topology writes, and requires every observed count to be
// one of the legitimate values. Run under -race this covers the cache's mutex and
// the read path's lock-free CSR access together.
func TestCSRPairCache_ConcurrentQueriesAgree(t *testing.T) {
	t.Parallel()
	eng, _ := cacheTestEngine(t)

	var wg sync.WaitGroup
	// Errors are collected under a mutex rather than sent to a buffered channel:
	// a channel that nothing drains until wg.Wait() deadlocks the producers the
	// moment they exceed its capacity, which turns a failing assertion into a
	// test hang.
	var mu sync.Mutex
	var errs []string
	fail := func(msg string) {
		mu.Lock()
		if len(errs) < 32 { // bounded: enough to diagnose, no unbounded growth
			errs = append(errs, msg)
		}
		mu.Unlock()
	}
	seen := map[int64]int{}
	note := func(n int64) {
		mu.Lock()
		seen[n]++
		mu.Unlock()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				res, err := eng.RunAny(context.Background(), cacheCountQuery, nil)
				if err != nil {
					fail("run: " + err.Error())
					return
				}
				var n int64
				for res.Next() {
					if v, ok := res.Record()["c"].(expr.IntegerValue); ok {
						n = int64(v)
					} else {
						fail(fmt.Sprintf("column c: got %T, want expr.IntegerValue", res.Record()["c"]))
					}
				}
				if err := res.Err(); err != nil {
					fail("err: " + err.Error())
				}
				res.Close()
				note(n)
				// 5 initially; 6 once the writer below has committed its edge.
				if n != 5 && n != 6 {
					fail(fmt.Sprintf("count = %d, want 5 or 6", n))
				}
			}
		}()
	}
	// One concurrent topology write, which must invalidate mid-flight.
	mustRunWrite(t, eng, `CREATE (b:N {key:'bx'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'a'}),(b:N {key:'bx'}) CREATE (a)-[:R]->(b)`)
	wg.Wait()
	for _, e := range errs {
		t.Error(e)
	}
	if t.Failed() {
		t.Logf("observed counts: %v", seen)
	}

	// After the writer, every reader must see the new topology.
	if got := countScalar(t, eng, cacheCountQuery); got != 6 {
		t.Errorf("final count = %d, want 6", got)
	}
}
