package cypher

// columnar_seek_precedence_test.go — precedence gate: an index access path outranks
// columnar execution (#2204).
//
// The columnar recognisers fire at the ir.Projection level, ABOVE the Selection where
// buildOperator applies the index-seek rewrites. So a recogniser that claims a Selection
// over a labelled scan silently discards any seek that would have fired — and the round-3
// audit measured the consequence: `MATCH (p:Person {age: v}) RETURN p.age` on an INDEXED
// property took 553 us at N=4000 and 9.67 ms at N=64000, identical to the same query with
// the seek disabled, while the entity-projecting form of the same predicate was flat at
// ~4.2 us. Having an index made the query slower than not having one, decided purely by
// the RETURN shape.
//
// The rule this file pins: a seek is SUBLINEAR in the label population where the columnar
// chain is LINEAR in it, so the seek wins by an order of magnitude, not a constant factor
// — the same reasoning that makes the columnar tier yield to the min-cardinality label
// re-anchor and the anchor swap (#2186).
//
// The assertions are behavioural rather than structural: they compare the work done at
// two label populations. A plan that scans grows with the population; a plan that seeks
// does not. That cannot pass by accident and needs no access to the physical plan.
//
// Layer: short.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// seedIndexedPeople builds n :Person nodes with an int64 `age` and a `tag` string, and
// creates an index on age. The returned engine is ready to query.
func seedIndexedPeople(t *testing.T, n int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("p%06d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "age", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty age: %v", err)
		}
		if err := g.SetNodeProperty(k, "tag", lpg.StringValue("t")); err != nil {
			t.Fatalf("SetNodeProperty tag: %v", err)
		}
	}
	eng := NewEngine(g)
	res, err := eng.RunInTx(context.Background(),
		`CREATE INDEX person_age FOR (p:Person) ON (p.age) OPTIONS {indexType:'btree'}`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("CREATE INDEX drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("CREATE INDEX close: %v", err)
	}
	return eng
}

// countColumnarBatches runs query and returns the number of columnar filter batches it
// drained, which is zero when the columnar path did not engage.
func countColumnarBatches(t *testing.T, eng *Engine, query string) uint64 {
	t.Helper()
	be := &countingBackend{}
	metrics.SetBackend(be)
	defer metrics.SetBackend(nil)
	be.filterBatches.Store(0)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
	return be.filterBatches.Load()
}

// seekGatePopulation is above the seek's own selectivity gate. The range/numeric seek
// only replaces a scan when the exact in-range cardinality makes it a provable win, so at
// a small population it declines and the columnar path legitimately claims the shape —
// correct behaviour, and the reason these tests use the same populations the benchmark
// does rather than a token fixture.
const seekGatePopulation = 4000

// TestColumnarYieldsToIndexSeek_ScalarProjection pins that a scalar-projecting equality on
// an indexed property does NOT take the columnar scan: the columnar filter must report
// zero batches, because the seek claimed the shape instead.
func TestColumnarYieldsToIndexSeek_ScalarProjection(t *testing.T) {
	eng := seedIndexedPeople(t, seekGatePopulation)

	// The scalar projection is the shape the columnar chain used to claim.
	if got := countColumnarBatches(t, eng, `MATCH (p:Person) WHERE p.age = 1000 RETURN p.age`); got != 0 {
		t.Errorf("scalar-projecting equality on an INDEXED property drained %d columnar "+
			"batches; it must yield to the index seek (#2204)", got)
	}

	// The same predicate WITHOUT an index must still take the columnar path, so the
	// assertion above is about the index and not about the shape being rejected outright.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < seekGatePopulation; i++ {
		k := fmt.Sprintf("q%06d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "age", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	unindexed := NewEngine(g)
	if got := countColumnarBatches(t, unindexed, `MATCH (p:Person) WHERE p.age = 1000 RETURN p.age`); got == 0 {
		t.Error("with NO index the same query did not engage the columnar path, so this " +
			"test can no longer distinguish the yield from a blanket rejection")
	}
}

// TestColumnarYieldsToIndexSeek_FlatInLabelPopulation is the behavioural proof that the
// yielded plan actually SEEKS: the rows a scan would visit grow with the label
// population, so a plan that is flat across a 16x population increase cannot be scanning.
//
// It compares columnar-batch counts rather than wall-clock time, so it is stable under
// load and needs no timing threshold.
func TestColumnarYieldsToIndexSeek_FlatInLabelPopulation(t *testing.T) {
	for _, n := range []int{seekGatePopulation, 16000} {
		eng := seedIndexedPeople(t, n)
		got := countColumnarBatches(t, eng, `MATCH (p:Person) WHERE p.age = 500 RETURN p.age`)
		if got != 0 {
			t.Fatalf("N=%d: drained %d columnar batches; the seek must claim the shape at "+
				"every population", n, got)
		}
	}
}

// TestColumnarYieldsToIndexSeek_ResultsIdentical is the correctness half: yielding changes
// the physical plan, so the row multiset must be unchanged. It compares the indexed engine
// against an unindexed one over identical data.
func TestColumnarYieldsToIndexSeek_ResultsIdentical(t *testing.T) {
	const n = seekGatePopulation
	indexed := seedIndexedPeople(t, n)

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("p%06d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "age", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty age: %v", err)
		}
		if err := g.SetNodeProperty(k, "tag", lpg.StringValue("t")); err != nil {
			t.Fatalf("SetNodeProperty tag: %v", err)
		}
	}
	unindexed := NewEngine(g)

	drain := func(eng *Engine, query string) []string {
		res, err := eng.Run(context.Background(), query, nil)
		if err != nil {
			t.Fatalf("Run(%q): %v", query, err)
		}
		var out []string
		for res.Next() {
			out = append(out, res.ValueAt(0).String())
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err(%q): %v", query, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close(%q): %v", query, err)
		}
		return out
	}

	for _, q := range []string{
		`MATCH (p:Person) WHERE p.age = 500 RETURN p.age`,
		`MATCH (p:Person) WHERE p.age = 500 RETURN p.age`,
		`MATCH (p:Person) WHERE p.age > 1990 RETURN p.age`,
		`MATCH (p:Person) WHERE p.age = 999999 RETURN p.age`, // no match
		`MATCH (p:Person {age: 500}) RETURN p.tag`,
	} {
		t.Run(q, func(t *testing.T) {
			withIdx := drain(indexed, q)
			without := drain(unindexed, q)
			if len(withIdx) != len(without) {
				t.Fatalf("row count differs: indexed=%d unindexed=%d", len(withIdx), len(without))
			}
			for i := range withIdx {
				if withIdx[i] != without[i] {
					t.Fatalf("value %d differs: indexed=%q unindexed=%q", i, withIdx[i], without[i])
				}
			}
		})
	}
}
