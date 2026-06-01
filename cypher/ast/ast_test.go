package ast_test

import (
	"reflect"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// ptr returns a pointer to the given string value — helper for optional names.
func ptr(s string) *string { return &s }

// ptrI returns a pointer to the given int64 — helper for RangeQuantifier bounds.
func ptrI(v int64) *int64 { return &v }

// ─────────────────────────────────────────────────────────────────────────────
// Position
// ─────────────────────────────────────────────────────────────────────────────

func TestPosition_String(t *testing.T) {
	tests := []struct {
		pos  ast.Position
		want string
	}{
		{ast.Position{}, "0:0"},
		{ast.Position{Line: 3, Column: 7, Offset: 42}, "3:7"},
	}
	for _, tc := range tests {
		if got := tc.pos.String(); got != tc.want {
			t.Errorf("Position.String() = %q; want %q", got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Literals
// ─────────────────────────────────────────────────────────────────────────────

func TestIntLiteral(t *testing.T) {
	n := &ast.IntLiteral{Value: 42}
	if n.String() != "42" {
		t.Fatalf("got %q", n.String())
	}
	var _ ast.Expression = n
}

func TestFloatLiteral(t *testing.T) {
	n := &ast.FloatLiteral{Value: 3.14}
	if got := n.String(); got != "3.14" {
		t.Fatalf("got %q", got)
	}
	var _ ast.Expression = n
}

func TestStringLiteral(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hello", "'hello'"},
		{"it's", `'it\'s'`},
		{"", "''"},
	}
	for _, tc := range tests {
		n := &ast.StringLiteral{Value: tc.in}
		if got := n.String(); got != tc.want {
			t.Errorf("StringLiteral(%q).String() = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestBoolLiteral(t *testing.T) {
	if (&ast.BoolLiteral{Value: true}).String() != "true" {
		t.Fatal("expected true")
	}
	if (&ast.BoolLiteral{Value: false}).String() != "false" {
		t.Fatal("expected false")
	}
}

func TestNullLiteral(t *testing.T) {
	n := &ast.NullLiteral{}
	if n.String() != "null" {
		t.Fatalf("got %q", n.String())
	}
	var _ ast.Expression = n
}

func TestListLiteral(t *testing.T) {
	n := &ast.ListLiteral{
		Elements: []ast.Expression{
			&ast.IntLiteral{Value: 1},
			&ast.IntLiteral{Value: 2},
		},
	}
	if got := n.String(); got != "[1, 2]" {
		t.Fatalf("got %q", got)
	}
	empty := &ast.ListLiteral{}
	if got := empty.String(); got != "[]" {
		t.Fatalf("empty list got %q", got)
	}
}

func TestMapLiteral(t *testing.T) {
	n := &ast.MapLiteral{
		Keys:   []string{"name", "age"},
		Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}, &ast.IntLiteral{Value: 30}},
	}
	if got := n.String(); got != "{name: 'Alice', age: 30}" {
		t.Fatalf("got %q", got)
	}
	empty := &ast.MapLiteral{}
	if got := empty.String(); got != "{}" {
		t.Fatalf("empty map got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Expressions
// ─────────────────────────────────────────────────────────────────────────────

func TestVariable(t *testing.T) {
	v := &ast.Variable{Name: "n"}
	if v.String() != "n" {
		t.Fatalf("got %q", v.String())
	}
	var _ ast.Expression = v
}

func TestParameter(t *testing.T) {
	p := &ast.Parameter{Name: "userId"}
	if p.String() != "$userId" {
		t.Fatalf("got %q", p.String())
	}
}

func TestProperty(t *testing.T) {
	p := &ast.Property{
		Receiver: &ast.Variable{Name: "n"},
		Key:      "name",
	}
	if p.String() != "n.name" {
		t.Fatalf("got %q", p.String())
	}
}

func TestFunctionInvocation(t *testing.T) {
	tests := []struct {
		name string
		f    *ast.FunctionInvocation
		want string
	}{
		{
			"simple",
			&ast.FunctionInvocation{
				Name: "length",
				Args: []ast.Expression{&ast.Variable{Name: "p"}},
			},
			"length(p)",
		},
		{
			"namespaced",
			&ast.FunctionInvocation{
				Namespace: []string{"apoc", "path"},
				Name:      "expand",
				Args:      []ast.Expression{&ast.Variable{Name: "n"}},
			},
			"apoc.path.expand(n)",
		},
		{
			"distinct",
			&ast.FunctionInvocation{
				Name:     "count",
				Distinct: true,
				Args:     []ast.Expression{&ast.Variable{Name: "n"}},
			},
			"count(DISTINCT n)",
		},
		{
			"no args",
			&ast.FunctionInvocation{Name: "timestamp"},
			"timestamp()",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.String(); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestBinaryOp(t *testing.T) {
	b := &ast.BinaryOp{
		Left:     &ast.Variable{Name: "x"},
		Operator: "+",
		Right:    &ast.IntLiteral{Value: 1},
	}
	if got := b.String(); got != "(x + 1)" {
		t.Fatalf("got %q", got)
	}
}

func TestUnaryOp(t *testing.T) {
	tests := []struct {
		op   string
		want string
	}{
		{"-", "(- x)"},
		{"NOT", "(NOT x)"},
		{"IS NULL", "(x IS NULL)"},
		{"IS NOT NULL", "(x IS NOT NULL)"},
	}
	for _, tc := range tests {
		u := &ast.UnaryOp{Operator: tc.op, Operand: &ast.Variable{Name: "x"}}
		if got := u.String(); got != tc.want {
			t.Errorf("op=%q got %q; want %q", tc.op, got, tc.want)
		}
	}
}

func TestCaseExpression(t *testing.T) {
	// generic CASE WHEN x = 1 THEN 'a' ELSE 'b' END
	c := &ast.CaseExpression{
		Alternatives: []*ast.CaseAlternative{
			{
				Condition:  &ast.BinaryOp{Left: &ast.Variable{Name: "x"}, Operator: "=", Right: &ast.IntLiteral{Value: 1}},
				Consequent: &ast.StringLiteral{Value: "a"},
			},
		},
		ElseExpr: &ast.StringLiteral{Value: "b"},
	}
	want := "CASE WHEN (x = 1) THEN 'a' ELSE 'b' END"
	if got := c.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}

	// value CASE x WHEN 1 THEN 'a' END
	cv := &ast.CaseExpression{
		Subject: &ast.Variable{Name: "x"},
		Alternatives: []*ast.CaseAlternative{
			{
				Condition:  &ast.IntLiteral{Value: 1},
				Consequent: &ast.StringLiteral{Value: "a"},
			},
		},
	}
	wantV := "CASE x WHEN 1 THEN 'a' END"
	if got := cv.String(); got != wantV {
		t.Fatalf("value case got %q; want %q", got, wantV)
	}
}

func TestListComprehension(t *testing.T) {
	lc := &ast.ListComprehension{
		Variable:   "x",
		Source:     &ast.Variable{Name: "nums"},
		Predicate:  &ast.BinaryOp{Left: &ast.Variable{Name: "x"}, Operator: ">", Right: &ast.IntLiteral{Value: 0}},
		Projection: &ast.BinaryOp{Left: &ast.Variable{Name: "x"}, Operator: "*", Right: &ast.IntLiteral{Value: 2}},
	}
	want := "[x IN nums WHERE (x > 0) | (x * 2)]"
	if got := lc.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}

	// without predicate
	lcNoPred := &ast.ListComprehension{
		Variable:   "x",
		Source:     &ast.Variable{Name: "nums"},
		Projection: &ast.Variable{Name: "x"},
	}
	want2 := "[x IN nums | x]"
	if got := lcNoPred.String(); got != want2 {
		t.Fatalf("no-pred got %q; want %q", got, want2)
	}
}

func TestPatternComprehension(t *testing.T) {
	n := ptr("n")
	pc := &ast.PatternComprehension{
		Pattern: &ast.PathPattern{
			Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}},
		},
		Projection: &ast.Variable{Name: "n"},
	}
	want := "[(n) | n]"
	if got := pc.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestMapProjection(t *testing.T) {
	mp := &ast.MapProjection{
		Subject: &ast.Variable{Name: "n"},
		Items: []*ast.MapProjectionItem{
			{Key: "name", Value: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}},
			{Key: "extra", Value: nil}, // property selector
			{IsAll: true},              // .*
		},
	}
	want := "n {name: n.name, .extra, .*}"
	if got := mp.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestExistsSubquery(t *testing.T) {
	n := ptr("n")
	e := &ast.ExistsSubquery{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}},
			},
		},
	}
	if got := e.String(); got != "EXISTS { (n) }" {
		t.Fatalf("got %q", got)
	}

	// subquery form
	e2 := &ast.ExistsSubquery{
		Query: &ast.SingleQuery{
			ReadingClauses: []ast.ReadingClause{
				&ast.Match{Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}},
					},
				}},
			},
			Return: &ast.Return{Projection: &ast.Projection{All: true}},
		},
	}
	want2 := "EXISTS { MATCH (n) RETURN * }"
	if got := e2.String(); got != want2 {
		t.Fatalf("subquery form got %q; want %q", got, want2)
	}
}

