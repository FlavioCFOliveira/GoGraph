package cypher

// api_internal_extra_test.go — internal-package tests that target functions
// not reachable through the public Engine API at sufficient coverage:
// decodeTemporalString (all tag branches), buildUnwindOperator (nil-ListExpr
// fallback), and the walMutatorAdapter read-side helpers (HasEdge,
// DelEdgeProperty, OutDegree, ResolveNodeID, ResolveNodeLabel, WalkNodeIDs).
// These tests do not exercise WAL persistence — they only exercise the
// in-memory side of the adapter by attaching the adapter to a transient
// txn.Tx whose Begin / Rollback bookend the test.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ─────────────────────────────────────────────────────────────────────────────
// decodeTemporalString — every SOH-tag branch (0x01 .. 0x06) plus the
// short-input and unknown-tag fallbacks.
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodeTemporalString_AllTagBranches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// 0x01 → date
		{"date_valid", "\x012024-05-21", true},
		{"date_malformed", "\x01not-a-date", false},

		// 0x02 → local date-time
		{"localdatetime_valid", "\x022024-05-21T13:45:00", true},
		{"localdatetime_malformed", "\x02garbage", false},

		// 0x03 → zoned date-time
		{"datetime_valid", "\x032024-05-21T13:45:00Z", true},
		{"datetime_malformed", "\x03oops", false},

		// 0x04 → local time
		{"localtime_valid", "\x0413:45:00", true},
		{"localtime_malformed", "\x04nope", false},

		// 0x05 → zoned time
		{"time_valid", "\x0513:45:00Z", true},
		{"time_malformed", "\x05bad", false},

		// 0x06 → duration
		{"duration_valid", "\x06P1D", true},
		{"duration_malformed", "\x06oops", false},

		// Negative / boundary
		{"empty_string", "", false},
		{"single_byte", "\x01", false},
		{"unknown_tag", "\x07anything", false},
		{"plain_string", "no tag here", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := decodeTemporalString(tc.in)
			if ok != tc.want {
				t.Fatalf("decodeTemporalString(%q): got ok=%v, want %v (value=%v)", tc.in, ok, tc.want, v)
			}
			if tc.want && v == nil {
				t.Fatal("decoded ok but value is nil")
			}
			if !tc.want && v != nil {
				t.Fatalf("decoded false but value non-nil: %v", v)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildUnwindOperator — nil ListExpr branch
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildUnwindOperator_NilListExpr drives the early-return branch in
// buildUnwindOperator where the IR carries no parsed list expression: the
// produced Unwind operator must emit zero rows for every input row.
func TestBuildUnwindOperator_NilListExpr(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("A"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	walker := &lpgNodeWalker{g: g}

	// Build an Unwind IR node with a single-row Argument input and ListExpr=nil.
	// We use the public ir.NewUnwind constructor with the raw string variant
	// so ListExpr remains nil.
	u := &ir.Unwind{ElementVar: "x", ListExpression: "", ListExpr: nil}
	op, err := buildUnwindOperator(u, exec.NewArgument(), map[string]int{}, walker, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildUnwindOperator: %v", err)
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer op.Close()

	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		t.Errorf("nil-ListExpr Unwind should emit no rows, got row=%v", row)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// walMutatorAdapter — direct exercise of read-side / single-call helpers
// ─────────────────────────────────────────────────────────────────────────────

// newWALAdapter builds a walMutatorAdapter attached to a fresh WAL-backed
// store, with a single open Tx. The Tx is rolled back via t.Cleanup so the
// store mutex is released even on test failure.
func newWALAdapter(t *testing.T) *walMutatorAdapter {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	tx := store.Begin()
	t.Cleanup(func() { _ = tx.Rollback() })

	return &walMutatorAdapter{g: g, tx: tx, buf: &exec.IndexBuffer{}}
}

func TestWALMutatorAdapter_ResolveID_AbsentKey(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	// resolveID on an absent key returns graph.NodeID(0) per documented
	// fallback (and never panics on a fresh graph).
	if id := a.resolveID("does-not-exist"); id != graph.NodeID(0) {
		t.Errorf("resolveID(absent) = %d, want 0", id)
	}
}

func TestWALMutatorAdapter_AddNode_AddEdge_HasEdge(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	srcID, err := a.AddNode("S")
	if err != nil {
		t.Fatalf("AddNode(S): %v", err)
	}
	dstID, err := a.AddNode("D")
	if err != nil {
		t.Fatalf("AddNode(D): %v", err)
	}
	if srcID == 0 || dstID == 0 {
		t.Fatalf("AddNode returned zero NodeID: src=%d dst=%d", srcID, dstID)
	}

	gotSrcID, gotDstID, err := a.AddEdge("S", "D", 1.5)
	if err != nil {
		t.Fatalf("AddEdge(S,D): %v", err)
	}
	if gotSrcID != srcID || gotDstID != dstID {
		t.Fatalf("AddEdge returned (%d,%d), want (%d,%d)", gotSrcID, gotDstID, srcID, dstID)
	}

	if !a.HasEdge("S", "D") {
		t.Error("HasEdge(S,D) = false, want true after AddEdge")
	}
	if a.HasEdge("D", "S") {
		t.Error("HasEdge(D,S) = true on a directed graph with only S→D")
	}
	if a.HasEdge("S", "missing") {
		t.Error("HasEdge(S,missing) = true, want false")
	}
}

func TestWALMutatorAdapter_OutDegree_Neighbours(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	if _, err := a.AddNode("Hub"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("L1"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("L2"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("L3"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("In1"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	if _, _, err := a.AddEdge("Hub", "L1", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if _, _, err := a.AddEdge("Hub", "L2", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if _, _, err := a.AddEdge("Hub", "L3", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if _, _, err := a.AddEdge("In1", "Hub", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if deg := a.OutDegree("Hub"); deg != 3 {
		t.Errorf("OutDegree(Hub) = %d, want 3", deg)
	}
	if deg := a.OutDegree("missing"); deg != 0 {
		t.Errorf("OutDegree(missing) = %d, want 0", deg)
	}

	outs := a.OutNeighbours("Hub")
	if len(outs) != 3 {
		t.Errorf("OutNeighbours(Hub) returned %d names, want 3", len(outs))
	}

	ins := a.InNeighbours("Hub")
	if len(ins) != 1 || ins[0] != "In1" {
		t.Errorf("InNeighbours(Hub) = %v, want [In1]", ins)
	}
	if got := a.InNeighbours("missing"); got != nil {
		t.Errorf("InNeighbours(missing) = %v, want nil", got)
	}
}

func TestWALMutatorAdapter_SetAndDelEdgeProperty(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	if _, err := a.AddNode("S"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("D"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, _, err := a.AddEdge("S", "D", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	a.SetEdgeLabel("S", "D", "KNOWS")
	if err := a.SetEdgeProperty("S", "D", "since", lpg.Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	a.DelEdgeProperty("S", "D", "since")

	// IndexBuffer should now hold one Set + one Del + one AddEdgeLabel change.
	if got := a.buf.Len(); got < 3 {
		t.Errorf("expected at least 3 buffered changes, got %d", got)
	}
}

func TestWALMutatorAdapter_NodePropertyAndLabelRoundTrip(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	if _, err := a.AddNode("N"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.SetNodeLabel("N", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := a.SetNodeProperty("N", "name", lpg.StringValue("Alice")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	// Read back via the adapter.
	props := a.NodeProperties("N")
	if got, ok := props["name"]; !ok {
		t.Errorf("NodeProperties: missing key 'name'")
	} else if s, _ := got.String(); s != "Alice" {
		t.Errorf("NodeProperties name = %q, want Alice", s)
	}

	labels := a.NodeLabels("N")
	found := false
	for _, l := range labels {
		if l == "Person" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("NodeLabels missing Person: got %v", labels)
	}

	a.DelNodeProperty("N", "name")
	a.RemoveNodeLabel("N", "Person")
}

func TestWALMutatorAdapter_ResolveNodeID_WalkNodeIDs(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	if _, err := a.AddNode("P"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("Q"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("R"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	id, ok := a.ResolveNodeID("Q")
	if !ok {
		t.Fatal("ResolveNodeID(Q): not found")
	}
	if id == 0 {
		t.Error("ResolveNodeID(Q): id == 0")
	}
	if _, ok := a.ResolveNodeID("missing"); ok {
		t.Error("ResolveNodeID(missing): expected ok=false")
	}

	if label, ok := a.ResolveNodeLabel(id); !ok || label != "Q" {
		t.Errorf("ResolveNodeLabel(%d) = (%q,%v), want (\"Q\",true)", id, label, ok)
	}

	count := 0
	a.WalkNodeIDs(func(_ graph.NodeID) bool {
		count++
		return true
	})
	if count != 3 {
		t.Errorf("WalkNodeIDs visited %d nodes, want 3", count)
	}

	// Early-stop variant.
	visited := 0
	a.WalkNodeIDs(func(_ graph.NodeID) bool {
		visited++
		return false // stop after first
	})
	if visited != 1 {
		t.Errorf("WalkNodeIDs with early stop visited %d, want 1", visited)
	}
}

func TestWALMutatorAdapter_RemoveEdge(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)

	if _, err := a.AddNode("X"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, err := a.AddNode("Y"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if _, _, err := a.AddEdge("X", "Y", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if !a.HasEdge("X", "Y") {
		t.Fatal("setup: expected X→Y edge")
	}

	a.RemoveEdge("X", "Y")
	if a.HasEdge("X", "Y") {
		t.Error("HasEdge(X,Y) = true after RemoveEdge, want false")
	}
}

// TestWALMutatorAdapter_TimedSequence sanity-checks that a sequence of
// mutator calls completes promptly (no lock contention on the single-writer
// store).
func TestWALMutatorAdapter_TimedSequence(t *testing.T) {
	t.Parallel()
	a := newWALAdapter(t)
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 100; i++ {
		if _, err := a.AddNode("N"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("AddNode loop exceeded 2s at iteration %d", i)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// irDirToExec — direction conversion smoke
// ─────────────────────────────────────────────────────────────────────────────

func TestIrDirToExec_AllDirections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   ir.Direction
		want exec.Direction
	}{
		{ir.DirectionOutgoing, exec.DirOut},
		{ir.DirectionIncoming, exec.DirIn},
		{ir.DirectionBoth, exec.DirBoth},
	}
	for _, c := range cases {
		if got := irDirToExec(c.in); got != c.want {
			t.Errorf("irDirToExec(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// lpgPropToExpr fallback to plain string for non-temporal SOH-leading values
// (covers the StringValue branch when decodeTemporalString returns false).
// ─────────────────────────────────────────────────────────────────────────────

func TestLpgPropToExpr_NonTemporalString(t *testing.T) {
	t.Parallel()

	pv := lpg.StringValue("hello world")
	v := lpgPropToExpr(pv)
	sv, ok := v.(expr.StringValue)
	if !ok {
		t.Fatalf("expected StringValue, got %T", v)
	}
	if string(sv) != "hello world" {
		t.Errorf("got %q, want %q", string(sv), "hello world")
	}
}
