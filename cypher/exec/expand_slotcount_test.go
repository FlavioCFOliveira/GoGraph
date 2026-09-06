package exec

// expand_slotcount_test.go — the exactness gate on [Expand.storageAccesses]
// (rmp #2761).
//
// The counter is not incremented per slot. The cursors already advance one
// position per slot consumed, so the count is RECOVERED from their positions with
// a fixed two subtractions per input row ([Expand.closeSlotWindow]) — which is what makes
// it affordable to maintain unconditionally, and what makes it worth proving.
// Recovering a count is only exact if every cursor movement is either a slot that
// was walked or a window that was closed first, and there are three places a
// cursor moves without a walk:
//
//   - [Expand.loadAdjacency] jumps to the next source's run (closes the window);
//   - [Expand.Init] resets the reverse cursor to zero (closes the window);
//   - [Expand.seekIntoRuns] narrows the range by binary search (rebased AFTER,
//     so the skipped block is not charged).
//
// This file drives a matrix over every axis those three interact with —
// direction, type filter, expand-into with the seek on and off, a drain cut short
// mid-run, and a re-Init that abandons a partly walked source — and compares the
// reported figure against a count derived independently from the CSR shape.
//
// The oracle is deliberately NOT another cursor subtraction: it is the sum of the
// degrees of the sources actually visited, computed from the vertex offsets, which
// is what "slots of the source's adjacency run" means in the first place.
//
// A second, stronger verification was run once and is recorded rather than kept:
// a temporary per-slot probe incremented beside each `op.fwdStart++` /
// `op.revStart++` and a panic in storageAccesses when the two disagreed. It ran
// green over ./cypher/ and ./cypher/exec/ in full. The probe is not kept because
// it is exactly the per-slot increment the design exists to avoid.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// slotCSR is a minimal [CSRAdjacency] over explicit arrays.
type slotCSR struct {
	vertices []uint64
	edges    []graph.NodeID
	handles  []uint64
}

func (c *slotCSR) VerticesSlice() []uint64    { return c.vertices }
func (c *slotCSR) EdgesSlice() []graph.NodeID { return c.edges }
func (c *slotCSR) HandlesSlice() []uint64     { return c.handles }

// degree returns the number of slots node uid occupies in this adjacency — the
// oracle's unit.
func (c *slotCSR) degree(uid uint64) int64 {
	if uid+1 >= uint64(len(c.vertices)) {
		return 0
	}
	return int64(c.vertices[uid+1] - c.vertices[uid])
}

// buildSlotCSR builds a destination-ordered CSR over n nodes, with one handle per
// slot numbered by position. Destination ordering is what [Expand.seekIntoRuns]
// requires, so the seek arm of the matrix exercises the real access path.
func buildSlotCSR(n int, arcs [][2]int) *slotCSR {
	verts := make([]uint64, n+1)
	for _, a := range arcs {
		verts[a[0]+1]++
	}
	for i := 1; i <= n; i++ {
		verts[i] += verts[i-1]
	}
	edges := make([]graph.NodeID, verts[n])
	cursor := make([]uint64, n+1)
	copy(cursor, verts)
	// Insert in destination order within each run: walk destinations ascending.
	for d := 0; d < n; d++ {
		for _, a := range arcs {
			if a[1] != d {
				continue
			}
			edges[cursor[a[0]]] = graph.NodeID(a[1])
			cursor[a[0]]++
		}
	}
	handles := make([]uint64, len(edges))
	for i := range handles {
		handles[i] = uint64(i)
	}
	return &slotCSR{vertices: verts, edges: edges, handles: handles}
}

// reverseArcs transposes an arc list so a reverse CSR can be built from it.
func reverseArcs(arcs [][2]int) [][2]int {
	out := make([][2]int, len(arcs))
	for i, a := range arcs {
		out[i] = [2]int{a[1], a[0]}
	}
	return out
}