func TestCountSubquery(t *testing.T) {
	n := ptr("n")
	c := &ast.CountSubquery{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}},
			},
		},
	}
	if got := c.String(); got != "COUNT { (n) }" {
		t.Fatalf("got %q", got)
	}
}

func TestSubscriptExpr(t *testing.T) {
	s := &ast.SubscriptExpr{
		Expr:  &ast.Variable{Name: "arr"},
		Index: &ast.IntLiteral{Value: 0},
	}
	if got := s.String(); got != "arr[0]" {
		t.Fatalf("got %q", got)
	}
}

func TestSliceExpr(t *testing.T) {
	tests := []struct {
		name string
		s    *ast.SliceExpr
		want string
	}{
		{
			"both",
			&ast.SliceExpr{Expr: &ast.Variable{Name: "a"}, From: &ast.IntLiteral{Value: 1}, To: &ast.IntLiteral{Value: 3}},
			"a[1..3]",
		},
		{
			"no from",
			&ast.SliceExpr{Expr: &ast.Variable{Name: "a"}, To: &ast.IntLiteral{Value: 3}},
			"a[..3]",
		},
		{
			"no to",
			&ast.SliceExpr{Expr: &ast.Variable{Name: "a"}, From: &ast.IntLiteral{Value: 1}},
			"a[1..]",
		},
		{
			"neither",
			&ast.SliceExpr{Expr: &ast.Variable{Name: "a"}},
			"a[..]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Patterns
// ─────────────────────────────────────────────────────────────────────────────

func TestRelDirection_String(t *testing.T) {
	tests := []struct {
		d    ast.RelDirection
		want string
	}{
		{ast.RelDirectionNone, "none"},
		{ast.RelDirectionOutgoing, "outgoing"},
		{ast.RelDirectionIncoming, "incoming"},
	}
	for _, tc := range tests {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("RelDirection %d String() = %q; want %q", tc.d, got, tc.want)
		}
	}
}

func TestNodePattern(t *testing.T) {
	tests := []struct {
		name string
		n    *ast.NodePattern
		want string
	}{
		{"anonymous", &ast.NodePattern{}, "()"},
		{"variable only", &ast.NodePattern{Variable: ptr("n")}, "(n)"},
		{"label only", &ast.NodePattern{Labels: []string{"Person"}}, "(:Person)"},
		{"var + labels", &ast.NodePattern{Variable: ptr("n"), Labels: []string{"Person", "Employee"}}, "(n:Person:Employee)"},
		{
			"with props",
			&ast.NodePattern{
				Variable:   ptr("n"),
				Labels:     []string{"Person"},
				Properties: &ast.MapLiteral{Keys: []string{"name"}, Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}}},
			},
			"(n:Person {name: 'Alice'})",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.n.String(); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestRangeQuantifier(t *testing.T) {
	tests := []struct {
		name string
		r    *ast.RangeQuantifier
		want string
	}{
		{"unbounded", &ast.RangeQuantifier{}, "*"},
		{"exact lower", &ast.RangeQuantifier{Min: ptrI(2)}, "*2.."},
		{"exact upper", &ast.RangeQuantifier{Max: ptrI(3)}, "*..3"},
		{"range", &ast.RangeQuantifier{Min: ptrI(1), Max: ptrI(3)}, "*1..3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.String(); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestRelationshipPattern(t *testing.T) {
	tests := []struct {
		name string
		r    *ast.RelationshipPattern
		want string
	}{
		{
			"anonymous undirected",
			&ast.RelationshipPattern{Direction: ast.RelDirectionNone},
			"-[]-",
		},
		{
			"outgoing typed",
			&ast.RelationshipPattern{Variable: ptr("r"), Types: []string{"KNOWS"}, Direction: ast.RelDirectionOutgoing},
			"-[r:KNOWS]->",
		},
		{
			"incoming multi-type",
			&ast.RelationshipPattern{Types: []string{"KNOWS", "LIKES"}, Direction: ast.RelDirectionIncoming},
			"<-[:KNOWS|LIKES]-",
		},
		{
			"variable length",
			&ast.RelationshipPattern{
				Variable:  ptr("r"),
				Types:     []string{"KNOWS"},
				Direction: ast.RelDirectionOutgoing,
				Range:     &ast.RangeQuantifier{Min: ptrI(1), Max: ptrI(3)},
			},
			"-[r:KNOWS*1..3]->",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.String(); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

func TestPathPattern(t *testing.T) {
	// (a)-[r:KNOWS]->(b)
	a := ptr("a")
	b := ptr("b")
	r := ptr("r")
	p := &ast.PathPattern{
		Head: &ast.PathElement{
			Node: &ast.NodePattern{Variable: a},
			Next: &ast.PathElement{
				Relationship: &ast.RelationshipPattern{Variable: r, Types: []string{"KNOWS"}, Direction: ast.RelDirectionOutgoing},
				Node:         &ast.NodePattern{Variable: b},
			},
		},
	}
	want := "(a)-[r:KNOWS]->(b)"
	if got := p.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}

	// named path: p = (a)-[r]->(b)
	pv := ptr("p")
	named := &ast.PathPattern{
		Variable: pv,
		Head: &ast.PathElement{
			Node: &ast.NodePattern{Variable: a},
			Next: &ast.PathElement{
				Relationship: &ast.RelationshipPattern{Variable: r, Direction: ast.RelDirectionOutgoing},
				Node:         &ast.NodePattern{Variable: b},
			},
		},
	}
	wantNamed := "p = (a)-[r]->(b)"
	if got := named.String(); got != wantNamed {
		t.Fatalf("named path got %q; want %q", got, wantNamed)
	}
}

func TestPattern(t *testing.T) {
	a := ptr("a")
	b := ptr("b")
	p := &ast.Pattern{
		Paths: []*ast.PathPattern{
			{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: a}}},
			{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: b}}},
		},
	}
	want := "(a), (b)"
	if got := p.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Clauses
// ─────────────────────────────────────────────────────────────────────────────

func TestWhere(t *testing.T) {
	w := &ast.Where{Predicate: &ast.BinaryOp{
		Left: &ast.Variable{Name: "n"}, Operator: "=", Right: &ast.IntLiteral{Value: 1},
	}}
	want := "WHERE (n = 1)"
	if got := w.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
	// implements ReadingClause
	var _ ast.ReadingClause = w
}

func TestMatch(t *testing.T) {
	n := ptr("n")
	m := &ast.Match{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n, Labels: []string{"Person"}}}}},
		},
	}
	if got := m.String(); got != "MATCH (n:Person)" {
		t.Fatalf("got %q", got)
	}

	mw := &ast.Match{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}}},
		},
		Where: &ast.Where{Predicate: &ast.Variable{Name: "x"}},
	}
	if got := mw.String(); got != "MATCH (n) WHERE x" {
		t.Fatalf("match+where got %q", got)
	}
	var _ ast.ReadingClause = m
}

