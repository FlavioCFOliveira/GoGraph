package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedIndexedGraph builds an engine with a (:Widget).sku index and a handful of
// nodes carrying sku values, returning the engine and adapter.
func seedIndexedGraph(t *testing.T) *EngineAdapter {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	mustRunIdx(t, eng, `CREATE INDEX widget_sku FOR (n:Widget) ON (n.sku)`)
	for _, sku := range []string{"A", "B", "B", "C"} {
		res, err := eng.RunInTxAny(ctx, `CREATE (n:Widget {sku:$sku})`, map[string]any{"sku": sku})
		if err != nil {
			t.Fatalf("CREATE Widget %q: %v", sku, err)
		}
		drainRes(t, res)
	}
	return NewEngineAdapter(eng)
}

func mustRunIdx(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RUN %q: %v", q, err)
	}
	drainRes(t, res)
}

func drainRes(t *testing.T, res *cypher.Result) {
	t.Helper()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestCheckIndexConsistency_CleanWhenInSync verifies the check finds zero
// violations on a correctly-maintained index, including for a value carried by
// more than one node.
func TestCheckIndexConsistency_CleanWhenInSync(t *testing.T) {
	t.Parallel()
	a := seedIndexedGraph(t)
	v := CheckIndexConsistency(1, nil, a, IndexSpec{Label: "Widget", Property: "sku"})
	if len(v) != 0 {
		t.Fatalf("expected a clean index, got violations: %v", v)
	}
}

// TestCheckIndexConsistency_CleanAfterDDLChurn verifies the check still passes
// after the index is dropped and re-created over existing data (the backfill
// path), which is the schema-chaos scenario's core stressor.
func TestCheckIndexConsistency_CleanAfterDDLChurn(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	for _, sku := range []string{"X", "Y", "Y", "Z"} {
		res, err := eng.RunInTxAny(ctx, `CREATE (n:Gadget {sku:$sku})`, map[string]any{"sku": sku})
		if err != nil {
			t.Fatalf("CREATE Gadget %q: %v", sku, err)
		}
		drainRes(t, res)
	}
	// DDL churn: create, drop, re-create over the existing data.
	mustRunIdx(t, eng, `CREATE INDEX gadget_sku FOR (n:Gadget) ON (n.sku)`)
	mustRunIdx(t, eng, `DROP INDEX gadget_sku IF EXISTS`)
	mustRunIdx(t, eng, `CREATE INDEX gadget_sku FOR (n:Gadget) ON (n.sku)`)

	a := NewEngineAdapter(eng)
	if v := CheckIndexConsistency(1, nil, a, IndexSpec{Label: "Gadget", Property: "sku"}); len(v) != 0 {
		t.Fatalf("expected clean index after DDL churn, got: %v", v)
	}
}

// TestCheckIndexConsistency_DetectsDivergence injects a divergence between the
// index-seek path and the full scan via a divergent fake engine and asserts the
// check flags an ACID_CONSISTENCY violation.
func TestCheckIndexConsistency_DetectsDivergence(t *testing.T) {
	t.Parallel()
	// The fake's full scan reports node 1 carries sku "A"; the seek for "A"
	// returns nodes {1,2} — node 2 is a torn/orphaned entry. It also under-reports
	// "B" (scan says {3}, seek says {}).
	fe := &divergentIndexEngine{
		scan: []scanRow{{id: 1, val: "A"}, {id: 3, val: "B"}},
		seek: map[string][]int64{"A": {1, 2}, "B": {}},
	}
	v := CheckIndexConsistency(1, nil, fe, IndexSpec{Label: "Widget", Property: "sku"})
	if len(v) < 2 {
		t.Fatalf("expected at least a torn and a stale violation, got: %v", v)
	}
	var torn, stale bool
	for _, viol := range v {
		if viol.Kind != ViolationACIDConsistency {
			t.Fatalf("unexpected violation kind %s", viol.Kind)
		}
		switch {
		case strings.Contains(viol.Message, "TORN/ORPHANED"):
			torn = true
		case strings.Contains(viol.Message, "STALE/LOST"):
			stale = true
		}
	}
	if !torn || !stale {
		t.Fatalf("expected both torn and stale violations, got torn=%v stale=%v: %v", torn, stale, v)
	}
}

// scanRow is one row of a divergentIndexEngine full scan.
type scanRow struct {
	id  int64
	val string
}

// divergentIndexEngine is a fake Engine that answers the scan query and the
// seek query from independent, deliberately-divergent fixtures, so the
// index-consistency check can be exercised without a real torn index.
type divergentIndexEngine struct {
	scan []scanRow
	seek map[string][]int64
}

func (e *divergentIndexEngine) Run(_ context.Context, query string, params map[string]any) (Result, error) {
	// The scan query has no params and returns (id, value); the seek query binds $v.
	if params == nil {
		return &scanResult{rows: e.scan}, nil
	}
	v, _ := params["v"].(string)
	return &seekResult{ids: e.seek[v]}, nil
}

func (e *divergentIndexEngine) NodeCount() (int64, error) { return int64(len(e.scan)), nil }
func (e *divergentIndexEngine) EdgeCount() (int64, error) { return 0, nil }

// scanResult yields (id, value) rows.
type scanResult struct {
	rows []scanRow
	cur  int
}

func (r *scanResult) Next() bool { r.cur++; return r.cur <= len(r.rows) }
func (r *scanResult) ScalarInt() (int64, bool) {
	return r.rows[r.cur-1].id, true
}
func (r *scanResult) IntAt(i int) (int64, bool) {
	if i == 0 {
		return r.rows[r.cur-1].id, true
	}
	return 0, false
}
func (r *scanResult) StringAt(i int) (string, bool) {
	if i == 1 {
		return r.rows[r.cur-1].val, true
	}
	return "", false
}
func (r *scanResult) RowCount() int { return r.cur }
func (r *scanResult) Err() error    { return nil }
func (r *scanResult) Close() error  { return nil }

// seekResult yields id rows for one seeked value.
type seekResult struct {
	ids []int64
	cur int
}

func (r *seekResult) Next() bool               { r.cur++; return r.cur <= len(r.ids) }
func (r *seekResult) ScalarInt() (int64, bool) { return r.ids[r.cur-1], true }
func (r *seekResult) IntAt(int) (int64, bool)  { return r.ids[r.cur-1], true }
func (r *seekResult) StringAt(int) (string, bool) {
	return "", false
}
func (r *seekResult) RowCount() int { return r.cur }
func (r *seekResult) Err() error    { return nil }
func (r *seekResult) Close() error  { return nil }
