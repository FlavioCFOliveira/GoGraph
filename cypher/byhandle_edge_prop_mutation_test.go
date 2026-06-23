package cypher_test

// byhandle_edge_prop_mutation_test.go — engine-level coverage for the
// per-instance (by-handle) edge-property store maintained on a relationship
// SET / REMOVE in multigraph mode (#1686), driving the PUBLIC Cypher engine
// over a real lpg.Graph (so the actual forward-CSR handle resolution and the
// undo log are exercised, not a stub).
//
// The openCypher model is a multigraph: two CREATEs between the same ordered
// pair yield two distinct relationships, each with its own properties. A SET on
// one bound instance must affect only that instance's by-handle store; the
// sibling must be untouched. A failed write must roll the by-handle change back
// (Atomicity).
//
// Layer: short. goleak-clean (engines/graphs are local).

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// inMemMultigraphEngine builds a store-less (in-memory) engine over a fresh
// directed multigraph, exercising the lpgMutatorAdapter write path.
func inMemMultigraphEngine(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g), g
}

// byHandlePropsForXY returns, for the (:N{key:'x'}) -> (:N{key:'y'}) ordered
// pair, the property map of each parallel instance keyed by stable handle, read
// DIRECTLY from the by-handle store (the read path is not rerouted in #1686).
func byHandlePropsForXY(t *testing.T, g *lpg.Graph[string, float64]) map[uint64]map[string]lpg.PropertyValue {
	t.Helper()
	srcID, ok := g.AdjList().Mapper().Lookup(keyNode(t, g, "x"))
	if !ok {
		t.Fatal("x not found")
	}
	dstID, ok := g.AdjList().Mapper().Lookup(keyNode(t, g, "y"))
	if !ok {
		t.Fatal("y not found")
	}
	out := make(map[uint64]map[string]lpg.PropertyValue)
	g.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
		if tr.Src == srcID && tr.Dst == dstID {
			out[tr.Handle] = g.EdgePropertiesByHandleID(srcID, dstID, tr.Handle)
		}
		return true
	})
	return out
}

func mustRunWrite(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	if err := runWrite(t, eng, q); err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
}

// TestByHandle_InMemory_SetOnOneParallelEdge_SiblingUntouched is the core
// sibling-isolation property: a SET on the USES instance must not corrupt the
// by-handle properties of the parallel CALLS instance.
func TestByHandle_InMemory_SetOnOneParallelEdge_SiblingUntouched(t *testing.T) {
	t.Parallel()
	eng, g := inMemMultigraphEngine(t)
	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2}]->(b)`)

	// SET a new property on exactly the USES instance.
	mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) SET r.tag = 'on-uses'`)

	perInstance := byHandlePropsForXY(t, g)
	if len(perInstance) != 2 {
		t.Fatalf("expected 2 parallel by-handle instances, got %d: %v", len(perInstance), perInstance)
	}
	var sawUses, sawCalls bool
	for _, props := range perInstance {
		_, hasTag := props["tag"]
		iv, _ := props["w"].Int64()
		switch iv {
		case 1: // USES
			sawUses = true
			if !hasTag {
				t.Fatalf("USES instance lost its SET tag: %v", props)
			}
			if s, _ := props["tag"].String(); s != "on-uses" {
				t.Fatalf("USES tag = %q, want on-uses", s)
			}
		case 2: // CALLS sibling
			sawCalls = true
			if hasTag {
				t.Fatalf("CALLS sibling corrupted with the USES tag: %v", props)
			}
		}
	}
	if !sawUses || !sawCalls {
		t.Fatalf("expected both instances present: sawUses=%v sawCalls=%v; full=%v", sawUses, sawCalls, perInstance)
	}
}

// TestByHandle_InMemory_RemoveOnOneParallelEdge_SiblingUntouched mirrors the SET
// test for REMOVE r.x.
func TestByHandle_InMemory_RemoveOnOneParallelEdge_SiblingUntouched(t *testing.T) {
	t.Parallel()
	eng, g := inMemMultigraphEngine(t)
	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1, tag:'keep'}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2, tag:'keep'}]->(b)`)

	mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) REMOVE r.tag`)

	perInstance := byHandlePropsForXY(t, g)
	for _, props := range perInstance {
		iv, _ := props["w"].Int64()
		_, hasTag := props["tag"]
		switch iv {
		case 1: // USES — tag removed
			if hasTag {
				t.Fatalf("USES instance still carries tag after REMOVE: %v", props)
			}
		case 2: // CALLS sibling — tag retained
			if !hasTag {
				t.Fatalf("CALLS sibling lost its tag after REMOVE on USES: %v", props)
			}
		}
	}
}