func TestOptionalMatch(t *testing.T) {
	n := ptr("n")
	o := &ast.OptionalMatch{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}}},
		},
	}
	if got := o.String(); got != "OPTIONAL MATCH (n)" {
		t.Fatalf("got %q", got)
	}
	var _ ast.ReadingClause = o
}

func TestCreate(t *testing.T) {
	n := ptr("n")
	c := &ast.Create{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n, Labels: []string{"Person"}}}}},
		},
	}
	if got := c.String(); got != "CREATE (n:Person)" {
		t.Fatalf("got %q", got)
	}
	var _ ast.UpdatingClause = c
}

func TestMerge(t *testing.T) {
	n := ptr("n")
	m := &ast.Merge{
		Pattern: &ast.PathPattern{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}},
	}
	if got := m.String(); got != "MERGE (n)" {
		t.Fatalf("no actions got %q", got)
	}

	// with ON CREATE / ON MATCH
	prop := &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "created"}
	mFull := &ast.Merge{
		Pattern:  &ast.PathPattern{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}},
		OnCreate: []*ast.SetItem{{Target: prop, Value: &ast.BoolLiteral{Value: true}, Operator: "="}},
		OnMatch:  []*ast.SetItem{{Target: prop, Value: &ast.BoolLiteral{Value: false}, Operator: "="}},
	}
	want := "MERGE (n) ON CREATE SET n.created = true ON MATCH SET n.created = false"
	if got := mFull.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
	var _ ast.UpdatingClause = m
}

