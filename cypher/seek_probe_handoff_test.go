package cypher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seekProbeRig is a small indexed graph: :Account nodes with an integer id (a
// btree range index) and a string name, each indexed.
func seekProbeRig(t *testing.T, n int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("a%d", i)
		if err := g.AddNode(id); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(id, "Account"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(id, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(id, "name", lpg.StringValue(id)); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(id, "balance", lpg.Int64Value(int64(10*i))); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(g)
	for _, q := range []string{
		"CREATE INDEX FOR (n:Account) ON (n.id)",
		"CREATE INDEX FOR (n:Account) ON (n.name)",
	} {
		if _, err := e.RunInTx(context.Background(), q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return e
}

// firstSelection returns the first Selection reachable from plan.
func firstSelection(plan ir.LogicalPlan) *ir.Selection {
	if s, ok := plan.(*ir.Selection); ok {
		return s
	}
	for _, c := range plan.Children() {
		if s := firstSelection(c); s != nil {
			return s
		}
	}
	return nil
}

// treeContains reports whether want is an operator of the tree rooted at op.
func treeContains(op, want exec.Operator) bool {
	if op == want {
		return true
	}
	if pc, ok := op.(exec.PlanChildren); ok {
		for _, c := range pc.PlanChildren() {
			if treeContains(c, want) {
				return true
			}
		}
	}
	if u, ok := op.(interface{ Unwrap() exec.Operator }); ok {
		return treeContains(u.Unwrap(), want)
	}
	return false
}

// TestSeekProbe_BuiltOncePerExecution pins rmp #3096: the seek the columnar
// recogniser's probe builds is the very operator the Selection build places in the
// tree — the seek is built once per execution, not twice — and the schema the
// adoption leaves is the one a fresh build writes.
func TestSeekProbe_BuiltOncePerExecution(t *testing.T) {
	e := seekProbeRig(t, 200)
	ctx := context.Background()
	cases := []struct {
		query string
		param expr.Value
		kind  seekProbeKind
	}{
		{"MATCH (n:Account {id: $p}) RETURN n.balance AS b", expr.IntegerValue(42), seekProbeRange},
		{"MATCH (n:Account {name: $p}) RETURN n.balance AS b", expr.StringValue("a42"), seekProbeHash},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			entry, _, err := e.parseAndAnalyse(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			sel := firstSelection(entry.plan)
			if sel == nil {
				t.Fatal("no Selection in the plan")
			}
			params := map[string]expr.Value{"p": tc.param}
			snap := e.g.BeginRead()
			defer e.g.EndRead(snap)
			rv := e.g.ReadAt(snap)
			var sc readBuildScaffold
			walker, labelSrc, _, _, bopts := (&sc).init(ctx, e, rv, newNowAwareRegistry(e.reg, time.Now()))
			bopts.indexSeekEnabled = e.indexSeekEnabled
			bopts.rangeSeekEnabled = e.rangeSeekEnabled
			bopts.prefixSeekEnabled = e.prefixSeekEnabled
			bopts.bitmapIntersectEnabled = e.bitmapIntersectEnabled
			bopts.idxSnap = indexSeekSnapshotOf(rv, e.trustIndexSnapshotForTest)

			// A non-empty schema records nothing: the layout would not match.
			if !indexSeekWouldFire(sel, walker, labelSrc, params, map[string]int{"x": 0}, e.g.IndexManager(), bopts) {
				t.Fatal("probe did not fire on a non-empty schema")
			}
			if bopts.seekProbe.sel != nil {
				t.Fatal("verdict recorded for a non-empty schema")
			}

			if !indexSeekWouldFire(sel, walker, labelSrc, params, map[string]int{}, e.g.IndexManager(), bopts) {
				t.Fatal("probe did not fire")
			}
			if bopts.seekProbe.kind != tc.kind {
				t.Fatalf("probe kind = %d, want %d", bopts.seekProbe.kind, tc.kind)
			}
			probed := bopts.seekProbe.op
			wantSchema := copySchema(bopts.seekProbe.after)

			schema := map[string]int{}
			op, err := buildOperator(sel, walker, labelSrc, e.reg, params, schema, e.g.IndexManager(), e.procReg, nil, bopts)
			if err != nil {
				t.Fatal(err)
			}
			if !treeContains(op, probed) {
				t.Fatal("the Selection build did not adopt the probe-built seek: the seek was built twice")
			}
			if bopts.seekProbe.sel != nil || bopts.seekProbe.op != nil {
				t.Fatal("verdict not cleared after adoption")
			}
			if len(schema) != len(wantSchema) {
				t.Fatalf("schema = %v, want %v", schema, wantSchema)
			}
			for k, v := range wantSchema {
				if schema[k] != v {
					t.Fatalf("schema = %v, want %v", schema, wantSchema)
				}
			}

			// A fresh build with no verdict yields the same layout.
			fresh := map[string]int{}
			if _, err := buildOperator(sel, walker, labelSrc, e.reg, params, fresh, e.g.IndexManager(), e.procReg, nil, bopts); err != nil {
				t.Fatal(err)
			}
			for k, v := range fresh {
				if schema[k] != v || len(fresh) != len(schema) {
					t.Fatalf("adopted schema %v != fresh schema %v", schema, fresh)
				}
			}
		})
	}
}

// TestSeekProbe_EndToEndResults runs both seek kinds through Engine.Run and checks
// every answer, so an adopted operator that is mis-wired fails here.
func TestSeekProbe_EndToEndResults(t *testing.T) {
	e := seekProbeRig(t, 200)
	ctx := context.Background()
	for i := 0; i < 200; i += 37 {
		for _, c := range []struct {
			q string
			p expr.Value
		}{
			{"MATCH (n:Account {id: $p}) RETURN n.balance AS b", expr.IntegerValue(int64(i))},
			{"MATCH (n:Account {name: $p}) RETURN n.balance AS b", expr.StringValue(fmt.Sprintf("a%d", i))},
		} {
			r, err := e.Run(ctx, c.q, map[string]expr.Value{"p": c.p})
			if err != nil {
				t.Fatal(err)
			}
			rows := 0
			for r.Next() {
				rows++
				if got := r.ValueAt(0); got != expr.IntegerValue(int64(10*i)) {
					t.Fatalf("%s p=%v: b=%v, want %d", c.q, c.p, got, 10*i)
				}
			}
			if err := r.Err(); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("%s p=%v: %d rows, want 1", c.q, c.p, rows)
			}
		}
	}
}
