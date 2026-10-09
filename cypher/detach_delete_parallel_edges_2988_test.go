package cypher_test

// detach_delete_parallel_edges_2988_test.go — rmp #2988.
//
// DETACH DELETE of a node with two parallel incoming relationships from one
// source left one adjacency arc behind: Cypher counted zero relationships, but
// the source still listed the deleted node as an out-neighbour and the deleted
// node still listed the source as an in-neighbour. The in-edge index names each
// source ONCE, and the removal loop took out one slot per name.
//
// Every case creates parallel instances of one shape around the Hub, DETACH
// DELETEs the Hub, reclaims, and compares the raw versioned adjacency — not
// Cypher, which hid the defect — with a control graph built from the same
// statements minus every relationship that touches the Hub.
//
// Layer: short. No goroutines are spawned.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const (
	parallelSetup  = "CREATE (:Hub {id:1}), (:X {id:53})"
	parallelDetach = "MATCH (h:Hub {id:1}) DETACH DELETE h"
	hubIn          = "MATCH (x:X {id:53}), (h:Hub {id:1}) CREATE (x)-[:R]->(h)"
	hubOut         = "MATCH (x:X {id:53}), (h:Hub {id:1}) CREATE (h)-[:R]->(x)"
	hubLoop        = "MATCH (h:Hub {id:1}) CREATE (h)-[:R]->(h)"
	xLoop          = "MATCH (x:X {id:53}) CREATE (x)-[:R]->(x)" // survives the DETACH DELETE
)

// parallelDetachCase is one parallel-edge shape around the Hub that is deleted.
type parallelDetachCase struct {
	name  string
	edges []string // CREATE statements run after the two nodes exist
}

// survivors returns the statements of c that do not touch the Hub: the
// relationships a correct DETACH DELETE leaves behind.
func (c parallelDetachCase) survivors() []string {
	var out []string
	for _, q := range c.edges {
		if q == xLoop {
			out = append(out, q)
		}
	}
	return out
}

func parallelDetachCases() []parallelDetachCase {
	return []parallelDetachCase{
		{name: "incoming_x2", edges: []string{hubIn, hubIn}},
		{name: "incoming_x3", edges: []string{hubIn, hubIn, hubIn}},
		{name: "outgoing_x2", edges: []string{hubOut, hubOut}},
		{name: "self_loop_x2", edges: []string{hubLoop, hubLoop}},
		{name: "mixed_both_directions_and_loops", edges: []string{hubIn, hubOut, hubIn, hubOut, hubLoop, hubLoop}},
		{name: "incoming_x2_survivor_untouched", edges: []string{hubIn, xLoop, hubIn, xLoop}},
	}
}

// parallelDetachConfigs are the adjacency shapes the cases run over: the
// directed multigraph, the only storage shape (rmp #3072).
var parallelDetachConfigs = []struct {
	name string
	cfg  adjlist.Config
}{
	{"directed", adjlist.Config{}},
}

// adjacencyArcs reads the committed adjacency of g and returns the arc entries
// whose both endpoints are alive and the arc entries that touch a dead node —
// out-entries of every node and in-entries of dead nodes.
func adjacencyArcs(g *lpg.Graph[string, float64]) (live, dead int) {
	snap := g.BeginRead()
	defer g.EndRead(snap)
	view := g.ReadAt(snap)
	// Walk must not re-enter the Mapper: collect the ids, then read.
	var ids []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	for _, id := range ids {
		srcLive := view.Exists(id)
		for _, dst := range view.EntryView(id).Neighbours {
			if srcLive && view.Exists(dst) {
				live++
			} else {
				dead++
			}
		}
		if !srcLive {
			dead += len(g.InNeighbourIDsAsOf(id, snap))
		}
	}
	return live, dead
}

func runAll(t *testing.T, eng *cypher.Engine, queries ...string) {
	t.Helper()
	for _, q := range queries {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("RunAny(%q): %v", q, err)
		}
		for res.Next() { // intentional drain
		}
		if rerr := res.Err(); rerr != nil {
			_ = res.Close()
			t.Fatalf("result error for %q: %v", q, rerr)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("Close(%q): %v", q, cerr)
		}
	}
}

func relCount(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.RunAny(context.Background(), "MATCH ()-[r]->() RETURN count(r) AS c", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("count: no row (err %v)", res.Err())
	}
	c, ok := res.Record()["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("count: %T", res.Record()["c"])
	}
	return int64(c)
}

// parallelShape is what the oracle compares: Cypher's relationship count and
// the raw adjacency's live and dangling arc entries.
type parallelShape struct {
	rels, live, dead int64
}

