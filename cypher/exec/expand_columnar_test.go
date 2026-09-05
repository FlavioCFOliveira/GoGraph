package exec_test

// expand_columnar_test.go — differential tests for the Expand ChunkProducer path
// (rmp #2106). Every scenario drives the SAME traversal two ways and asserts a
// byte-identical result MULTISET:
//
//   - row mode  (OFF): Expand.Next drained row-at-a-time — the behavioural fallback;
//   - chunk mode (ON): NewColumnarExpand(...).FillChunk drained column-major.
//
// Because the two paths share the edge-decision helpers (advanceFwdEdge/
// advanceRevEdge), these tests are the guard that the ADDITIVE chunk plumbing — the
// two-level fan-out cursor with cross-call resume, the child-batch refill, the
// passthrough column copy, and the multiplicity re-emit — never corrupts, drops, or
// duplicates a row relative to the proven row path, across DirOut/DirIn/DirBoth, an
// edge-type filter, multigraph parallel edges on a reverse hop, cyphermorphism, and
// a high-degree node whose fan-out spans many FillChunk calls.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// nodeIDChunkSource emits a fixed sequence of NodeIDs both row-at-a-time (Next) and
// column-major (FillChunk), so an Expand fed by it can be driven either way from the
// SAME input. batch caps the rows emitted per FillChunk call (0 = unlimited),
// forcing the Expand's cScratch to refill and exercising the child-batch boundary.
type nodeIDChunkSource struct {
	ids   []int64
	batch int
	idx   int
}

func (s *nodeIDChunkSource) Init(context.Context) error { s.idx = 0; return nil }
func (s *nodeIDChunkSource) Close() error               { return nil }

func (s *nodeIDChunkSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= len(s.ids) {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(s.ids[s.idx])}
	s.idx++
	return true, nil
}

func (s *nodeIDChunkSource) NewOutputChunk(capacity int) *exec.Chunk {
	return exec.NewChunk(capacity, expr.KindInteger)
}

func (s *nodeIDChunkSource) FillChunk(dst *exec.Chunk, maxRows int) (int, error) {
	lim := maxRows
	if s.batch > 0 && s.batch < lim {
		lim = s.batch
	}
	n := 0
	for n < lim && s.idx < len(s.ids) {
		dst.AppendInt64(0, s.ids[s.idx])
		s.idx++
		n++
	}
	return n, nil
}

// sortedRowKeys renders each row via the shared [rowKey] canonicaliser and sorts,
// so two result multisets compare byte-identically regardless of emission order.
func sortedRowKeys(rows []exec.Row) []string {
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = rowKey(r)
	}
	sort.Strings(keys)
	return keys
}

func equalKeys(a, b []string) (int, bool) {
	if len(a) != len(b) {
		return -1, false
	}
	for i := range a {
		if a[i] != b[i] {
			return i, false
		}
	}
	return -1, true
}

// drainExpandRow drains a plain row-mode Expand over ids and returns its rows.
func drainExpandRow(t *testing.T, ids []int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig) []exec.Row {
	t.Helper()
	op := exec.NewExpand(&nodeIDChunkSource{ids: ids}, exec.StaticAdjacency(fwd, rev, filter), cfg)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("row-mode Drain: %v", err)
	}
	return rows
}