// fanGraph is the shape refutation 2 of the honesty audit measured on, generalised:
// node 0 has `fan` out-arcs, of which exactly `admitted` are of the accepted type.
// Node 1 also carries out-arcs so a multi-source drain has more than one window to
// close. Returns the forward and reverse adjacency and the accepted forward
// positions.
func fanGraph(fan, admitted int) (fwd, rev *slotCSR, accept map[uint64]string) {
	arcs := make([][2]int, 0, fan+2)
	for i := 0; i < fan; i++ {
		arcs = append(arcs, [2]int{0, i + 2})
	}
	arcs = append(arcs, [2]int{1, 2}, [2]int{1, 3})
	n := fan + 4
	fwd = buildSlotCSR(n, arcs)
	rev = buildSlotCSR(n, reverseArcs(arcs))
	accept = map[uint64]string{}
	// Admit the LAST `admitted` slots of node 0's run, so a filtered walk must
	// traverse the whole run before it emits anything.
	start, end := fwd.vertices[0], fwd.vertices[1]
	for p := end - uint64(admitted); p < end; p++ {
		accept[p] = "KNOWS"
		_ = start
	}
	return fwd, rev, accept
}

// drainSlots runs op for at most limit rows (limit < 0 drains fully) and returns
// the rows emitted.
func drainSlots(ctx context.Context, t *testing.T, op *Expand, limit int) int {
	t.Helper()
	n := 0
	for limit < 0 || n < limit {
		var row Row
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		n++
	}
	return n
}

// TestExpandStorageAccesses_CountsEverySlotWalked is the exactness gate.
func TestExpandStorageAccesses_CountsEverySlotWalked(t *testing.T) {
	t.Parallel()
	const fan = 100
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		dir       Direction
		admitted  int   // 0 = no type filter at all
		srcs      []int // input rows, in order
		limit     int   // -1 = drain fully
		wantSlots int64 // the independent oracle
		wantRows  int
	}{
		{
			name: "DirOut unfiltered full drain", dir: DirOut, srcs: []int{0},
			limit: -1, wantSlots: fan, wantRows: fan,
		},
		{
			// The audit's refutation 2, in the operator: the SAME walk, one row out.
			name: "DirOut type-filtered full drain", dir: DirOut, admitted: 1, srcs: []int{0},
			limit: -1, wantSlots: fan, wantRows: 1,
		},
		{
			name: "DirIn unfiltered full drain", dir: DirIn, srcs: []int{2},
			limit: -1, wantSlots: 2, wantRows: 2, // node 2 is reached from 0 and from 1
		},
		{
			name: "DirBoth walks both runs", dir: DirBoth, srcs: []int{0},
			limit: -1, wantSlots: fan, wantRows: fan, // node 0 has no in-arcs
		},
		{
			name: "DirBoth on a node with both", dir: DirBoth, srcs: []int{2},
			limit: -1, wantSlots: 2, wantRows: 2,
		},
		{
			name: "two sources, both windows closed", dir: DirOut, srcs: []int{0, 1},
			limit: -1, wantSlots: fan + 2, wantRows: fan + 2,
		},
		{
			// Early termination: the open window must still be reported.
			name: "drain cut short after one row", dir: DirOut, srcs: []int{0},
			limit: 1, wantSlots: 1, wantRows: 1,
		},
		{
			// Early termination with a filter that admits only the LAST slot: one row
			// emitted, the whole run walked. A figure of 1 here is the pre-#2761 count.
			name: "drain cut short after a filtered scan", dir: DirOut, admitted: 1, srcs: []int{0},
			limit: 1, wantSlots: fan, wantRows: 1,
		},
		{
			name: "a source with no adjacency at all", dir: DirOut, srcs: []int{fan + 3},
			limit: -1, wantSlots: 0, wantRows: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fwd, rev, accept := fanGraph(fan, tc.admitted)
			if tc.admitted == 0 {
				accept = nil
			}
			rows := make([]Row, len(tc.srcs))
			for i, s := range tc.srcs {
				rows[i] = Row{expr.IntegerValue(s)}
			}
			cfg := ExpandConfig{Direction: tc.dir, InputCol: 0}
			if tc.admitted > 0 {
				cfg.EdgeType = "KNOWS"
			}
			op := NewExpand(NewStaticRows(rows), StaticAdjacency(fwd, rev, accept), cfg)
			if err := op.Init(ctx); err != nil {
				t.Fatalf("Init: %v", err)
			}
			got := drainSlots(ctx, t, op, tc.limit)
			if got != tc.wantRows {
				t.Fatalf("emitted %d rows, want %d — the shape this case measures is not the "+
					"shape it built", got, tc.wantRows)
			}
			if slots := op.storageAccesses(); slots != tc.wantSlots {
				t.Errorf("storageAccesses() = %d, want %d. The figure must be the adjacency "+
					"slots the cursors CONSUMED — walked or rejected — not the rows emitted "+
					"(%d) (rmp #2761)", slots, tc.wantSlots, got)
			}
		})
	}
}

