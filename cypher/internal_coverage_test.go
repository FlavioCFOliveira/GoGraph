package cypher

import (
	"context"
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
	"gograph/cypher/ir"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/index"
	"gograph/graph/index/hash"
	"gograph/graph/lpg"
)

// indexManagerFor returns the index.Manager for g, initialising one if the
// graph was created without one (lpg.New does not install a manager by default).
func indexManagerFor(g *lpg.Graph[string, float64]) *index.Manager {
	if mgr := g.IndexManager(); mgr != nil {
		return mgr
	}
	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	return mgr
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildPlan — direct access to unexported walker/labelSrc types
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildPlan_AllNodesScan(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("A")
	g.AddNode("B")

	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}

	scan := ir.NewAllNodesScan("n")
	plan := ir.NewProduceResults([]string{"n"}, scan)

	op, cols, err := BuildPlan(plan, walker, labelSrc, funcs.DefaultRegistry, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(cols) != 1 || cols[0] != "n" {
		t.Errorf("unexpected cols: %v", cols)
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}
	defer op.Close()

	count := 0
	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("op.Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}

func TestBuildPlan_NonProduceResultsRoot(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}

	scan := ir.NewAllNodesScan("n")
	_, _, err := BuildPlan(scan, walker, labelSrc, funcs.DefaultRegistry, nil)
	if err == nil {
		t.Fatal("expected error for non-ProduceResults root")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildPlanWithMutator
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildPlanWithMutator_AllNodesScan(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("X")

	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}
	mutator := &lpgMutatorAdapter{g: g}

	scan := ir.NewAllNodesScan("n")
	plan := ir.NewProduceResults([]string{"n"}, scan)

	op, cols, err := BuildPlanWithMutator(plan, walker, labelSrc, funcs.DefaultRegistry, nil, mutator)
	if err != nil {
		t.Fatalf("BuildPlanWithMutator: %v", err)
	}
	if len(cols) != 1 || cols[0] != "n" {
		t.Errorf("unexpected cols: %v", cols)
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}
	defer op.Close()

	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("op.Next: %v", err)
		}
		if !ok {
			break
		}
	}
}

func TestBuildPlanWithMutator_WriteOnlyRoot(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}
	mutator := &lpgMutatorAdapter{g: g}

	// A write-only plan (no ProduceResults wrapper) — exercises the
	// non-ProduceResults branch in BuildPlanWithMutator.
	scan := ir.NewAllNodesScan("n")
	op, _, err := BuildPlanWithMutator(scan, walker, labelSrc, funcs.DefaultRegistry, nil, mutator)
	if err != nil {
		t.Fatalf("BuildPlanWithMutator (write-only): %v", err)
	}
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}
	defer op.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// lpgMutatorAdapter: edge operations
// ─────────────────────────────────────────────────────────────────────────────

func TestLpgMutatorAdapter_HasAndRemoveEdge(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	g.AddNode("S")
	g.AddNode("D")
	g.AddEdge("S", "D", 1.0)

	a := &lpgMutatorAdapter{g: g}

	if !a.HasEdge("S", "D") {
		t.Error("expected edge S→D to exist")
	}
	if a.HasEdge("D", "S") {
		t.Error("expected no reverse edge D→S")
	}

	a.RemoveEdge("S", "D")
	if a.HasEdge("S", "D") {
		t.Error("expected edge S→D to be removed")
	}
}

func TestLpgMutatorAdapter_DelEdgeProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	g.AddNode("S")
	g.AddNode("D")
	g.AddEdge("S", "D", 1.0)
	g.SetEdgeProperty("S", "D", "weight", lpg.Int64Value(42))

	a := &lpgMutatorAdapter{g: g}
	a.DelEdgeProperty("S", "D", "weight")
}

func TestLpgMutatorAdapter_WalkNodeIDs(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("P")
	g.AddNode("Q")

	a := &lpgMutatorAdapter{g: g}
	count := 0
	a.WalkNodeIDs(func(_ graph.NodeID) bool {
		count++
		return true
	})
	if count != 2 {
		t.Errorf("expected 2 nodes, got %d", count)
	}
}