// drainExpandChunk drains a columnarExpand over ids via FillChunk with the given
// per-call cap and child-batch size, boxing the resulting chunk back to rows.
func drainExpandChunk(t *testing.T, ids []int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig, perCall, batch int) []exec.Row {
	t.Helper()
	base := exec.NewExpand(&nodeIDChunkSource{ids: ids, batch: batch}, exec.StaticAdjacency(fwd, rev, filter), cfg)
	cp, ok := exec.NewColumnarExpand(base)
	if !ok {
		t.Fatalf("NewColumnarExpand: child was not recognised as a ChunkProducer")
	}
	if err := cp.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := cp.NewOutputChunk(exec.DefaultChunkCapacity)
	for {
		before := dst.Len()
		n, err := cp.FillChunk(dst, perCall)
		if err != nil {
			t.Fatalf("FillChunk: %v", err)
		}
		if got := dst.Len() - before; got != n {
			t.Fatalf("FillChunk returned n=%d but appended %d rows", n, got)
		}
		if n < perCall {
			break // n < perCall ⇔ end-of-stream
		}
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rows := make([]exec.Row, dst.Len())
	for i := range rows {
		rows[i] = dst.BoxRow(i, nil)
	}
	return rows
}

// assertColumnarMatchesRow drives the chunk path across a matrix of per-call caps
// and child-batch sizes (to exercise both cursor levels and their cross-call
// resume) and asserts every run yields the row-mode multiset byte-for-byte.
func assertColumnarMatchesRow(t *testing.T, ids []int64, fwd, rev *staticCSR, filter map[uint64]string, cfg exec.ExpandConfig) {
	t.Helper()
	want := sortedRowKeys(drainExpandRow(t, ids, fwd, rev, filter, cfg))
	for _, perCall := range []int{1, 2, 3, 7, 64, exec.DefaultChunkCapacity} {
		for _, batch := range []int{0, 1, 2, 5} {
			got := sortedRowKeys(drainExpandChunk(t, ids, fwd, rev, filter, cfg, perCall, batch))
			if len(got) != len(want) {
				t.Fatalf("perCall=%d batch=%d: got %d rows, want %d", perCall, batch, len(got), len(want))
			}
			if i, ok := equalKeys(want, got); !ok {
				t.Fatalf("perCall=%d batch=%d: row multiset mismatch at %d:\n want %q\n  got %q",
					perCall, batch, i, want[i], got[i])
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Direction × filter differential cases
// ─────────────────────────────────────────────────────────────────────────────

func TestExpandColumnar_DirOut(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {2, 0}, {3, 1}, {3, 2}, {4, 3}})
	ids := []int64{0, 1, 2, 3, 4}
	assertColumnarMatchesRow(t, ids, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0})
}

func TestExpandColumnar_DirIn(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}, {2, 1}, {3, 1}, {1, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {1, 2}, {1, 3}, {4, 1}})
	ids := []int64{0, 1, 2, 3, 4}
	assertColumnarMatchesRow(t, ids, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirIn, InputCol: 0})
}

func TestExpandColumnar_DirBoth(t *testing.T) {
	// Includes a self-loop (2→2) so the DirBoth undirected self-loop dedup is
	// exercised on both paths.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {1, 2}, {2, 2}, {2, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {2, 1}, {2, 2}, {3, 2}})
	ids := []int64{0, 1, 2, 3}
	assertColumnarMatchesRow(t, ids, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirBoth, InputCol: 0})
}

func TestExpandColumnar_EdgeTypeFilter(t *testing.T) {
	// fwd positions: 0:(0→1) 1:(0→2) 2:(0→3) 3:(1→3). Accept only positions {0,2}.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {3, 0}, {3, 1}})
	filter := map[uint64]string{0: "KNOWS", 2: "KNOWS"}
	ids := []int64{0, 1}
	assertColumnarMatchesRow(t, ids, fwd, rev, filter, exec.ExpandConfig{
		Direction: exec.DirOut, EdgeType: "KNOWS", InputCol: 0,
	})
}

