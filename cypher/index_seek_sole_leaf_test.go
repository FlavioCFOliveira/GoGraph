package cypher

// Regression tests for rmp #3056: a statement that writes the property it seeks
// on keeps its equality seek when that seek is the plan's sole leaf
// ([soleSeekLeaf]), and declines it in every other shape rmp #2814 guards.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// soleLeafProfile returns the profiled plan of the write statement q.
func soleLeafProfile(t *testing.T, eng *Engine, q string, params map[string]expr.Value) string {
	t.Helper()
	s, err := eng.Profile(context.Background(), q, params)
	if err != nil {
		t.Fatalf("Profile %q: %v", q, err)
	}
	return s
}

// TestSoleSeekLeaf_WritingTheSeekPropertyKeepsTheSeek is the #3056 regression:
// before the fix the statement's own SET on (:L, s) turned the seek on (:L, s)
// into a label scan plus a filter. Both the parameter and the literal form are
// checked, and the write must still land.
func TestSoleSeekLeaf_WritingTheSeekPropertyKeepsTheSeek(t *testing.T) {
	eng, _ := idxUncommittedEngine(t)
	cases := []struct {
		q      string
		params map[string]expr.Value
		newKey string
	}{
		{`MATCH (n:L {s: $old}) SET n.s = $new`,
			map[string]expr.Value{"old": expr.StringValue("v7"), "new": expr.StringValue("p7")}, "p7"},
		{`MATCH (n:L {s: 'v8'}) SET n.s = 'p8'`, nil, "p8"},
		{`MATCH (n:L {s: 'v9'}) REMOVE n.s`, nil, ""},
	}
	for _, tc := range cases {
		plan := soleLeafProfile(t, eng, tc.q, tc.params)
		if !strings.Contains(plan, "NodeByIndexSeek") || strings.Contains(plan, "NodeByLabelScan") {
			t.Errorf("%s: want the equality seek, got\n%s", tc.q, plan)
		}
		if !strings.Contains(plan, "rows=1") {
			t.Errorf("%s: the seek must emit the one matching node, got\n%s", tc.q, plan)
		}
		if tc.newKey != "" {
			if got := idxUncommittedAutocommit(t, eng,
				`MATCH (m:L) WHERE m.s + '' = '`+tc.newKey+`' RETURN count(m) AS c`); got != 1 {
				t.Errorf("%s: the write did not land: scan count of %q = %d, want 1", tc.q, tc.newKey, got)
			}
		}
	}
}

// TestSoleSeekLeaf_OtherShapesKeepTheScan pins the shapes the narrowing must not
// reach: a plan with two leaves, and a chain through an operator that is not
// [initOnceChild] (Expand).
func TestSoleSeekLeaf_OtherShapesKeepTheScan(t *testing.T) {
	eng, _ := idxUncommittedEngine(t)
	for _, q := range []string{
		`MATCH (a:L {s: 'v1'}), (b:L {s: 'v2'}) SET a.s = 'x1'`,
		`MATCH (a:L {s: 'v3'})-[:R]->(b) SET a.s = 'x3'`,
	} {
		plan := soleLeafProfile(t, eng, q, nil)
		if strings.Contains(plan, "NodeByIndexSeek") {
			t.Errorf("%s: a seek on the written property must decline outside the sole-leaf shape, got\n%s", q, plan)
		}
	}
}

// TestSoleSeekLeaf_EarlierStatementStillBlocks pins the cross-statement half: a
// SET left unflushed by an earlier statement of the same transaction still
// blocks the sole leaf's seek. Through the stale index the second statement
// would find no node carrying 'zzz' and write nothing.
func TestSoleSeekLeaf_EarlierStatementStillBlocks(t *testing.T) {
	ctx := context.Background()
	eng, _ := idxUncommittedEngine(t)
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	idxUncommittedExec(t, tx, `MATCH (m:L) WHERE m.s + '' = 'v7' SET m.s = 'zzz'`)
	idxUncommittedExec(t, tx, `MATCH (n:L {s: 'zzz'}) SET n.s = 'yyy'`)

	if got := idxUncommittedTxCount(t, tx, `MATCH (m:L) WHERE m.s + '' = 'yyy' RETURN count(m) AS c`); got != 1 {
		t.Errorf("the second statement must find the node the first one moved: count of 'yyy' = %d, want 1", got)
	}
	if got := idxUncommittedTxCount(t, tx, `MATCH (m:L) WHERE m.s + '' = 'zzz' RETURN count(m) AS c`); got != 0 {
		t.Errorf("'zzz' must be gone: count = %d, want 0", got)
	}
}