// TestExpandStorageAccesses_AccumulatesAcrossReInit pins the lifetime convention:
// Init runs once per OUTER ROW under a correlated Apply, so a counter reset there
// would report the last outer row's walk as the whole operator's. It is the same
// convention [VarLengthExpand.Init] follows.
//
// The second run is deliberately ABANDONED mid-run, which is the normal case (an
// EXISTS stops at the first match, a LIMIT at the n-th): the slots that run walked
// must survive the Init that follows it.
func TestExpandStorageAccesses_AccumulatesAcrossReInit(t *testing.T) {
	t.Parallel()
	const fan = 100
	ctx := context.Background()
	fwd, rev, _ := fanGraph(fan, 0)

	op := NewExpand(NewStaticRows([]Row{{expr.IntegerValue(0)}}),
		StaticAdjacency(fwd, rev, nil), ExpandConfig{Direction: DirOut, InputCol: 0})

	// Run 1: full drain — the whole run.
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 1: %v", err)
	}
	drainSlots(ctx, t, op, -1)
	if got := op.storageAccesses(); got != fan {
		t.Fatalf("after run 1: storageAccesses() = %d, want %d", got, fan)
	}
	// Run 2: abandoned after 3 rows. Init must BANK those 3 slots, not erase them.
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 2: %v", err)
	}
	drainSlots(ctx, t, op, 3)
	if got := op.storageAccesses(); got != fan+3 {
		t.Fatalf("after run 2 (abandoned at 3 rows): storageAccesses() = %d, want %d",
			got, fan+3)
	}
	// Run 3: the Init that follows the abandonment must carry the 3 forward.
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 3: %v", err)
	}
	drainSlots(ctx, t, op, -1)
	if got := op.storageAccesses(); got != 2*fan+3 {
		t.Errorf("after run 3: storageAccesses() = %d, want %d. A re-Init must not erase "+
			"slots PROFILE has yet to report — an operator driven once per outer row under "+
			"an Apply reports its WHOLE lifetime (rmp #2761)", got, 2*fan+3)
	}
}

