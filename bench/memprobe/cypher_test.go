//go:build threeway

package memprobe

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The Cypher probes exist because the containerised comparison drives GoGraph
// through Cypher over Bolt, while the probes in edge_test.go drive the storage
// layer through the Go API. If those two disagree, the difference is the query
// path's own storage — the per-CREATE bookkeeping that openCypher's
// parallel-edge semantics require — and it belongs to neither the adjacency
// nor the caller.
//
// They are deliberately smaller than the Go-API probes: a Cypher CREATE is
// orders of magnitude slower than an AddEdge, and the quantity measured is a
// per-element slope that does not need a million elements to be visible.
const (
	probeCypherNodes = 100_000
	probeCypherDeg   = 8
	probeCypherEdges = probeCypherNodes * probeCypherDeg
)

// newCypherEngine builds the same engine configuration bench/comparison/ggserver
// serves over Bolt, so that a probe here describes the deployment the
// comparison measured.
func newCypherEngine() (*lpg.Graph[string, float64], *cypher.Engine) {
	return newCypherEngineMode(true)
}

// newCypherEngineMode is newCypherEngine with the parallel-edge decision
// exposed, so the per-handle stores that only multigraph mode populates can be
// switched off and their cost measured rather than estimated.
func newCypherEngineMode(multigraph bool) (*lpg.Graph[string, float64], *cypher.Engine) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: multigraph})
	g.SetIndexManager(index.NewManager())
	return g, cypher.NewEngine(g)
}

func cypherRun(t *testing.T, eng *cypher.Engine, q string, params map[string]any) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if res != nil {
		if err := res.Close(); err != nil {
			t.Fatalf("close %q: %v", q, err)
		}
	}
}

// cypherSeedNodes creates the node population and the seek index every edge
// probe joins through.
func cypherSeedNodesOn(t *testing.T, eng *cypher.Engine) { cypherSeedNodes(t, eng) }

func cypherSeedNodes(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	cypherRun(t, eng, `CREATE INDEX idx_Person_sid FOR (n:Person) ON (n.sid)`, nil)
	const batch = 1000
	for lo := 0; lo < probeCypherNodes; lo += batch {
		rows := make([]any, 0, batch)
		for i := lo; i < lo+batch && i < probeCypherNodes; i++ {
			rows = append(rows, map[string]any{"sid": fmt.Sprintf("p%08d", i)})
		}
		cypherRun(t, eng, `UNWIND $rows AS r CREATE (:Person {sid: r.sid})`, map[string]any{"rows": rows})
	}
}

// TestProbe_CypherNodes measures a node created through Cypher, against the
// Go-API figure from TestProbe_NodesWithLabelAndProp.
func TestProbe_CypherNodes(t *testing.T) {
	measure(t, "cypher/nodes+label+1prop", probeCypherNodes, func() any {
		g, eng := newCypherEngine()
		cypherSeedNodes(t, eng)
		return []any{g, eng}
	})
}

// TestProbe_CypherEdges measures a typed relationship created through Cypher.
//
// The Go API's AddEdge writes only the adjacency. A Cypher CREATE must also
// satisfy openCypher's requirement that two CREATEs between the same pair of
// nodes yield two distinct relationships, and graph/lpg/lpg.go carries five
// further sharded stores for exactly that: edgeCreateCountShards,
// edgeInstanceLabelShards, edgeInstancePropShards, edgeHandleLabelShards and
// edgeHandlePropShards. This probe is what says whether they are free.
func TestProbe_CypherEdges(t *testing.T) {
	measure(t, "cypher/typed-edges", probeCypherEdges, func() any {
		g, eng := newCypherEngine()
		cypherSeedNodes(t, eng)
		const batch = 200
		for lo := 0; lo < probeCypherNodes; lo += batch {
			rows := make([]any, 0, batch*probeCypherDeg)
			for i := lo; i < lo+batch && i < probeCypherNodes; i++ {
				for k := 1; k <= probeCypherDeg; k++ {
					dst := (i*31 + k*2654435761) % probeCypherNodes
					rows = append(rows, map[string]any{
						"ss": fmt.Sprintf("p%08d", i), "ts": fmt.Sprintf("p%08d", dst),
					})
				}
			}
			cypherRun(t, eng,
				`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS]->(b)`,
				map[string]any{"rows": rows})
		}
		return []any{g, eng}
	})
}

// TestProbe_CypherEdgesBaseline is TestProbe_CypherEdges without the edges, so
// the difference between the two is the per-relationship cost of the Cypher
// write path.
func TestProbe_CypherEdgesBaseline(t *testing.T) {
	measure(t, "cypher/edge-baseline-nodes-only", probeCypherNodes, func() any {
		g, eng := newCypherEngine()
		cypherSeedNodes(t, eng)
		return []any{g, eng}
	})
}

// TestProbe_CypherEdgesSimpleGraph is TestProbe_CypherEdges with parallel
// edges disallowed. graph/lpg/lpg.go documents the per-handle stores as
// "Populated only in multigraph mode (one handle per CREATE)", so the
// difference between the two probes is what openCypher's parallel-edge
// semantics cost per relationship on the Cypher write path.
//
// It is a measurement, not a proposal: a simple graph cannot represent two
// relationships between the same pair, which openCypher requires.
func TestProbe_CypherEdgesSimpleGraph(t *testing.T) {
	measure(t, "cypher/typed-edges-simple", probeCypherEdges, func() any {
		g, eng := newCypherEngineMode(false)
		cypherSeedNodesOn(t, eng)
		const batch = 200
		for lo := 0; lo < probeCypherNodes; lo += batch {
			rows := make([]any, 0, batch*probeCypherDeg)
			for i := lo; i < lo+batch && i < probeCypherNodes; i++ {
				for k := 1; k <= probeCypherDeg; k++ {
					dst := (i*31 + k*2654435761) % probeCypherNodes
					rows = append(rows, map[string]any{
						"ss": fmt.Sprintf("p%08d", i), "ts": fmt.Sprintf("p%08d", dst),
					})
				}
			}
			cypherRun(t, eng,
				`UNWIND $rows AS r MATCH (a:Person {sid: r.ss}), (b:Person {sid: r.ts}) CREATE (a)-[:KNOWS]->(b)`,
				map[string]any{"rows": rows})
		}
		return []any{g, eng}
	})
}