func TestExpandColumnar_EdgeTypeFilter_Reverse(t *testing.T) {
	// DirIn with a type filter: the reverse hop must resolve the forward position
	// through reverseEdgePassesFilter identically on both paths.
	fwd := buildCSR(4, [][2]int{{0, 1}, {2, 1}, {3, 1}})
	rev := buildCSR(4, [][2]int{{1, 0}, {1, 2}, {1, 3}})
	filter := map[uint64]string{0: "R", 2: "R"} // accept (0→1) and (3→1), not (2→1)
	ids := []int64{1}
	assertColumnarMatchesRow(t, ids, fwd, rev, filter, exec.ExpandConfig{
		Direction: exec.DirIn, EdgeType: "R", InputCol: 0,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Multigraph parallel edges — per-instance relationship typing on a reverse hop
// (rmp #1634/#1685): the reverse traversal must recover the SPECIFIC forward edge
// position for each parallel edge via its stable handle, not collapse them.
// ─────────────────────────────────────────────────────────────────────────────

func TestExpandColumnar_Multigraph_ParallelEdges_ReverseHop(t *testing.T) {
	// Node 0 has THREE parallel edges to node 1, forward positions 0,1,2 with
	// distinct handles. DirIn from node 1 must emit three rows whose edgeIDs are
	// exactly {0,1,2}. buildRevFromFwd is not enough here (handles are the point),
	// so the CSRs are built explicitly with matching handles.
	fwd := &staticCSR{
		vertices: []uint64{0, 3, 3}, // node0: [0,3), node1: [3,3)
		edges:    []graph.NodeID{1, 1, 1},
		handles:  []uint64{100, 200, 300},
	}
	rev := &staticCSR{
		vertices: []uint64{0, 0, 3}, // node0: [0,0), node1: [0,3)
		edges:    []graph.NodeID{0, 0, 0},
		handles:  []uint64{100, 200, 300},
	}
	ids := []int64{0, 1}
	assertColumnarMatchesRow(t, ids, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirIn, InputCol: 0})

	// Sanity: the reverse hop from node 1 must recover three DISTINCT edgeIDs.
	rows := drainExpandChunk(t, []int64{1}, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirIn, InputCol: 0}, 2, 0)
	if len(rows) != 3 {
		t.Fatalf("reverse hop over 3 parallel edges: got %d rows, want 3", len(rows))
	}
	seen := map[int64]struct{}{}
	for _, r := range rows {
		seen[int64(r[2].(expr.IntegerValue))] = struct{}{} // edgeID column
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 distinct per-instance edgeIDs, got %d: %v", len(seen), seen)
	}
}

func TestExpandColumnar_Multigraph_DirBoth(t *testing.T) {
	// Parallel edges both directions with handles; DirBoth exercises forward
	// multiplicity + reverse per-instance recovery together.
	fwd := &staticCSR{
		vertices: []uint64{0, 2, 3},
		edges:    []graph.NodeID{1, 1, 0}, // 0→1, 0→1, 1→0
		handles:  []uint64{10, 20, 30},
	}
	rev := &staticCSR{
		vertices: []uint64{0, 1, 3},
		edges:    []graph.NodeID{1, 0, 0}, // into 0: (1→0); into 1: (0→1)×2
		handles:  []uint64{30, 10, 20},
	}
	ids := []int64{0, 1}
	assertColumnarMatchesRow(t, ids, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirBoth, InputCol: 0})
}

// ─────────────────────────────────────────────────────────────────────────────
// Cyphermorphism — a sibling relationship column excludes the same edge instance.
// ─────────────────────────────────────────────────────────────────────────────

// relColSource emits rows of [nodeID, siblingEdgeID] both ways, so the Expand's
// RelCols morphism check can be exercised over a columnar input carrying a prior
// hop's edge id in column 1.
type relColSource struct {
	rows  [][2]int64 // {nodeID, siblingEdgeID}
	batch int
	idx   int
}

func (s *relColSource) Init(context.Context) error { s.idx = 0; return nil }
func (s *relColSource) Close() error               { return nil }

func (s *relColSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(s.rows[s.idx][0]), expr.IntegerValue(s.rows[s.idx][1])}
	s.idx++
	return true, nil
}

func (s *relColSource) NewOutputChunk(capacity int) *exec.Chunk {
	return exec.NewChunk(capacity, expr.KindInteger, expr.KindInteger)
}

func (s *relColSource) FillChunk(dst *exec.Chunk, maxRows int) (int, error) {
	lim := maxRows
	if s.batch > 0 && s.batch < lim {
		lim = s.batch
	}
	n := 0
	for n < lim && s.idx < len(s.rows) {
		dst.AppendInt64(0, s.rows[s.idx][0])
		dst.AppendInt64(1, s.rows[s.idx][1])
		s.idx++
		n++
	}
	return n, nil
}