// TestExpandStorageAccesses_SeekIsNotChargedForSlotsItSkipped pins the one place
// the figure is deliberately LOWER than the run length.
//
// [Expand.seekIntoRuns] narrows the cursor to the bound destination's contiguous
// block by binary search. The slots it steps over are not read, so charging them
// would report the Θ(d) walk the seek exists to avoid — the counter would then
// make an optimisation invisible, which is the opposite of what the column is for.
// The control is the SAME query with the seek turned off, which does walk the run
// and must report it.
func TestExpandStorageAccesses_SeekIsNotChargedForSlotsItSkipped(t *testing.T) {
	t.Parallel()
	const fan = 100
	ctx := context.Background()
	fwd, rev, _ := fanGraph(fan, 0)

	// One input row binding the source (col 0) and an already-bound destination
	// (col 1) — the expand-into shape.
	const boundDst = 50
	run := func(seek bool) (int64, int) {
		op := NewExpand(NewStaticRows([]Row{{expr.IntegerValue(0), expr.IntegerValue(boundDst)}}),
			StaticAdjacency(fwd, rev, nil), ExpandConfig{Direction: DirOut, InputCol: 0}).
			WithExpandInto(1).WithExpandIntoSeek(seek)
		if err := op.Init(ctx); err != nil {
			t.Fatalf("Init(seek=%v): %v", seek, err)
		}
		rows := drainSlots(ctx, t, op, -1)
		return op.storageAccesses(), rows
	}

	seekSlots, seekRows := run(true)
	filterSlots, filterRows := run(false)

	if seekRows != 1 || filterRows != 1 {
		t.Fatalf("the two arms emitted %d and %d rows, want 1 each — they must be "+
			"result-identical or the comparison below is meaningless", seekRows, filterRows)
	}
	if filterSlots != fan {
		t.Errorf("filter-only expand-into reported %d slots, want %d: with the seek off "+
			"the operator walks the whole run and rejects every slot but one, and a slot "+
			"read and rejected is still a read", filterSlots, fan)
	}
	if seekSlots >= filterSlots {
		t.Errorf("the seeking expand-into reported %d slots and the filter-only arm %d. "+
			"The seek narrows the cursor by binary search and never reads the block it "+
			"stepped over, so it must report FEWER (rmp #2761)", seekSlots, filterSlots)
	}
	if seekSlots != 1 {
		t.Errorf("the seeking expand-into reported %d slots, want 1 — node %d occupies "+
			"exactly one slot of the destination-ordered run", seekSlots, boundDst)
	}
}

// TestExpandStorageAccesses_MatchesTheOracleOverAFuzzedMatrix drives a wider set of
// shapes than the table above and checks each against the degree-sum oracle. It is
// the sweep that would catch a window closed in the wrong ORDER — a defect a single
// hand-written case can miss because it needs two sources and both directions to
// show up.
func TestExpandStorageAccesses_MatchesTheOracleOverAFuzzedMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A denser, less regular shape than the fan: every node points at a few others,
	// so out-degrees and in-degrees differ per node.
	const n = 24
	var arcs [][2]int
	for i := 0; i < n-1; i++ {
		for k := 1; k <= (i%4)+1 && i+k < n; k++ {
			arcs = append(arcs, [2]int{i, i + k})
		}
	}
	fwd := buildSlotCSR(n, arcs)
	rev := buildSlotCSR(n, reverseArcs(arcs))

	dirs := []struct {
		name string
		d    Direction
	}{{"DirOut", DirOut}, {"DirIn", DirIn}, {"DirBoth", DirBoth}}

	for _, dc := range dirs {
		for _, nsrc := range []int{1, 3, n} {
			t.Run(fmt.Sprintf("%s/%dsrc", dc.name, nsrc), func(t *testing.T) {
				t.Parallel()
				rows := make([]Row, nsrc)
				var want int64
				for i := 0; i < nsrc; i++ {
					uid := uint64(i * 2 % n)
					rows[i] = Row{expr.IntegerValue(int64(uid))}
					if dc.d != DirIn {
						want += fwd.degree(uid)
					}
					if dc.d != DirOut {
						want += rev.degree(uid)
					}
				}
				op := NewExpand(NewStaticRows(rows), StaticAdjacency(fwd, rev, nil),
					ExpandConfig{Direction: dc.d, InputCol: 0})
				if err := op.Init(ctx); err != nil {
					t.Fatalf("Init: %v", err)
				}
				drainSlots(ctx, t, op, -1)
				if got := op.storageAccesses(); got != want {
					t.Errorf("storageAccesses() = %d, want %d (sum of the visited sources' "+
						"degrees in the directions this operator walks)", got, want)
				}
			})
		}
	}
}

