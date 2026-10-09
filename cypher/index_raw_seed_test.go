package cypher_test

// index_raw_seed_test.go — rmp #2848.
//
// A secondary index is maintained by exactly one path: the engine's commit-time
// write-back (cypher/exec/index_writeback.go → index.Manager.ApplyBatch). The
// public lpg.Graph mutators write the graph directly and deliver no
// index.Change, so a node seeded through them after CREATE INDEX never reaches
// the index. Before #2848 the write succeeded, the index stayed empty, and an
// index seek returned zero rows while a label scan over the same nodes returned
// every one of them. Symmetrically, after a supported seed-then-index, a raw
// RemoveNode or DelNodeProperty left the index holding a node the scan no longer
// found.
//
// The resolution: while an index is registered, every raw mutator that can
// change indexed state is refused with lpg.ErrIndexedRawWrite and changes
// nothing, so the index and the graph cannot diverge through that path.
//
// Layer: short.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const rawSeedNodes = 100

const rawSeedSeekQ = "MATCH (n:USER) WHERE n.id = $id RETURN n"

func rawSeedGraph() (*cypher.Engine, *lpg.Graph[string, float64]) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g), g
}

func rawSeedCreateIndex(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), "CREATE INDEX FOR (n:USER) ON (n.id)", nil)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	_ = res.Close()
}

// rawSeedSupported seeds rawSeedNodes :USER nodes through the raw API on a
// graph with NO index yet — the supported ordering.
func rawSeedSupported(t *testing.T, g *lpg.Graph[string, float64]) {
	t.Helper()
	for i := range rawSeedNodes {
		k := fmt.Sprintf("u%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "USER"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "id", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", k, err)
		}
	}
}

func rawSeedCount(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) int {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	return len(collectRecords(t, res))
}

// rawSeedSeekAll sums the index-seek rows over every seeded id, after checking
// that the equality really is planned as an index seek.
func rawSeedSeekAll(t *testing.T, eng *cypher.Engine) int {
	t.Helper()
	plan, err := eng.Explain(rawSeedSeekQ, map[string]expr.Value{"id": expr.StringValue("u0")})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("premise: the equality must be served by an index seek; plan:\n%s", plan)
	}
	seek := 0
	for i := range rawSeedNodes {
		seek += rawSeedCount(t, eng, rawSeedSeekQ, map[string]expr.Value{"id": expr.StringValue(fmt.Sprintf("u%d", i))})
	}
	return seek
}

func wantIndexedRawWrite(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, lpg.ErrIndexedRawWrite) {
		t.Fatalf("%s on an indexed graph returned %v, want lpg.ErrIndexedRawWrite", op, err)
	}
}

// TestRawSeedAfterIndexIsRefusedLoudly reproduces the divergence and pins the
// resolution: a raw seed after CREATE INDEX is refused with the typed error,
// nothing it attempted is applied, and seek and scan agree.
func TestRawSeedAfterIndexIsRefusedLoudly(t *testing.T) {
	eng, g := rawSeedGraph()
	rawSeedCreateIndex(t, eng)

	for i := range rawSeedNodes {
		k := fmt.Sprintf("u%d", i)
		// Creating a bare node changes no indexed state and stays admitted.
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%s) of a fresh node on an indexed graph: %v", k, err)
		}
		wantIndexedRawWrite(t, "SetNodeLabel", g.SetNodeLabel(k, "USER"))
		wantIndexedRawWrite(t, "SetNodeProperty", g.SetNodeProperty(k, "id", lpg.StringValue(k)))
	}

	scan := rawSeedCount(t, eng, "MATCH (n:USER) RETURN n", nil)
	if scan != 0 {
		t.Fatalf("a refused raw seed left %d :USER nodes behind; a refusal must change nothing", scan)
	}
	if seek := rawSeedSeekAll(t, eng); seek != scan {
		t.Fatalf("index seek returned %d rows, label scan %d: the raw seed bypassed index maintenance silently", seek, scan)
	}
}

// TestRawSeedBeforeIndexBackfills pins the supported ordering: the raw API
// populates an UNINDEXED graph, and CREATE INDEX afterwards backfills from it.
func TestRawSeedBeforeIndexBackfills(t *testing.T) {
	eng, g := rawSeedGraph()
	rawSeedSupported(t, g)
	rawSeedCreateIndex(t, eng)
	if seek := rawSeedSeekAll(t, eng); seek != rawSeedNodes {
		t.Fatalf("index seek after a pre-index raw seed returned %d rows, want %d", seek, rawSeedNodes)
	}
}

// TestRawRetireAfterIndexIsRefusedLoudly covers the other direction: after a
// supported seed-then-index, the raw mutators that would retire indexed state —
// node removal, label removal, property deletion, revival — are refused with the
// typed error and change nothing, so the seek still agrees with the scan.
func TestRawRetireAfterIndexIsRefusedLoudly(t *testing.T) {
	eng, g := rawSeedGraph()
	rawSeedSupported(t, g)
	rawSeedCreateIndex(t, eng)

	wantIndexedRawWrite(t, "RemoveNode", g.RemoveNode("u0"))
	wantIndexedRawWrite(t, "RemoveNodeLabel", g.RemoveNodeLabel("u1", "USER"))
	wantIndexedRawWrite(t, "DelNodeProperty", g.DelNodeProperty("u2", "id"))
	wantIndexedRawWrite(t, "Revive", g.Revive("u3"))
	if id, ok := g.AdjList().Mapper().Lookup("u4"); ok {
		wantIndexedRawWrite(t, "RestoreTombstones", g.RestoreTombstones([]graph.NodeID{id}))
	} else {
		t.Fatal("premise: u4 is not interned")
	}

	scan := rawSeedCount(t, eng, "MATCH (n:USER) RETURN n", nil)
	if scan != rawSeedNodes {
		t.Fatalf("label scan returned %d :USER nodes after refused raw retirements, want %d: a refusal changed the graph",
			scan, rawSeedNodes)
	}
	if seek := rawSeedSeekAll(t, eng); seek != scan {
		t.Fatalf("index seek returned %d rows, label scan %d, after raw retirements on an indexed graph", seek, scan)
	}

	// The engine's own write path is unaffected: it maintains the index.
	res, err := eng.RunAny(context.Background(), "MATCH (n:USER {id: 'u0'}) DETACH DELETE n", nil)
	if err != nil {
		t.Fatalf("DETACH DELETE through the engine on an indexed graph: %v", err)
	}
	_ = res.Close()
	if seek := rawSeedSeekAll(t, eng); seek != rawSeedNodes-1 {
		t.Fatalf("index seek after an engine DETACH DELETE returned %d rows, want %d", seek, rawSeedNodes-1)
	}
}