func TestLpgMutatorAdapter_ResolveNodeID(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("N")

	a := &lpgMutatorAdapter{g: g}

	id, ok := a.ResolveNodeID("N")
	if !ok {
		t.Fatal("expected to resolve N")
	}
	if id == 0 {
		t.Error("expected non-zero NodeID")
	}

	_, ok2 := a.ResolveNodeID("nonexistent")
	if ok2 {
		t.Error("expected false for nonexistent node")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// astLiteralToValue — all literal branches + parameter (bound/unbound)
// ─────────────────────────────────────────────────────────────────────────────

func TestAstLiteralToValue_AllBranches(t *testing.T) {
	t.Parallel()
	params := map[string]expr.Value{
		"x": expr.StringValue("hello"),
	}

	cases := []struct {
		name    string
		node    ast.Expression
		wantErr bool
		wantVal expr.Value
	}{
		{
			name:    "string literal",
			node:    &ast.StringLiteral{Value: "world"},
			wantVal: expr.StringValue("world"),
		},
		{
			name:    "int literal",
			node:    &ast.IntLiteral{Value: 42},
			wantVal: expr.IntegerValue(42),
		},
		{
			name:    "float literal",
			node:    &ast.FloatLiteral{Value: 3.14},
			wantVal: expr.FloatValue(3.14),
		},
		{
			name:    "bool literal true",
			node:    &ast.BoolLiteral{Value: true},
			wantVal: expr.BoolValue(true),
		},
		{
			name:    "bool literal false",
			node:    &ast.BoolLiteral{Value: false},
			wantVal: expr.BoolValue(false),
		},
		{
			name:    "param bound",
			node:    &ast.Parameter{Name: "x"},
			wantVal: expr.StringValue("hello"),
		},
		{
			name:    "param unbound",
			node:    &ast.Parameter{Name: "missing"},
			wantVal: expr.Null,
		},
		{
			name:    "unsupported node type",
			node:    &ast.Variable{Name: "n"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := astLiteralToValue(tc.node, params)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVal {
				t.Errorf("got %v, want %v", got, tc.wantVal)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// extractEqFromAST — all paths: left=Property, right=Property (mirror),
// non-BinaryOp, wrong operator, non-property sides
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractEqFromAST_AllBranches(t *testing.T) {
	t.Parallel()

	varN := &ast.Variable{Name: "n"}
	prop := &ast.Property{Receiver: varN, Key: "name"}

	t.Run("left=Property right=literal", func(t *testing.T) {
		binOp := &ast.BinaryOp{
			Left:     prop,
			Operator: "=",
			Right:    &ast.StringLiteral{Value: "Alice"},
		}
		key, val, ok := extractEqFromAST(binOp, "n", nil)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if key != "name" {
			t.Errorf("key = %q, want %q", key, "name")
		}
		if val != expr.StringValue("Alice") {
			t.Errorf("val = %v, want StringValue(Alice)", val)
		}
	})

	t.Run("mirror form: right=Property left=literal", func(t *testing.T) {
		binOp := &ast.BinaryOp{
			Left:     &ast.StringLiteral{Value: "Bob"},
			Operator: "=",
			Right:    prop,
		}
		key, val, ok := extractEqFromAST(binOp, "n", nil)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if key != "name" || val != expr.StringValue("Bob") {
			t.Errorf("got key=%q val=%v", key, val)
		}
	})

	t.Run("non-BinaryOp node", func(t *testing.T) {
		_, _, ok := extractEqFromAST(&ast.StringLiteral{Value: "x"}, "n", nil)
		if ok {
			t.Fatal("expected ok=false for non-BinaryOp")
		}
	})

	t.Run("BinaryOp with wrong operator", func(t *testing.T) {
		binOp := &ast.BinaryOp{
			Left:     prop,
			Operator: "<>",
			Right:    &ast.StringLiteral{Value: "x"},
		}
		_, _, ok := extractEqFromAST(binOp, "n", nil)
		if ok {
			t.Fatal("expected ok=false for non-= operator")
		}
	})

	t.Run("left=Property wrong nodeVar", func(t *testing.T) {
		binOp := &ast.BinaryOp{
			Left:     prop,
			Operator: "=",
			Right:    &ast.StringLiteral{Value: "x"},
		}
		_, _, ok := extractEqFromAST(binOp, "m", nil) // nodeVar "m", not "n"
		if ok {
			t.Fatal("expected ok=false for wrong nodeVar")
		}
	})

	t.Run("left=Property right=unsupported expression", func(t *testing.T) {
		binOp := &ast.BinaryOp{
			Left:     prop,
			Operator: "=",
			Right:    &ast.Variable{Name: "other"}, // not a literal
		}
		_, _, ok := extractEqFromAST(binOp, "n", nil)
		if ok {
			t.Fatal("expected ok=false when right is not a literal")
		}
	})

	t.Run("int literal right side", func(t *testing.T) {
		binOp := &ast.BinaryOp{
			Left:     prop,
			Operator: "=",
			Right:    &ast.IntLiteral{Value: 30},
		}
		key, val, ok := extractEqFromAST(binOp, "n", nil)
		if !ok {
			t.Fatal("expected ok=true for int literal")
		}
		if key != "name" || val != expr.IntegerValue(30) {
			t.Errorf("got key=%q val=%v", key, val)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// resolveSeekValue — all literal branches + parameter (bound/unbound/unsupported)
// ─────────────────────────────────────────────────────────────────────────────

func TestResolveSeekValue_AllBranches(t *testing.T) {
	t.Parallel()

	params := map[string]expr.Value{
		"name": expr.StringValue("Alice"),
	}

	cases := []struct {
		name    string
		value   string
		params  map[string]expr.Value
		wantErr bool
		wantVal expr.Value
	}{
		{
			name:    "double-quoted string",
			value:   `"hello"`,
			wantVal: expr.StringValue("hello"),
		},
		{
			name:    "single-quoted string",
			value:   `'world'`,
			wantVal: expr.StringValue("world"),
		},
		{
			name:    "bool true",
			value:   "true",
			wantVal: expr.BoolValue(true),
		},
		{
			name:    "bool false",
			value:   "false",
			wantVal: expr.BoolValue(false),
		},
		{
			name:    "integer",
			value:   "42",
			wantVal: expr.IntegerValue(42),
		},
		{
			name:    "param bound",
			value:   "$name",
			params:  params,
			wantVal: expr.StringValue("Alice"),
		},
		{
			name:    "param unbound nil params",
			value:   "$missing",
			params:  nil,
			wantVal: expr.Null,
		},
		{
			name:    "param unbound params present",
			value:   "$missing",
			params:  params,
			wantVal: expr.Null,
		},
		{
			name:    "unsupported value",
			value:   "???",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSeekValue(tc.value, tc.params)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVal {
				t.Errorf("got %v, want %v", got, tc.wantVal)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tryNewHashSeek — int64 hash index path (string path is covered by other tests)
// ─────────────────────────────────────────────────────────────────────────────

func TestTryNewHashSeek_Int64Index(t *testing.T) {
	t.Parallel()

	idx := hash.New[int64]()
	idx.Insert(int64(42), graph.NodeID(1))

	op, ok := tryNewHashSeek(idx, expr.IntegerValue(42))
	if !ok {
		t.Fatal("expected ok=true for int64 hash index")
	}
	if op == nil {
		t.Fatal("expected non-nil operator")
	}
}

func TestTryNewHashSeek_UnsupportedType(t *testing.T) {
	t.Parallel()

	// A type that implements index.Subscriber but neither hashStringLookup nor hashInt64Lookup.
	// Use a label hash index which holds *roaring64.Bitmap by label ID — it has no
	// Lookup(string) or Lookup(int64) method so tryNewHashSeek must return false.
	// We use the existing hash.New[bool]() which satisfies neither interface.
	boolIdx := hash.New[bool]()
	_, ok := tryNewHashSeek(boolIdx, expr.StringValue("x"))
	if ok {
		t.Fatal("expected ok=false for unsupported index type (bool hash)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// aggregateFactory — all function branches including unknown-function error
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregateFactory_AllBranches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fn       string
		argument string
		wantErr  bool
	}{
		{"count", "", false},
		{"count", "n", false},
		{"sum", "x", false},
		{"avg", "x", false},
		{"min", "x", false},
		{"max", "x", false},
		{"collect", "x", false},
		{"stdev", "x", false},
		{"stdevp", "x", false},
		{"UNKNOWN_FUNC", "x", true},
	}

	for _, tc := range cases {
		t.Run(tc.fn+"_"+tc.argument, func(t *testing.T) {
			f, err := aggregateFactory(tc.fn, tc.argument)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f == nil {
				t.Fatal("expected non-nil factory")
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildIndexSeekOperator — direct white-box call with a real Manager + hash index
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildIndexSeekOperator_StringHash(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("Alice")
	id, _ := g.AdjList().Mapper().Lookup("Alice")

	// Populate a string hash index named "person_name_hash".
	idx := hash.New[string]()
	idx.Insert("Alice", id)

	mgr := indexManagerFor(g)
	if err := mgr.CreateIndex("person_name_hash", idx); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	p := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	schema := make(map[string]int)

	op, err := buildIndexSeekOperator(p, nil, schema, mgr)
	if err != nil {
		t.Fatalf("buildIndexSeekOperator: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil operator")
	}
	if _, ok := schema["n"]; !ok {
		t.Error("expected 'n' in schema after buildIndexSeekOperator")
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}

	count := 0
	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			op.Close() //nolint:errcheck // test teardown
			t.Fatalf("op.Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	op.Close() //nolint:errcheck // test teardown
	if count != 1 {
		t.Errorf("expected 1 row from index seek, got %d", count)
	}
}

func TestBuildIndexSeekOperator_NilManager(t *testing.T) {
	t.Parallel()

	p := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	schema := make(map[string]int)

	_, err := buildIndexSeekOperator(p, nil, schema, nil)
	if err == nil {
		t.Fatal("expected error when index manager is nil")
	}
}

func TestBuildIndexSeekOperator_NoMatchingIndex(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	mgr := indexManagerFor(g)
	// Install a bool hash index; it satisfies neither hashStringLookup nor
	// hashInt64Lookup, so tryNewHashSeek returns (nil, false) for any seek value.
	idx := hash.New[bool]()
	if err := mgr.CreateIndex("bool_hash", idx); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	p := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	schema := make(map[string]int)

	_, err := buildIndexSeekOperator(p, nil, schema, mgr)
	if err == nil {
		t.Fatal("expected error when no matching hash index found")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildOperator — *ir.NodeByIndexSeek case (exercises the switch case and calls
// buildIndexSeekOperator). We call buildOperator directly because BuildPlan
// passes nil as idxMgr.
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildOperator_NodeByIndexSeekCase(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("Alice")
	id, _ := g.AdjList().Mapper().Lookup("Alice")

	idx := hash.New[string]()
	idx.Insert("Alice", id)
	mgr := indexManagerFor(g)
	if err := mgr.CreateIndex("person_name_hash3", idx); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}

	seek := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	schema := make(map[string]int)

	op, err := buildOperator(seek, walker, labelSrc, funcs.DefaultRegistry, nil, schema, mgr, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildOperator with NodeByIndexSeek: %v", err)
	}
	if op == nil {
		t.Fatal("expected non-nil operator")
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}

	count := 0
	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			op.Close() //nolint:errcheck // test teardown
			t.Fatalf("op.Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	op.Close() //nolint:errcheck // test teardown
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}