func TestSet(t *testing.T) {
	s := &ast.Set{Items: []*ast.SetItem{
		{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Value: &ast.StringLiteral{Value: "Bob"}, Operator: "="},
	}}
	want := "SET n.name = 'Bob'"
	if got := s.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestSetItem_Labels(t *testing.T) {
	si := &ast.SetItem{
		Target: &ast.Variable{Name: "n"},
		Labels: []string{"Admin", "User"},
	}
	want := "n:Admin:User"
	if got := si.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestSetItem_PlusEquals(t *testing.T) {
	si := &ast.SetItem{
		Target:   &ast.Variable{Name: "n"},
		Value:    &ast.MapLiteral{Keys: []string{"x"}, Values: []ast.Expression{&ast.IntLiteral{Value: 1}}},
		Operator: "+=",
	}
	want := "n += {x: 1}"
	if got := si.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestRemove(t *testing.T) {
	r := &ast.Remove{Items: []*ast.RemoveItem{
		{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"}},
	}}
	want := "REMOVE n.age"
	if got := r.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestRemoveItem_Labels(t *testing.T) {
	ri := &ast.RemoveItem{
		Target: &ast.Variable{Name: "n"},
		Labels: []string{"Admin"},
	}
	want := "n:Admin"
	if got := ri.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestDelete(t *testing.T) {
	d := &ast.Delete{Expressions: []ast.Expression{&ast.Variable{Name: "n"}}}
	if got := d.String(); got != "DELETE n" {
		t.Fatalf("got %q", got)
	}
	var _ ast.UpdatingClause = d
}

func TestDetachDelete(t *testing.T) {
	d := &ast.DetachDelete{Expressions: []ast.Expression{&ast.Variable{Name: "n"}, &ast.Variable{Name: "r"}}}
	if got := d.String(); got != "DETACH DELETE n, r" {
		t.Fatalf("got %q", got)
	}
	var _ ast.UpdatingClause = d
}

func TestCall(t *testing.T) {
	tests := []struct {
		name string
		c    *ast.Call
		want string
	}{
		{
			"simple call no yield",
			&ast.Call{Procedure: "db.ping"},
			"CALL db.ping",
		},
		{
			"call with args",
			&ast.Call{
				Procedure: "createUser",
				Args:      []ast.Expression{&ast.StringLiteral{Value: "Alice"}},
			},
			"CALL createUser('Alice')",
		},
		{
			"call yield star",
			&ast.Call{
				Namespace: []string{"apoc", "load"},
				Procedure: "json",
				Args:      []ast.Expression{&ast.StringLiteral{Value: "url"}},
				Yield:     []*ast.YieldItem{}, // empty = YIELD *
			},
			"CALL apoc.load.json('url') YIELD *",
		},
		{
			"call yield items",
			&ast.Call{
				Procedure: "getUser",
				Yield:     []*ast.YieldItem{{Name: "id"}, {Name: "name", Alias: ptr("username")}},
			},
			"CALL getUser YIELD id, name AS username",
		},
		{
			"call yield where",
			&ast.Call{
				Procedure: "search",
				Args:      []ast.Expression{},
				Yield:     []*ast.YieldItem{{Name: "node"}},
				Where:     &ast.Where{Predicate: &ast.Variable{Name: "node"}},
			},
			"CALL search() YIELD node WHERE node",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.String(); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Query-level nodes
// ─────────────────────────────────────────────────────────────────────────────

func TestReturn(t *testing.T) {
	r := &ast.Return{Projection: &ast.Projection{
		Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
	}}
	if got := r.String(); got != "RETURN n" {
		t.Fatalf("got %q", got)
	}
}

func TestReturn_All(t *testing.T) {
	r := &ast.Return{Projection: &ast.Projection{All: true}}
	if got := r.String(); got != "RETURN *" {
		t.Fatalf("got %q", got)
	}
}

func TestWith(t *testing.T) {
	w := &ast.With{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		},
	}
	if got := w.String(); got != "WITH n" {
		t.Fatalf("got %q", got)
	}

	ww := &ast.With{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		},
		Where: &ast.Where{Predicate: &ast.Variable{Name: "x"}},
	}
	if got := ww.String(); got != "WITH n WHERE x" {
		t.Fatalf("with+where got %q", got)
	}
}

func TestUnwind(t *testing.T) {
	u := &ast.Unwind{Expr: &ast.Variable{Name: "list"}, Variable: "x"}
	if got := u.String(); got != "UNWIND list AS x" {
		t.Fatalf("got %q", got)
	}
	var _ ast.ReadingClause = u
}

func TestUnion(t *testing.T) {
	n := ptr("n")
	sq := &ast.SingleQuery{
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		}},
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{
				Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}}},
			}},
		},
	}
	u := &ast.Union{Query: sq}
	if got := u.String(); got != "UNION MATCH (n) RETURN n" {
		t.Fatalf("got %q", got)
	}
	ua := &ast.Union{All: true, Query: sq}
	if got := ua.String(); got != "UNION ALL MATCH (n) RETURN n" {
		t.Fatalf("union all got %q", got)
	}
}

