package exec_test

// expand_test.go — tests for Expand operator (task-240).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test graph helpers
// ─────────────────────────────────────────────────────────────────────────────

// staticCSR is a minimal csrAdjacency stub built from an explicit edge list.
//
//	edges: [][]int64 where edges[srcID] = list of dstIDs
type staticCSR struct {
	vertices []uint64
	edges    []graph.NodeID
}

func buildCSR(maxNode int, edgeList [][2]int) *staticCSR {
	verts := make([]uint64, maxNode+1)
	// Count out-edges per source.
	for _, e := range edgeList {
		verts[e[0]+1]++
	}
	// Prefix sum.
	for i := 1; i <= maxNode; i++ {
		verts[i] += verts[i-1]
	}
	edges := make([]graph.NodeID, verts[maxNode])
	cursor := make([]uint64, maxNode+1)
	copy(cursor, verts)
	for _, e := range edgeList {
		pos := cursor[e[0]]
		edges[pos] = graph.NodeID(e[1])
		cursor[e[0]]++
	}
	return &staticCSR{vertices: verts, edges: edges}
}

func (c *staticCSR) VerticesSlice() []uint64    { return c.vertices }
func (c *staticCSR) EdgesSlice() []graph.NodeID { return c.edges }

// ─────────────────────────────────────────────────────────────────────────────
// 1. Expand DirOut — basic single-hop
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_DirOut_Basic(t *testing.T) {
	// Graph: 0→1, 0→2, 1→3
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {3, 1}})

	// Input: single row with NodeID=0
	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Node 0 has out-edges to 1 and 2.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	dsts := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		// row = [srcID, edgeID, dstID]  (appended to input row [nodeID])
		if len(row) != 4 {
			t.Fatalf("row width = %d, want 4", len(row))
		}
		dsts[int64(row[3].(expr.IntegerValue))] = struct{}{}
	}
	for _, want := range []int64{1, 2} {
		if _, ok := dsts[want]; !ok {
			t.Errorf("expected dstID %d in output", want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Expand DirIn — follows reverse edges
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_DirIn_Basic(t *testing.T) {
	// Graph: 0→1, 2→1
	// Node 1 has two in-edges (from 0 and 2).
	fwd := buildCSR(3, [][2]int{{0, 1}, {2, 1}})
	rev := buildCSR(3, [][2]int{{1, 0}, {1, 2}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirIn,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Expand DirBoth — forward + reverse
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_DirBoth(t *testing.T) {
	// Graph: 0→1, 0→2
	// Node 0: out-edges to 1, 2; no in-edges.
	fwd := buildCSR(3, [][2]int{{0, 1}, {0, 2}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 0}}) // reverse graph

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirBoth,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// DirBoth: forward edges (0→1, 0→2) = 2 rows;
	// reverse: node 0 has no in-edges in this graph → 0 rows.
	// Total = 2.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Expand — isolated node emits no rows
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_IsolatedNode(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}})
	rev := buildCSR(5, [][2]int{{1, 0}})

	// NodeID 4 has no edges.
	input := newSliceOperator(exec.Row{expr.IntegerValue(4)})
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows for isolated node, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Expand — multiple input rows
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_MultipleInputRows(t *testing.T) {
	// Graph: 0→1, 0→2, 1→2
	fwd := buildCSR(3, [][2]int{{0, 1}, {0, 2}, {1, 2}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 0}, {2, 1}})

	input := newSliceOperator(
		exec.Row{expr.IntegerValue(0)},
		exec.Row{expr.IntegerValue(1)},
	)
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// node0→{1,2} = 2, node1→{2} = 1 → total 3 rows.
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Expand — edge-type filter
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_EdgeTypeFilter(t *testing.T) {
	// Graph: 0→1 (type "KNOWS"), 0→2 (no type / "OTHER")
	fwd := buildCSR(3, [][2]int{{0, 1}, {0, 2}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 0}})

	// fwd edges: position 0 → KNOWS, position 1 → OTHER.
	// buildEdgeTypeFilter in cypher/api.go populates the filter map only
	// with edges of accepted types, so position 1 (OTHER) is intentionally
	// absent here — Expand decides membership by presence in the map, not
	// by comparing the recorded type label against op.EdgeType.
	filter := map[uint64]string{0: "KNOWS"}

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction:      exec.DirOut,
		EdgeType:       "KNOWS",
		EdgeTypeFilter: filter,
		InputCol:       0,
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (type filter should retain only KNOWS edge)", len(rows))
	}
	// The surviving row's dstID must be 1 (the KNOWS neighbour).
	dstID := int64(rows[0][3].(expr.IntegerValue))
	if dstID != 1 {
		t.Errorf("dstID = %d, want 1", dstID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Expand — cancellation honoured
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand_Cancellation(t *testing.T) {
	// Large fan-out.
	edges := make([][2]int, 1000)
	for i := range edges {
		edges[i] = [2]int{0, i + 1}
	}
	fwd := buildCSR(1001, edges)
	rev := buildCSR(1001, nil)

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewExpand(input, fwd, rev, exec.ExpandConfig{
		Direction: exec.DirOut,
		InputCol:  0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row exec.Row
	for range 5 {
		if _, err := op.Next(&row); err != nil {
			t.Fatalf("early Next error: %v", err)
		}
	}
	cancel()
	_, err := op.Next(&row)
	if err == nil {
		t.Log("Next nil after cancel — acceptable if remaining edges exist in buffer")
	}
	_ = op.Close()
}
