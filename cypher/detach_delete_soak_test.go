//go:build soak

package cypher_test

// detach_delete_soak_test.go — DETACH DELETE hub with 1 million leaves (T848, soak layer).
//
// Activated by: go test -tags=soak ./cypher/...
// Not part of the short-layer (PR-CI) run.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestDetachDelete_Hub1M_Soak(t *testing.T) {
	const leaves = 1_000_000

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:HubM {name: "hugeHub"})`)
	hubKey := synthKeyForLabel(t, g, "HubM")

	for i := range leaves {
		lk := fmt.Sprintf("m%d", i)
		if err := g.AddNode(lk); err != nil {
			t.Fatalf("AddNode %d: %v", i, err)
		}
		if err := g.AddEdge(hubKey, lk, 1.0); err != nil {
			t.Fatalf("AddEdge hub->%d: %v", i, err)
		}
	}

	drainRunInTx(t, eng, `MATCH (n:HubM) DETACH DELETE n`)

	// Hub must be absent from the label index.
	lid, ok := g.Registry().Lookup("HubM")
	if ok {
		bm := g.NodeIndex().Intersect(uint32(lid))
		if !bm.IsEmpty() {
			t.Fatal("hub still in HubM label index after DETACH DELETE")
		}
	}

	// Spot-check: first and last leaf must still be reachable in the mapper.
	alive := 0
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if len(key) >= 1 && key[0] == 'm' {
			alive++
		}
		return true
	})
	if alive != leaves {
		t.Errorf("expected %d leaves alive after DETACH DELETE, got %d", leaves, alive)
	}
}
