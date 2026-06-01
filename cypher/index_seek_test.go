package cypher_test

// index_seek_test.go — acceptance tests for task-370: inline node property
// predicates use hash index seeks when an index is available.
//
// AC1: EXPLAIN shows NodeByIndexSeek (not LabelScan) when index exists.
// AC2: Benchmark shows ≥2 orders of magnitude latency drop (seek vs scan).
// AC3: Dropping the index causes re-plan to show LabelScan.
// AC4: No regression — plans without indexes stay as LabelScan.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// newPersonGraph returns a graph pre-populated with n Person nodes whose names
// are "Person0", "Person1", … plus one node named "Alice". It also installs a
// string hash index named "person_name_hash" on the name property for all nodes
// when withIndex is true.
//
// Nodes are created via the Cypher engine (RunInTxAny) so they have the correct
// label and property structure. The index is populated by walking the graph's
// internal mapper and reading the "name" property directly from the LPG layer.
func newPersonGraph(n int, withIndex bool) (*lpg.Graph[string, float64], *cypher.Engine) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Person%d", i)
		q := fmt.Sprintf(`CREATE (n:Person {name: '%s'})`, name)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			panic(fmt.Sprintf("newPersonGraph CREATE %d: %v", i, err))
		}
		drainResultIdx(res)
	}
	// One node named Alice at the end.
	res, err := eng.RunInTxAny(ctx, `CREATE (n:Person {name: 'Alice'})`, nil)
	if err != nil {
		panic(fmt.Sprintf("newPersonGraph CREATE Alice: %v", err))
	}
	drainResultIdx(res)

	if withIndex {
		installPersonNameIndex(g)
	}
	return g, eng
}

// installPersonNameIndex creates and populates a hash[string] index named
// "person_name_hash" by walking every node in g that carries the label "Person"
// and reading its "name" property directly from the LPG layer.
//
// hash.Index[V].Apply is a no-op for the generic index, so we call Insert
// directly rather than relying on the change-fan-out mechanism.
func installPersonNameIndex(g *lpg.Graph[string, float64]) {
	idx := hash.New[string]()
	if err := g.IndexManager().CreateIndex("person_name_hash", idx); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			panic(fmt.Sprintf("installPersonNameIndex CreateIndex: %v", err))
		}
		return
	}

	// Walk every node; insert those with label "Person" and a "name" property.
	g.AdjList().Mapper().Walk(func(id graph.NodeID, nodeKey string) bool {
		if !g.HasNodeLabel(nodeKey, "Person") {
			return true
		}
		pv, ok := g.GetNodeProperty(nodeKey, "name")
		if !ok {
			return true
		}
		if s, ok2 := pv.String(); ok2 {
			idx.Insert(s, id)
		}
		return true
	})
}

func drainResultIdx(r *cypher.Result) {
	defer r.Close() //nolint:errcheck // test helper
	for r.Next() {
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC1: EXPLAIN shows NodeByIndexSeek when a hash index exists.
// ─────────────────────────────────────────────────────────────────────────────

func TestIndexSeek_ExplainShowsNodeByIndexSeek(t *testing.T) {
	const n = 100
	_, eng := newPersonGraph(n, true)

	plan, err := eng.Explain(`MATCH (n:Person {name: 'Alice'}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("expected NodeByIndexSeek in plan, got:\n%s", plan)
	}
	// The rewritten plan must not expose a bare Selection over LabelScan.
	if strings.Contains(plan, "Selection") {
		lines := strings.Split(plan, "\n")
		for _, l := range lines {
			if strings.Contains(l, "Selection") {
				t.Errorf("unexpected Selection in plan; full plan:\n%s", plan)
				break
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC2: Benchmark — index seek vs label scan on 100k nodes.
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkIndexSeek_vs_LabelScan(b *testing.B) {
	const N = 100_000

	// Build graph with index.
	_, engIdx := newPersonGraph(N, true)
	// Build graph without index.
	_, engScan := newPersonGraph(N, false)

	ctx := context.Background()
	q := `MATCH (n:Person {name: 'Alice'}) RETURN n`

	b.Run("LabelScan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, err := engScan.Run(ctx, q, nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
			}
			if err := res.Err(); err != nil {
				b.Fatal(err)
			}
			res.Close() //nolint:errcheck
		}
	})

	b.Run("IndexSeek", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, err := engIdx.Run(ctx, q, nil)
			if err != nil {
				b.Fatal(err)
			}
			for res.Next() {
			}
			if err := res.Err(); err != nil {
				b.Fatal(err)
			}
			res.Close() //nolint:errcheck
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// AC3: Dropping the index causes re-plan to LabelScan.
// ─────────────────────────────────────────────────────────────────────────────

func TestIndexSeek_DropIndexFallsBackToLabelScan(t *testing.T) {
	g, eng := newPersonGraph(10, true)

	// First: verify index seek is used.
	plan, err := eng.Explain(`MATCH (n:Person {name: 'Alice'}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain before drop: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("expected NodeByIndexSeek before drop; plan:\n%s", plan)
	}

	// Drop the index.
	if err := g.IndexManager().DropIndex("person_name_hash"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}

	// Second: same query should now use LabelScan (no index available).
	// Explain re-checks the index manager on every call.
	plan2, err := eng.Explain(`MATCH (n:Person {name: 'Alice'}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain after drop: %v", err)
	}
	if strings.Contains(plan2, "NodeByIndexSeek") {
		t.Errorf("expected no NodeByIndexSeek after drop; plan:\n%s", plan2)
	}
	// LabelScan (or Selection) must appear.
	if !strings.Contains(plan2, "LabelScan") && !strings.Contains(plan2, "Selection") {
		t.Errorf("expected LabelScan or Selection after drop; plan:\n%s", plan2)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC4: No index → plan stays as LabelScan (no regression).
// ─────────────────────────────────────────────────────────────────────────────

func TestIndexSeek_NoIndexKeepsLabelScan(t *testing.T) {
	_, eng := newPersonGraph(10, false)

	plan, err := eng.Explain(`MATCH (n:Person {name: 'Alice'}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("unexpected NodeByIndexSeek without index; plan:\n%s", plan)
	}
	if !strings.Contains(plan, "LabelScan") && !strings.Contains(plan, "Selection") {
		t.Errorf("expected LabelScan or Selection without index; plan:\n%s", plan)
	}
}
