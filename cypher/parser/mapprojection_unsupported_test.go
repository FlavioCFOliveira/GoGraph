package parser

// mapprojection_unsupported_test.go — parser-level acceptance gate for map
// projection (#1775). Map projection (n{.name, .*, k: expr}) is wired end to
// end: the grammar provides the mapProjection / mapProjectionItem productions,
// the visitor (VisitMapProjection) builds an *ast.MapProjection, and the
// expression evaluator / semantic analyser handle it. This test pins that the
// parser ACCEPTS every map-projection form and builds the expected AST shape.
//
// The behavioural, value-level coverage (projected values, .* expansion,
// literal entries, null handling) lives in the end-to-end Engine test
// cypher/mapprojection_test.go. See docs/tck/DIVERGENCES.md.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// findMapProjection walks a parsed query AST and returns the first
// *ast.MapProjection found in a RETURN projection item, or nil.
func findMapProjection(t *testing.T, query string) *ast.MapProjection {
	t.Helper()
	q, err := Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", query, err)
	}
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		t.Fatalf("Parse(%q): query is %T, want *ast.SingleQuery", query, q)
	}
	if sq.Return == nil || sq.Return.Projection == nil {
		t.Fatalf("Parse(%q): query has no RETURN projection", query)
	}
	for _, item := range sq.Return.Projection.Items {
		if mp, ok := item.Expr.(*ast.MapProjection); ok {
			return mp
		}
	}
	return nil
}

func TestMapProjection_Parses(t *testing.T) {
	t.Parallel()

	t.Run("property selectors", func(t *testing.T) {
		t.Parallel()
		mp := findMapProjection(t, "MATCH (n:Person) RETURN n{.name, .age} AS m")
		if mp == nil {
			t.Fatal("no *ast.MapProjection in RETURN")
		}
		if v, ok := mp.Subject.(*ast.Variable); !ok || v.Name != "n" {
			t.Fatalf("subject = %#v, want Variable{n}", mp.Subject)
		}
		if len(mp.Items) != 2 {
			t.Fatalf("items = %d, want 2", len(mp.Items))
		}
		for i, want := range []string{"name", "age"} {
			it := mp.Items[i]
			if it.IsAll || it.Value != nil || it.Key != want {
				t.Errorf("item[%d] = %#v, want property selector .%s", i, it, want)
			}
		}
	})

	t.Run("all-properties selector", func(t *testing.T) {
		t.Parallel()
		mp := findMapProjection(t, "MATCH (n:Person) RETURN n{.*} AS m")
		if mp == nil {
			t.Fatal("no *ast.MapProjection in RETURN")
		}
		if len(mp.Items) != 1 || !mp.Items[0].IsAll {
			t.Fatalf("items = %#v, want a single .* selector", mp.Items)
		}
	})

	t.Run("literal entry", func(t *testing.T) {
		t.Parallel()
		mp := findMapProjection(t, "MATCH (n) RETURN n{.name, extra: 1} AS m")
		if mp == nil {
			t.Fatal("no *ast.MapProjection in RETURN")
		}
		if len(mp.Items) != 2 {
			t.Fatalf("items = %d, want 2", len(mp.Items))
		}
		// First item: property selector .name.
		if mp.Items[0].Key != "name" || mp.Items[0].Value != nil || mp.Items[0].IsAll {
			t.Errorf("item[0] = %#v, want property selector .name", mp.Items[0])
		}
		// Second item: literal entry extra: 1.
		lit := mp.Items[1]
		if lit.Key != "extra" || lit.IsAll {
			t.Fatalf("item[1] = %#v, want literal entry extra: <expr>", lit)
		}
		if il, ok := lit.Value.(*ast.IntLiteral); !ok || il.Value != 1 {
			t.Errorf("item[1].Value = %#v, want IntLiteral{1}", lit.Value)
		}
	})

	t.Run("variable selector", func(t *testing.T) {
		t.Parallel()
		mp := findMapProjection(t, "MATCH (n) RETURN n{.name, n} AS m")
		if mp == nil {
			t.Fatal("no *ast.MapProjection in RETURN")
		}
		if len(mp.Items) != 2 {
			t.Fatalf("items = %d, want 2", len(mp.Items))
		}
		// Variable selector: Key empty, Value is a *ast.Variable.
		vs := mp.Items[1]
		if vs.Key != "" || vs.IsAll {
			t.Fatalf("item[1] = %#v, want variable selector", vs)
		}
		if v, ok := vs.Value.(*ast.Variable); !ok || v.Name != "n" {
			t.Errorf("item[1].Value = %#v, want Variable{n}", vs.Value)
		}
	})

	t.Run("map variable subject", func(t *testing.T) {
		t.Parallel()
		mp := findMapProjection(t, "WITH {a: 1, b: 2} AS m RETURN m{.a, .b} AS x")
		if mp == nil {
			t.Fatal("no *ast.MapProjection in RETURN")
		}
		if v, ok := mp.Subject.(*ast.Variable); !ok || v.Name != "m" {
			t.Fatalf("subject = %#v, want Variable{m}", mp.Subject)
		}
		if len(mp.Items) != 2 {
			t.Fatalf("items = %d, want 2", len(mp.Items))
		}
	})
}