func TestSingleQuery(t *testing.T) {
	n := ptr("n")
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{
				Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n, Labels: []string{"Person"}}}}},
			}},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		}},
	}
	want := "MATCH (n:Person) RETURN n"
	if got := q.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestMultiQuery(t *testing.T) {
	n := ptr("n")
	sq := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{
				Paths: []*ast.PathPattern{{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: n}}}},
			}},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		}},
	}
	mq := &ast.MultiQuery{Parts: []*ast.SingleQuery{sq, sq}, All: false}
	want := "MATCH (n) RETURN n UNION MATCH (n) RETURN n"
	if got := mq.String(); got != want {
		t.Fatalf("union got %q; want %q", got, want)
	}
	mqAll := &ast.MultiQuery{Parts: []*ast.SingleQuery{sq, sq}, All: true}
	wantAll := "MATCH (n) RETURN n UNION ALL MATCH (n) RETURN n"
	if got := mqAll.String(); got != wantAll {
		t.Fatalf("union all got %q; want %q", got, wantAll)
	}
	// empty guard
	if got := (&ast.MultiQuery{}).String(); got != "" {
		t.Fatalf("empty MultiQuery should return empty string, got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Projection + SortItem
// ─────────────────────────────────────────────────────────────────────────────

func TestProjectionItem_Alias(t *testing.T) {
	pi := &ast.ProjectionItem{Expr: &ast.Variable{Name: "n"}, Alias: ptr("node")}
	if got := pi.String(); got != "n AS node" {
		t.Fatalf("got %q", got)
	}
}

func TestSortItem(t *testing.T) {
	tests := []struct {
		si   *ast.SortItem
		want string
	}{
		{&ast.SortItem{Expr: &ast.Variable{Name: "x"}, Descending: false}, "x ASC"},
		{&ast.SortItem{Expr: &ast.Variable{Name: "x"}, Descending: true}, "x DESC"},
	}
	for _, tc := range tests {
		if got := tc.si.String(); got != tc.want {
			t.Errorf("got %q; want %q", got, tc.want)
		}
	}
}

func TestProjection_Full(t *testing.T) {
	p := &ast.Projection{
		Distinct: true,
		Items:    []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		OrderBy:  []*ast.SortItem{{Expr: &ast.Variable{Name: "n"}, Descending: false}},
		Skip:     &ast.IntLiteral{Value: 10},
		Limit:    &ast.IntLiteral{Value: 5},
	}
	want := "DISTINCT n ORDER BY n ASC SKIP 10 LIMIT 5"
	if got := p.String(); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// structural equality via reflect.DeepEqual
// ─────────────────────────────────────────────────────────────────────────────

func TestDeepEqual_SimpleQuery(t *testing.T) {
	build := func() *ast.SingleQuery {
		n := ptr("n")
		return &ast.SingleQuery{
			ReadingClauses: []ast.ReadingClause{
				&ast.Match{Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: n, Labels: []string{"Person"}},
					}}},
				}},
			},
			Return: &ast.Return{Projection: &ast.Projection{
				Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
			}},
		}
	}
	a, b := build(), build()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("identical hand-built trees should be DeepEqual")
	}
}