// TestOptionalExpandStorageAccesses_ForwardsTheInnerCount is the OptionalExpand
// arm of rmp #2761.
//
// # Why this is an exec-level test and not an engine-level PROFILE
//
// [OptionalExpand] is NOT REACHABLE from a Cypher query on this tree. It is built
// only for an ir.OptionalExpand, which cypher/ir/match.go:1960 emits only when
// matchPattern is called with optional=true — and the sole caller,
// cypher/ir/translator.go:375, passes false unconditionally. Every OPTIONAL MATCH
// plans an OptionalApply over a plain Expand instead (verified by rendering
// `OPTIONAL MATCH (r:Root)-[:KNOWS]->(b) RETURN b`, which yields
// OptionalApply → Expand). So no PROFILE output can exercise this operator, and a
// test that went through the engine would be measuring Expand under another name.
//
// The classification still has to be right, because the operator is built,
// registered in the db-hits census, and would report the moment the translator
// wires it. That it is unwired is recorded as a finding of rmp #2761, not fixed
// here.
//
// # What the derived figure got wrong, twice
//
// It counted the ADMITTED slots rather than the walked ones, exactly as Expand
// did. It ALSO counted the NULL-EXTENSION rows, which are produced after reading
// nothing at all: a source with no matching edge emitted one row and therefore
// charged one db-hit for zero slots. Both cases are asserted below.
func TestOptionalExpandStorageAccesses_ForwardsTheInnerCount(t *testing.T) {
	t.Parallel()
	const fan = 100
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		admitted  int
		srcs      []int
		wantRows  int
		wantSlots int64
	}{
		{
			// The type-filtered walk: 100 slots read, one edge admitted.
			name: "type-filtered hit", admitted: 1, srcs: []int{0},
			wantRows: 1, wantSlots: fan,
		},
		{
			name: "unfiltered", admitted: 0, srcs: []int{0},
			wantRows: fan, wantSlots: fan,
		},
		{
			// The NULL-extension case: one row out, ZERO slots read. The derived
			// figure charged 1 here.
			name: "no match at all pads a row and reads nothing", admitted: 0, srcs: []int{fan + 3},
			wantRows: 1, wantSlots: 0,
		},
		{
			// A type filter that admits nothing: the whole run is walked, one padded
			// row comes out. The derived figure charged 1 for 100 slots.
			name: "filter admits nothing", admitted: 0, srcs: []int{0},
			wantRows: fan, wantSlots: fan,
		},
		{
			name: "matching and padded sources together", admitted: 1, srcs: []int{0, fan + 3},
			wantRows: 2, wantSlots: fan,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fwd, rev, accept := fanGraph(fan, tc.admitted)
			cfg := ExpandConfig{Direction: DirOut, InputCol: 0}
			if tc.admitted > 0 {
				cfg.EdgeType = "KNOWS"
			} else {
				accept = nil
			}
			rows := make([]Row, len(tc.srcs))
			for i, s := range tc.srcs {
				rows[i] = Row{expr.IntegerValue(s)}
			}
			op := NewOptionalExpand(NewStaticRows(rows), StaticAdjacency(fwd, rev, accept), cfg)
			if err := op.Init(ctx); err != nil {
				t.Fatalf("Init: %v", err)
			}
			n := 0
			for {
				var row Row
				ok, err := op.Next(&row)
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if !ok {
					break
				}
				n++
			}
			if n != tc.wantRows {
				t.Fatalf("emitted %d rows, want %d — the shape this case measures is not "+
					"the shape it built", n, tc.wantRows)
			}
			if got := op.storageAccesses(); got != tc.wantSlots {
				t.Errorf("storageAccesses() = %d, want %d. OptionalExpand must forward the "+
					"inner Expand's SLOT count; the derived figure it replaced was its own row "+
					"count, which counts padded non-match rows that read nothing (rmp #2761)",
					got, tc.wantSlots)
			}
		})
	}
}