// initOnceCounter wraps an operator and counts its Init calls.
type initOnceCounter struct {
	exec.Operator
	inits int
}

func (c *initOnceCounter) Init(ctx context.Context) error {
	c.inits++
	return c.Operator.Init(ctx)
}

// TestSoleSeekLeaf_InitOnceChildOperators proves the safety condition of
// [soleSeekLeaf]: the physical operator of every [initOnceChild] case
// initialises its child exactly once while it processes several rows. The case
// set is read from the source of initOnceChild, so an operator added there
// without a proof here fails the test.
func TestSoleSeekLeaf_InitOnceChildOperators(t *testing.T) {
	builders := map[string]func(child exec.Operator, schema map[string]int, m exec.GraphMutator) (exec.Operator, error){
		"*ir.SetProperty": func(c exec.Operator, s map[string]int, m exec.GraphMutator) (exec.Operator, error) {
			return exec.NewSetProperty("n", "s", "'x'", s, c, m)
		},
		"*ir.RemoveProperty": func(c exec.Operator, s map[string]int, m exec.GraphMutator) (exec.Operator, error) {
			return exec.NewRemoveProperty("n", "s", s, c, m), nil
		},
		"*ir.SetLabels": func(c exec.Operator, s map[string]int, m exec.GraphMutator) (exec.Operator, error) {
			return exec.NewSetLabels("n", []string{"M"}, s, c, m), nil
		},
		"*ir.RemoveLabels": func(c exec.Operator, s map[string]int, m exec.GraphMutator) (exec.Operator, error) {
			return exec.NewRemoveLabels("n", []string{"L"}, s, c, m), nil
		},
	}

	// The case set of initOnceChild, read from its source.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "index_pending_delta.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var cases []string
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "initOnceChild" {
			return true
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if st, ok := e.(*ast.StarExpr); ok {
						if sel, ok := st.X.(*ast.SelectorExpr); ok {
							cases = append(cases, "*"+sel.X.(*ast.Ident).Name+"."+sel.Sel.Name)
						}
					}
				}
			}
			return true
		})
		return false
	})
	sort.Strings(cases)
	want := make([]string, 0, len(builders))
	for k := range builders {
		want = append(want, k)
	}
	sort.Strings(want)
	if strings.Join(cases, ",") != strings.Join(want, ",") {
		t.Fatalf("initOnceChild cases %v, proved here %v: every case needs a proof", cases, want)
	}

	const rows = 3
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{Directed: true})
			for _, id := range []string{"a", "b", "c"} {
				if err := g.AddNode(id); err != nil {
					t.Fatal(err)
				}
				if err := g.SetNodeLabel(id, "L"); err != nil {
					t.Fatal(err)
				}
				if err := g.SetNodeProperty(id, "s", lpg.StringValue("v")); err != nil {
					t.Fatal(err)
				}
			}
			labelSrc := &lpgLabelResolver{g: g.ReadAt(nil)}
			child := &initOnceCounter{Operator: exec.NewNodeByLabelScan("L", &execLabelAdapter{labelSrc: labelSrc})}
			op, err := builders[name](child, map[string]int{"n": 0}, &lpgMutatorAdapter{g: g})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if err := op.Init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			defer func() { _ = op.Close() }()
			n := 0
			var row exec.Row
			for {
				ok, err := op.Next(&row)
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if !ok {
					break
				}
				n++
			}
			if n != rows {
				t.Fatalf("emitted %d rows, want %d: the proof needs several rows", n, rows)
			}
			if child.inits != 1 {
				t.Errorf("child initialised %d times over %d rows, want exactly 1", child.inits, rows)
			}
		})
	}
}