func TestDeepEqual_DifferentTrees(t *testing.T) {
	a := &ast.SingleQuery{
		Return: &ast.Return{Projection: &ast.Projection{All: true}},
	}
	b := &ast.SingleQuery{
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		}},
	}
	if reflect.DeepEqual(a, b) {
		t.Fatal("different trees should not be DeepEqual")
	}
}

func TestDeepEqual_PathPattern(t *testing.T) {
	build := func() *ast.PathPattern {
		a, b := ptr("a"), ptr("b")
		r := ptr("r")
		return &ast.PathPattern{
			Head: &ast.PathElement{
				Node: &ast.NodePattern{Variable: a},
				Next: &ast.PathElement{
					Relationship: &ast.RelationshipPattern{
						Variable:  r,
						Types:     []string{"KNOWS"},
						Direction: ast.RelDirectionOutgoing,
					},
					Node: &ast.NodePattern{Variable: b},
				},
			},
		}
	}
	x, y := build(), build()
	if !reflect.DeepEqual(x, y) {
		t.Fatal("identical PathPattern trees should be DeepEqual")
	}
}

func TestDeepEqual_Literals(t *testing.T) {
	a := &ast.ListLiteral{Elements: []ast.Expression{
		&ast.IntLiteral{Value: 1},
		&ast.StringLiteral{Value: "x"},
		&ast.BoolLiteral{Value: true},
		&ast.NullLiteral{},
	}}
	b := &ast.ListLiteral{Elements: []ast.Expression{
		&ast.IntLiteral{Value: 1},
		&ast.StringLiteral{Value: "x"},
		&ast.BoolLiteral{Value: true},
		&ast.NullLiteral{},
	}}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("identical ListLiteral trees should be DeepEqual")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Interface compliance — compile-time checks via blank assignments
// ─────────────────────────────────────────────────────────────────────────────

func TestInterfaceCompliance(t *testing.T) {
	// ReadingClause implementors
	var _ ast.ReadingClause = &ast.Match{}
	var _ ast.ReadingClause = &ast.OptionalMatch{}
	var _ ast.ReadingClause = &ast.Unwind{}
	var _ ast.ReadingClause = &ast.With{}
	var _ ast.ReadingClause = &ast.Return{}
	var _ ast.ReadingClause = &ast.Union{}
	var _ ast.ReadingClause = &ast.Where{}
	var _ ast.ReadingClause = &ast.Call{}

	// UpdatingClause implementors
	var _ ast.UpdatingClause = &ast.Create{}
	var _ ast.UpdatingClause = &ast.Merge{}
	var _ ast.UpdatingClause = &ast.Set{}
	var _ ast.UpdatingClause = &ast.Remove{}
	var _ ast.UpdatingClause = &ast.Delete{}
	var _ ast.UpdatingClause = &ast.DetachDelete{}
	var _ ast.UpdatingClause = &ast.Call{}

	// Expression implementors
	var _ ast.Expression = &ast.Variable{}
	var _ ast.Expression = &ast.Parameter{}
	var _ ast.Expression = &ast.Property{}
	var _ ast.Expression = &ast.FunctionInvocation{}
	var _ ast.Expression = &ast.BinaryOp{}
	var _ ast.Expression = &ast.UnaryOp{}
	var _ ast.Expression = &ast.CaseExpression{}
	var _ ast.Expression = &ast.ListComprehension{}
	var _ ast.Expression = &ast.PatternComprehension{}
	var _ ast.Expression = &ast.MapProjection{}
	var _ ast.Expression = &ast.ExistsSubquery{}
	var _ ast.Expression = &ast.CountSubquery{}
	var _ ast.Expression = &ast.SubscriptExpr{}
	var _ ast.Expression = &ast.SliceExpr{}
	var _ ast.Expression = &ast.IntLiteral{}
	var _ ast.Expression = &ast.FloatLiteral{}
	var _ ast.Expression = &ast.StringLiteral{}
	var _ ast.Expression = &ast.BoolLiteral{}
	var _ ast.Expression = &ast.NullLiteral{}
	var _ ast.Expression = &ast.ListLiteral{}
	var _ ast.Expression = &ast.MapLiteral{}

	// Query implementors
	var _ ast.Query = &ast.SingleQuery{}
	var _ ast.Query = &ast.MultiQuery{}

	t.Log("all interface compliance checks passed")
}