func shapeOf(t *testing.T, eng *cypher.Engine, g *lpg.Graph[string, float64]) parallelShape {
	t.Helper()
	live, dead := adjacencyArcs(g)
	return parallelShape{rels: relCount(t, eng), live: int64(live), dead: int64(dead)}
}

// controlShape builds a fresh graph holding only the survivors of c and returns
// its shape: what the graph under test must equal after the DETACH DELETE.
func controlShape(t *testing.T, cfg adjlist.Config, c parallelDetachCase) parallelShape {
	t.Helper()
	g := lpg.New[string, float64](cfg)
	eng := cypher.NewEngine(g)
	runAll(t, eng, parallelSetup)
	runAll(t, eng, c.survivors()...)
	runAll(t, eng, "MATCH (h:Hub {id:1}) DELETE h")
	g.ReclaimNow()
	s := shapeOf(t, eng, g)
	if s.dead != 0 {
		t.Fatalf("control: %d dangling arc entries", s.dead)
	}
	return s
}

func checkShape(t *testing.T, stage string, got, want parallelShape) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: rels=%d live arcs=%d dangling=%d, want rels=%d live arcs=%d dangling=%d",
			stage, got.rels, got.live, got.dead, want.rels, want.live, want.dead)
	}
}

// TestDetachDelete_ParallelEdges_NoDangling_InMemory is the #2988 reproduction
// on the in-memory engine, checked before and after ReclaimNow.
func TestDetachDelete_ParallelEdges_NoDangling_InMemory(t *testing.T) {
	t.Parallel()
	for _, cf := range parallelDetachConfigs {
		for _, c := range parallelDetachCases() {
			t.Run(cf.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				want := controlShape(t, cf.cfg, c)
				g := lpg.New[string, float64](cf.cfg)
				eng := cypher.NewEngine(g)
				runAll(t, eng, parallelSetup)
				runAll(t, eng, c.edges...)
				runAll(t, eng, parallelDetach)
				checkShape(t, "before ReclaimNow", shapeOf(t, eng, g), want)
				g.ReclaimNow()
				g.ReclaimNow()
				checkShape(t, "after ReclaimNow", shapeOf(t, eng, g), want)
			})
		}
	}
}

// TestDetachDelete_ParallelEdges_Rollback_RestoresEveryInstance pins the abort
// path: a rolled-back DETACH DELETE restores every parallel instance, and a
// later committed DETACH DELETE still leaves nothing dangling.
func TestDetachDelete_ParallelEdges_Rollback_RestoresEveryInstance(t *testing.T) {
	t.Parallel()
	for _, cf := range parallelDetachConfigs {
		for _, c := range parallelDetachCases() {
			t.Run(cf.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				want := controlShape(t, cf.cfg, c)
				g := lpg.New[string, float64](cf.cfg)
				eng := cypher.NewEngine(g)
				runAll(t, eng, parallelSetup)
				runAll(t, eng, c.edges...)
				before := shapeOf(t, eng, g)

				tx, err := eng.BeginTx(context.Background())
				if err != nil {
					t.Fatalf("BeginTx: %v", err)
				}
				if err := execClose(tx, parallelDetach); err != nil {
					t.Fatalf("DETACH DELETE in tx: %v", err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatalf("Rollback: %v", err)
				}
				g.ReclaimNow()
				checkShape(t, "after rollback", shapeOf(t, eng, g), before)

				runAll(t, eng, parallelDetach)
				g.ReclaimNow()
				g.ReclaimNow()
				checkShape(t, "after rollback then commit", shapeOf(t, eng, g), want)
			})
		}
	}
}

// TestDetachDelete_ParallelEdges_NoDangling_WAL runs the reproduction through
// the WAL-backed engine, checks the live graph, then recovers and checks the
// replay.
func TestDetachDelete_ParallelEdges_NoDangling_WAL(t *testing.T) {
	t.Parallel()
	for _, cf := range parallelDetachConfigs {
		for _, c := range parallelDetachCases() {
			t.Run(cf.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				want := controlShape(t, cf.cfg, c)
				dir := t.TempDir()
				w, err := wal.Open(filepath.Join(dir, "wal"))
				if err != nil {
					t.Fatalf("wal.Open: %v", err)
				}
				g := lpg.New[string, float64](cf.cfg)
				queries := append(append([]string{parallelSetup}, c.edges...), parallelDetach)
				deleteWALEngineRun(t, g, w, queries...)
				g.ReclaimNow()
				g.ReclaimNow()
				checkShape(t, "live after ReclaimNow", shapeOf(t, cypher.NewEngine(g), g), want)

				rec, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
				if err != nil {
					t.Fatalf("recovery.Open: %v", err)
				}
				rec.Graph.ReclaimNow()
				checkShape(t, "recovered", shapeOf(t, cypher.NewEngine(rec.Graph), rec.Graph), want)
			})
		}
	}
}