// TestByHandle_RollbackRevertsByHandle is the C3 atomicity test: a SET that
// fails on a later row must roll back the by-handle write applied to the earlier
// instance. The nthSetRejector rejects the 2nd `tag` write, so the SET applied
// to the first matched parallel edge (its by-handle write included) must be
// undone when the statement errors.
func TestByHandle_RollbackRevertsByHandle(t *testing.T) {
	eng, g, w, _ := walMultigraphEngineWithGraph(t)
	defer w.Close()

	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2}]->(b)`)

	// Snapshot the by-handle state BEFORE the failing write so we can assert
	// byte-for-byte restoration.
	before := byHandlePropsForXY(t, g)

	// Reject the 2nd `tag` write: the SET binds both parallel edges; the first
	// applies eagerly (per-pair + by-handle), the second is rejected, and the
	// whole statement must roll back.
	g.SetValidator(&nthSetRejector{key: "tag", rejN: 2})
	err := runWrite(t, eng, `MATCH (:N {key:'x'})-[r]->(:N {key:'y'}) SET r.tag = 'boom'`)
	if err == nil {
		t.Fatal("expected the write to error on the 2nd SET, got nil")
	}
	g.SetValidator(nil)

	// No parallel instance may carry tag: the first instance's by-handle write
	// was rolled back inside the barrier.
	after := byHandlePropsForXY(t, g)
	for h, props := range after {
		if _, hasTag := props["tag"]; hasTag {
			t.Fatalf("by-handle tag survived a rolled-back SET on handle %d: %v", h, props)
		}
	}
	// And the by-handle state must equal the pre-write snapshot exactly.
	if !sameByHandle(before, after) {
		t.Fatalf("by-handle state not restored to pre-write image:\n before=%v\n after =%v", before, after)
	}
}

// TestByHandle_RollbackRevertsByHandle_Remove is the REMOVE analogue: a failed
// statement that already removed a by-handle property on an earlier instance
// must restore it.
func TestByHandle_RollbackRevertsByHandle_Remove(t *testing.T) {
	eng, g, w, _ := walMultigraphEngineWithGraph(t)
	defer w.Close()

	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1, tag:'keep'}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2, tag:'keep'}]->(b)`)

	before := byHandlePropsForXY(t, g)

	// REMOVE r.tag then SET r.x — fail on the SET so the prior REMOVE on the
	// same instance must be reverted. A validator that rejects the FIRST `x`
	// write fails the SET clause after the REMOVE clause already ran for that
	// row's instance.
	g.SetValidator(&nthSetRejector{key: "x", rejN: 1})
	err := runWrite(t, eng, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) REMOVE r.tag SET r.x = 1`)
	if err == nil {
		t.Fatal("expected the write to error on the SET after REMOVE, got nil")
	}
	g.SetValidator(nil)

	after := byHandlePropsForXY(t, g)
	if !sameByHandle(before, after) {
		t.Fatalf("by-handle state not restored after rolled-back REMOVE+SET:\n before=%v\n after =%v", before, after)
	}
}

// sameByHandle reports whether two per-instance by-handle snapshots carry the
// same set of (handle → {key → stringified value}) entries.
func sameByHandle(a, b map[uint64]map[string]lpg.PropertyValue) bool {
	if len(a) != len(b) {
		return false
	}
	for h, ap := range a {
		bp, ok := b[h]
		if !ok || len(ap) != len(bp) {
			return false
		}
		for k, av := range ap {
			bv, ok := bp[k]
			if !ok {
				return false
			}
			if propStr(av) != propStr(bv) {
				return false
			}
		}
	}
	return true
}

// TestByHandle_ConcurrentViewReaders_NoRace runs many lock-free Graph.View
// readers over the by-handle store concurrently with a writer that repeatedly
// SETs and REMOVEs a property on ONE parallel relationship instance. Under
// `go test -race` it must show no data race: by-handle writes happen inside the
// ApplyAtomically barrier (which Engine.RunInTx takes), and View readers see a
// consistent snapshot. This guards the per-instance store's sharded-mutex
// concurrency contract end-to-end.
//
// Layer: soak (concurrency stress; the deterministic sibling-isolation tests
// above carry the short-layer coverage).
func TestByHandle_ConcurrentViewReaders_NoRace(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()
	eng, g := inMemMultigraphEngine(t)
	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:USES {w:1}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:CALLS {w:2}]->(b)`)

	srcID, _ := g.AdjList().Mapper().Lookup(keyNode(t, g, "x"))
	dstID, _ := g.AdjList().Mapper().Lookup(keyNode(t, g, "y"))

	const (
		readers    = 8
		iterations = 300
	)
	var (
		readersWG sync.WaitGroup
		stop      atomic.Bool
	)
	readersWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				// Lock-free read of the by-handle store, bracketed in View so the
				// cross-store read sees a consistent, non-partial snapshot.
				g.View(func() {
					g.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
						if tr.Src == srcID && tr.Dst == dstID {
							_ = g.EdgePropertiesByHandleID(srcID, dstID, tr.Handle)
						}
						return true
					})
				})
			}
		}()
	}

	// Writer: flip a property on the USES instance on and off repeatedly. Each
	// statement commits under the visibility barrier.
	for i := 0; i < iterations; i++ {
		if i%2 == 0 {
			mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) SET r.tag = 'on'`)
		} else {
			mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:USES]->(:N {key:'y'}) REMOVE r.tag`)
		}
	}
	stop.Store(true)
	readersWG.Wait()
}

// propStr renders a property value to a stable comparable string.
func propStr(v lpg.PropertyValue) string {
	if s, ok := v.String(); ok {
		return "s:" + s
	}
	if i, ok := v.Int64(); ok {
		return "i:" + strconv.FormatInt(i, 10)
	}
	return "?"
}