func TestExpandColumnar_Cyphermorphism(t *testing.T) {
	// Node 0 out-edges at forward positions 0:(0→1) 1:(0→2) 2:(0→3). The input row
	// carries a sibling edge id in column 1; the Expand must exclude the edge whose
	// forward position equals that sibling id. RelCols = [1]. InputCol = 0.
	fwd := buildCSR(4, [][2]int{{0, 1}, {0, 2}, {0, 3}})
	rev := buildCSR(4, [][2]int{{1, 0}, {2, 0}, {3, 0}})
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0, RelCols: []int{1}}

	// Two input rows: one excludes edge position 1, the other excludes position 2.
	inRows := [][2]int64{{0, 1}, {0, 2}}

	rowOp := exec.NewExpand(&relColSource{rows: inRows}, exec.StaticAdjacency(fwd, rev, nil), cfg)
	wantRows, err := exec.Drain(context.Background(), rowOp)
	if err != nil {
		t.Fatalf("row-mode Drain: %v", err)
	}
	want := sortedRowKeys(wantRows)

	for _, perCall := range []int{1, 3, 64} {
		for _, batch := range []int{0, 1} {
			base := exec.NewExpand(&relColSource{rows: inRows, batch: batch}, exec.StaticAdjacency(fwd, rev, nil), cfg)
			cp, ok := exec.NewColumnarExpand(base)
			if !ok {
				t.Fatalf("NewColumnarExpand: not a ChunkProducer")
			}
			if err := cp.Init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			dst := cp.NewOutputChunk(exec.DefaultChunkCapacity)
			for {
				before := dst.Len()
				n, ferr := cp.FillChunk(dst, perCall)
				if ferr != nil {
					t.Fatalf("FillChunk: %v", ferr)
				}
				if got := dst.Len() - before; got != n {
					t.Fatalf("n=%d but appended %d", n, got)
				}
				if n < perCall {
					break
				}
			}
			gotRows := make([]exec.Row, dst.Len())
			for i := range gotRows {
				gotRows[i] = dst.BoxRow(i, nil)
			}
			got := sortedRowKeys(gotRows)
			if i, eq := equalKeys(want, got); !eq {
				t.Fatalf("perCall=%d batch=%d: morphism multiset mismatch at %d:\n want %v\n  got %v",
					perCall, batch, i, want, got)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Chunk-boundary fan-out resume — a single high-degree source whose neighbours
// span many FillChunk calls, proving the two-level cursor resumes exactly.
// ─────────────────────────────────────────────────────────────────────────────

func TestExpandColumnar_HighDegree_FanOutResume(t *testing.T) {
	const deg = 5000 // > DefaultChunkCapacity, so the output backing also grows
	edges := make([][2]int, deg)
	for i := range edges {
		edges[i] = [2]int{0, i + 1}
	}
	fwd := buildCSR(deg+1, edges)
	rev := buildCSR(deg+1, nil)
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}

	want := sortedRowKeys(drainExpandRow(t, []int64{0}, fwd, rev, nil, cfg))
	if len(want) != deg {
		t.Fatalf("row-mode fan-out: got %d rows, want %d", len(want), deg)
	}
	// Tiny per-call caps force the fan-out of the single source across ~deg/perCall
	// FillChunk calls; the cursor (fwdStart) must resume mid-adjacency each time.
	for _, perCall := range []int{1, 3, 17, 1000} {
		got := sortedRowKeys(drainExpandChunk(t, []int64{0}, fwd, rev, nil, cfg, perCall, 0))
		if len(got) != deg {
			t.Fatalf("perCall=%d: got %d rows, want %d (resume dropped/duplicated)", perCall, len(got), deg)
		}
		if i, ok := equalKeys(want, got); !ok {
			t.Fatalf("perCall=%d: fan-out resume mismatch at %d: want %q got %q", perCall, i, want[i], got[i])
		}
	}
}

func TestExpandColumnar_MultiSource_BatchBoundary(t *testing.T) {
	// Many sources, each with a small fan-out, and a tiny child batch, so the
	// cScratch refill boundary interleaves with the fan-out cursor.
	var edges [][2]int
	for src := 0; src < 50; src++ {
		for k := 0; k < 4; k++ {
			edges = append(edges, [2]int{src, (src + k + 1) % 60})
		}
	}
	fwd := buildCSR(60, edges)
	rev := buildCSR(60, nil)
	ids := make([]int64, 50)
	for i := range ids {
		ids[i] = int64(i)
	}
	assertColumnarMatchesRow(t, ids, fwd, rev, nil, exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0})
}

// ─────────────────────────────────────────────────────────────────────────────
// Output-chunk schema and capacity (rmp #2656)
// ─────────────────────────────────────────────────────────────────────────────

// capRecorder is a [nodeIDChunkSource] that records every capacity its
// NewOutputChunk is asked for, so a test can assert what its PARENT requested.
type capRecorder struct {
	*nodeIDChunkSource
	asked []int
}

func (r *capRecorder) NewOutputChunk(capacity int) *exec.Chunk {
	r.asked = append(r.asked, capacity)
	return r.nodeIDChunkSource.NewOutputChunk(capacity)
}

// The child template columnarExpand builds to read the passthrough kinds is
// DISCARDED, so its size is invisible to every assertion about the chunk that is
// returned. This pins it at the child seam instead: the template must be requested
// at capacity 1 — minimal backing — and never at 0, which [exec.NewChunk] and
// [exec.NewDynamicChunk] map to [exec.DefaultChunkCapacity], silently restoring the
// full allocation the capacity-1 template exists to avoid.
func TestExpandColumnar_TemplateRequestedAtCapacityOne(t *testing.T) {
	fwd := buildCSR(3, [][2]int{{0, 1}, {1, 2}})
	rev := buildCSR(3, [][2]int{{1, 0}, {2, 1}})

	rec := &capRecorder{nodeIDChunkSource: &nodeIDChunkSource{ids: []int64{0, 1, 2}}}
	base := exec.NewExpand(rec, exec.StaticAdjacency(fwd, rev, nil),
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0})
	cp, ok := exec.NewColumnarExpand(base)
	if !ok {
		t.Fatalf("NewColumnarExpand: child was not recognised as a ChunkProducer")
	}

	if got := cp.NewOutputChunk(exec.DefaultChunkCapacity).Cap(); got != exec.DefaultChunkCapacity {
		t.Fatalf("returned chunk Cap=%d, want %d", got, exec.DefaultChunkCapacity)
	}
	if len(rec.asked) != 1 {
		t.Fatalf("child NewOutputChunk called %d times, want exactly 1 (the template)", len(rec.asked))
	}
	if rec.asked[0] != 1 {
		t.Errorf("template requested at capacity %d, want 1 (0 or a negative would fall back to %d)",
			rec.asked[0], exec.DefaultChunkCapacity)
	}
}

// columnarExpand.NewOutputChunk builds a THROWAWAY child template purely to read
// the passthrough column kinds, at capacity 1 so it carries minimal backing, and
// then returns the real chunk at the CALLER's capacity. Nothing else asserted that
// separation, so a future edit could size the real, row-carrying chunk from the
// template's capacity and no test would notice: a Chunk's capacity is a pre-sizing
// hint, not a bound, so an under-sized chunk still produces correct rows — it just
// reallocates its backings as it fills.
//
// It also pins the constructor fallback the capacity-1 choice depends on:
// [exec.NewChunk] maps a capacity < 1 to [exec.DefaultChunkCapacity], which is why
// the template is built at 1 and not at 0.
func TestExpandColumnar_OutputChunkCapacityAndSchema(t *testing.T) {
	fwd := buildCSR(5, [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}})
	rev := buildCSR(5, [][2]int{{1, 0}, {2, 0}, {3, 1}, {3, 2}, {4, 3}})
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}

	newCP := func() exec.ChunkProducer {
		base := exec.NewExpand(&nodeIDChunkSource{ids: []int64{0, 1, 2, 3, 4}},
			exec.StaticAdjacency(fwd, rev, nil), cfg)
		cp, ok := exec.NewColumnarExpand(base)
		if !ok {
			t.Fatalf("NewColumnarExpand: child was not recognised as a ChunkProducer")
		}
		return cp
	}

	// The schema is available BEFORE Init — NewColumnarHashJoin relies on exactly
	// that when it takes capacity-1 templates of its children.
	pre := newCP().NewOutputChunk(7)
	if got := pre.NumCols(); got != 4 {
		t.Errorf("pre-Init NewOutputChunk: NumCols=%d, want 4 (1 passthrough + srcID/edgeID/dstID)", got)
	}
	if got := pre.Cap(); got != 7 {
		t.Errorf("pre-Init NewOutputChunk(7): Cap=%d, want 7", got)
	}

	// The caller's capacity reaches the real chunk, for a small value, for the
	// pipeline default, and through the < 1 → DefaultChunkCapacity fallback.
	for _, tc := range []struct{ ask, want int }{
		{1, 1},
		{7, 7},
		{exec.DefaultChunkCapacity, exec.DefaultChunkCapacity},
		{0, exec.DefaultChunkCapacity},
		{-3, exec.DefaultChunkCapacity},
	} {
		if got := newCP().NewOutputChunk(tc.ask).Cap(); got != tc.want {
			t.Errorf("NewOutputChunk(%d): Cap=%d, want %d", tc.ask, got, tc.want)
		}
	}

	// Every column is a typed integer column (the passthrough kind is read through
	// the capacity-1 template, so a wrong template must not box it), and the chunk
	// really carries the traversal's rows.
	cp := newCP()
	if err := cp.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dst := cp.NewOutputChunk(exec.DefaultChunkCapacity)
	for j := 0; j < dst.NumCols(); j++ {
		if got := dst.ColKind(j); got != expr.KindInteger {
			t.Errorf("output col %d: ColKind=%s, want Integer", j, got)
		}
	}
	n, err := cp.FillChunk(dst, exec.DefaultChunkCapacity)
	if err != nil {
		t.Fatalf("FillChunk: %v", err)
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n != 5 || dst.Len() != 5 {
		t.Fatalf("FillChunk: n=%d Len=%d, want 5,5 (one row per fwd edge)", n, dst.Len())
	}

	// Stacked columnar expands: the OUTER template is a capacity-1 template of a
	// 4-column producer that itself builds a capacity-1 template. The nested
	// schema must still widen to 4 + 3 columns.
	inner := exec.NewExpand(&nodeIDChunkSource{ids: []int64{0, 1, 2, 3, 4}},
		exec.StaticAdjacency(fwd, rev, nil), cfg)
	innerCP, ok := exec.NewColumnarExpand(inner)
	if !ok {
		t.Fatalf("inner NewColumnarExpand: not a ChunkProducer")
	}
	outer := exec.NewExpand(innerCP, exec.StaticAdjacency(fwd, rev, nil),
		exec.ExpandConfig{Direction: exec.DirOut, InputCol: 3}) // 3 = inner dstID
	outerCP, ok := exec.NewColumnarExpand(outer)
	if !ok {
		t.Fatalf("outer NewColumnarExpand: not a ChunkProducer")
	}
	stacked := outerCP.NewOutputChunk(9)
	if got := stacked.NumCols(); got != 7 {
		t.Errorf("stacked NewOutputChunk: NumCols=%d, want 7 (4 passthrough + 3)", got)
	}
	if got := stacked.Cap(); got != 9 {
		t.Errorf("stacked NewOutputChunk(9): Cap=%d, want 9", got)
	}
	for j := 0; j < stacked.NumCols(); j++ {
		if got := stacked.ColKind(j); got != expr.KindInteger {
			t.Errorf("stacked col %d: ColKind=%s, want Integer", j, got)
		}
	}
}
