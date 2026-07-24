package exec

// hash_join_columnar_test.go — differential coverage for ColumnarHashJoin
// (#2105, design docs/columnar-deepening-design.md §5).
//
// Every case drives the SAME inputs and key functions through three drains and
// asserts a byte-identical sorted result multiset:
//
//   - the shipped row-mode HashJoin (the reference — "OFF"),
//   - ColumnarHashJoin via its row-mode Next fallback,
//   - ColumnarHashJoin via its column-major FillChunk output (boxed at the sink).
//
// The three must agree for every case, proving the columnar operator is
// equivalent by construction to the operator it replaces on BOTH output paths
// (reversibility contract, design §6). The build/probe children are a dual-mode
// source that emits the same rows row-at-a-time (Next) and column-major
// (FillChunk, dynamic columns committing exactly as the engine's chunks do), so
// the ChunkProducer path is exercised end-to-end.

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// dualSource emits a fixed row sequence either row-at-a-time (Next) or
// column-major (FillChunk into a dynamic chunk via PutValue). It is the
// two-faced ChunkProducer the differential drains share, so an int-only key
// column commits to int64, a float-only column to float64, a mixed column
// promotes to boxed, and a NULL records validity — the storage shapes the
// operator must all handle.
type dualSource struct {
	ctx   context.Context //nolint:containedctx // test stub
	rows  []Row
	ncols int
	idx   int
}

func (s *dualSource) Init(ctx context.Context) error { s.ctx = ctx; s.idx = 0; return nil }
func (s *dualSource) Close() error                   { return nil }

func (s *dualSource) Next(out *Row) (bool, error) {
	if err := s.ctx.Err(); err != nil {
		return false, err
	}
	if s.idx >= len(s.rows) {
		return false, nil
	}
	r := make(Row, s.ncols)
	copy(r, s.rows[s.idx])
	s.idx++
	*out = r
	return true, nil
}

func (s *dualSource) NewOutputChunk(capacity int) *Chunk {
	return NewDynamicChunk(capacity, s.ncols)
}

func (s *dualSource) FillChunk(dst *Chunk, maxRows int) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n := 0
	for n < maxRows && s.idx < len(s.rows) {
		for c := 0; c < s.ncols; c++ {
			dst.PutValue(c, s.rows[s.idx][c])
		}
		s.idx++
		n++
	}
	return n, nil
}

// nodeIDColumnProducer marks dualSource as a NodeIDColumnProducer (the scan-row
// shape the planner requires on both hash-join arms), so NewColumnarHashJoin
// accepts it. The test's key columns carry plain scalars, exercising the same
// unboxed cell copy the real NodeID columns take.
func (s *dualSource) nodeIDColumnProducer() {}

// chunkOnlySource is a ChunkProducer that is deliberately NOT a
// NodeIDColumnProducer (no marker method), proving NewColumnarHashJoin falls back
// to the row-mode HashJoin for such a child. Its bodies are trivial: the gate
// rejects it at the constructor's type assertion, before any method runs.
type chunkOnlySource struct{}

func (chunkOnlySource) Init(context.Context) error         { return nil }
func (chunkOnlySource) Next(*Row) (bool, error)            { return false, nil }
func (chunkOnlySource) Close() error                       { return nil }
func (chunkOnlySource) NewOutputChunk(int) *Chunk          { return NewDynamicChunk(1, 1) }
func (chunkOnlySource) FillChunk(*Chunk, int) (int, error) { return 0, nil }

// rowString renders a boxed row for stable multiset comparison.
func rowString(r Row) string {
	s := ""
	for i, v := range r {
		if i > 0 {
			s += ","
		}
		if v == nil {
			s += "<nil>"
			continue
		}
		s += v.String()
	}
	return s
}

// drainColumnarNext drives ColumnarHashJoin through its row-mode Next fallback.
func drainColumnarNext(t *testing.T, chj *ColumnarHashJoin) []string {
	t.Helper()
	if err := chj.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		var r Row
		ok, err := chj.Next(&r)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		out = append(out, rowString(r))
	}
	if err := chj.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// drainColumnarFill drives ColumnarHashJoin through its column-major FillChunk
// output, boxing each produced row at the (test) sink. A small batch size forces
// the multi-batch / partially-consumed-input path.
func drainColumnarFill(t *testing.T, chj *ColumnarHashJoin) []string {
	t.Helper()
	if err := chj.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	const batch = 3
	dst := chj.NewOutputChunk(batch)
	var out []string
	for {
		dst.Reset()
		n, err := chj.FillChunk(dst, batch)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		var scratch Row
		for i := 0; i < n; i++ {
			scratch = dst.BoxRow(i, scratch)
			out = append(out, rowString(scratch))
		}
	}
	if err := chj.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// hjCase is one differential scenario: build/probe rows, the key column on each
// side, and the number of columns each source emits.
type hjCase struct {
	name                 string
	buildRows            []Row
	probeRows            []Row
	buildKey, probeKey   int
	buildCols, probeCols int
}

// assertColumnarMatchesRow runs the scenario under the shipped row-mode HashJoin
// and both ColumnarHashJoin output paths, for buildOnLeft both false and true,
// and asserts all three drains agree on the sorted multiset.
func assertColumnarMatchesRow(t *testing.T, c *hjCase) {
	t.Helper()
	for _, buildOnLeft := range []bool{false, true} {
		buildOnLeft := buildOnLeft
		name := "buildOnRight"
		if buildOnLeft {
			name = "buildOnLeft"
		}
		t.Run(c.name+"/"+name, func(t *testing.T) {
			mkBuild := func() *dualSource {
				return &dualSource{rows: cloneRows(c.buildRows), ncols: c.buildCols}
			}
			mkProbe := func() *dualSource {
				return &dualSource{rows: cloneRows(c.probeRows), ncols: c.probeCols}
			}

			ref := drainJoin(t, NewHashJoin(mkBuild(), mkProbe(), keyCol(c.buildKey), keyCol(c.probeKey), buildOnLeft))

			chjNext, ok := NewColumnarHashJoin(mkBuild(), mkProbe(), keyCol(c.buildKey), keyCol(c.probeKey), buildOnLeft)
			if !ok {
				t.Fatal("NewColumnarHashJoin returned !ok for two ChunkProducer children")
			}
			gotNext := drainColumnarNext(t, chjNext)

			chjFill, ok := NewColumnarHashJoin(mkBuild(), mkProbe(), keyCol(c.buildKey), keyCol(c.probeKey), buildOnLeft)
			if !ok {
				t.Fatal("NewColumnarHashJoin returned !ok for two ChunkProducer children")
			}
			gotFill := drainColumnarFill(t, chjFill)

			assertSameMultiset(t, "columnar-Next vs row-HashJoin", gotNext, ref)
			assertSameMultiset(t, "columnar-FillChunk vs row-HashJoin", gotFill, ref)
		})
	}
}

func cloneRows(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		cp := make(Row, len(r))
		copy(cp, r)
		out[i] = cp
	}
	return out
}

func assertSameMultiset(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: row-count mismatch got %d want %d\n got=%v\nwant=%v", label, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d differs\n got=%q\nwant=%q", label, i, got[i], want[i])
		}
	}
}

func TestColumnarHashJoin_Differential(t *testing.T) {
	const twoTo53 = int64(1) << 53
	cases := []hjCase{
		{
			name: "MultiRowMatches",
			// keys mod 4; multiple rows per key on both sides + payload columns to
			// observe column order.
			buildRows: mkKeyPayload(24, 4, "B"),
			probeRows: mkKeyPayload(20, 4, "P"),
			buildKey:  0, probeKey: 0, buildCols: 2, probeCols: 2,
		},
		{
			name:      "NullKeysExcluded",
			buildRows: []Row{{iv(1)}, {expr.Null}, {iv(2)}, {expr.Null}, {iv(2)}},
			probeRows: []Row{{iv(1)}, {iv(2)}, {expr.Null}, {iv(3)}},
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name:      "NaNKeysExcluded",
			buildRows: []Row{{fv(math.NaN())}, {fv(1.0)}, {fv(math.NaN())}, {fv(2.0)}},
			probeRows: []Row{{fv(math.NaN())}, {fv(1.0)}, {fv(2.0)}, {fv(2.0)}},
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name: "CrossTypeIntFloatEqualMatch",
			// int 1 must join float 1.0 (openCypher numeric equality).
			buildRows: []Row{{iv(0)}, {iv(1)}, {iv(2)}, {iv(3)}},
			probeRows: []Row{{fv(0.0)}, {fv(1.0)}, {fv(2.0)}, {fv(9.0)}},
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name: "CrossTypeLargeIntFloatNoMatch",
			// int 2^53+1 shares a hash bucket with float 2^53.0 but is a DISTINCT
			// number — must NOT match. int 2^53 vs float 2^53.0 DOES match.
			buildRows: []Row{{iv(twoTo53 + 1)}, {iv(twoTo53)}},
			probeRows: []Row{{fv(float64(twoTo53))}},
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name:      "StringNeverMatchesNumber",
			buildRows: []Row{{sv("1")}, {sv("2")}},
			probeRows: []Row{{iv(1)}, {iv(2)}},
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name:      "EmptyBuild",
			buildRows: nil,
			probeRows: []Row{{iv(1)}, {iv(2)}},
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name:      "EmptyProbe",
			buildRows: []Row{{iv(1)}, {iv(2)}},
			probeRows: nil,
			buildKey:  0, probeKey: 0, buildCols: 1, probeCols: 1,
		},
		{
			name:      "KeyNotFirstColumn",
			buildRows: []Row{{sv("bx"), iv(5)}, {sv("by"), iv(6)}, {sv("bz"), iv(5)}},
			probeRows: []Row{{iv(5), sv("px")}, {iv(6), sv("py")}, {iv(7), sv("pz")}},
			buildKey:  1, probeKey: 0, buildCols: 2, probeCols: 2,
		},
	}
	for i := range cases {
		assertColumnarMatchesRow(t, &cases[i])
	}
}

// mkKeyPayload builds n rows of {key=i%mod, payload=tag+i} to give multi-row
// buckets and a distinct payload column per row for order verification.
func mkKeyPayload(n, mod int, tag string) []Row {
	rows := make([]Row, n)
	for i := 0; i < n; i++ {
		rows[i] = Row{iv(int64(i % mod)), sv(tag + string(rune('A'+i%26)))}
	}
	return rows
}

// TestColumnarHashJoin_ResidualPredicate wraps both the shipped and the columnar
// join in an identical residual Filter (the composition the planner builds for a
// non-key conjunct) and asserts the filtered multiset is identical.
func TestColumnarHashJoin_ResidualPredicate(t *testing.T) {
	buildRows := mkKeyPayload(24, 4, "B")
	probeRows := mkKeyPayload(20, 4, "P")
	// Residual: keep only rows whose join key (output column 0 under buildOnLeft
	// false = probe key column) is > 1.
	residual := func(row Row) (expr.Value, error) {
		k, ok := row[0].(expr.IntegerValue)
		return expr.BoolValue(ok && int64(k) > 1), nil
	}
	mkBuild := func() *dualSource { return &dualSource{rows: cloneRows(buildRows), ncols: 2} }
	mkProbe := func() *dualSource { return &dualSource{rows: cloneRows(probeRows), ncols: 2} }

	ref := drainJoinOp(t, wrapFilter(NewHashJoin(mkBuild(), mkProbe(), keyCol(0), keyCol(0), false), residual))

	chj, ok := NewColumnarHashJoin(mkBuild(), mkProbe(), keyCol(0), keyCol(0), false)
	if !ok {
		t.Fatal("NewColumnarHashJoin !ok")
	}
	got := drainJoinOp(t, wrapFilter(chj, residual))

	assertSameMultiset(t, "columnar+residual vs row+residual", got, ref)
}

// wrapFilter composes op with a residual Filter, mirroring buildResidualFilter.
func wrapFilter(op Operator, fn FilterFn) Operator { return NewFilter(op, fn) }

// drainJoinOp drains any Operator to a sorted multiset of rendered rows.
func drainJoinOp(t *testing.T, op Operator) []string {
	t.Helper()
	if err := op.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		var r Row
		ok, err := op.Next(&r)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		out = append(out, rowString(r))
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestColumnarHashJoin_ByteBudgetTrips confirms the retained-build byte budget
// trips with the same sentinel the row-mode join uses.
func TestColumnarHashJoin_ByteBudgetTrips(t *testing.T) {
	buildRows := []Row{{iv(1)}, {iv(2)}, {iv(3)}, {iv(4)}}
	probeRows := []Row{{iv(1)}}
	// A tiny budget with a fixed 100-byte-per-row estimate trips after the first
	// retained build row.
	est := func(_ Row) int64 { return 100 }
	chj, ok := NewColumnarHashJoin(
		&dualSource{rows: cloneRows(buildRows), ncols: 1},
		&dualSource{rows: cloneRows(probeRows), ncols: 1},
		keyCol(0), keyCol(0), false)
	if !ok {
		t.Fatal("NewColumnarHashJoin !ok")
	}
	chj.WithByteBudget(50, est)
	if err := chj.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r Row
	_, err := chj.Next(&r)
	if !errors.Is(err, ErrHashJoinMemoryExceeded) {
		t.Fatalf("expected ErrHashJoinMemoryExceeded, got %v", err)
	}
	_ = chj.Close()
}

// TestColumnarHashJoin_Cancellation confirms ctx cancellation surfaces from Next.
func TestColumnarHashJoin_Cancellation(t *testing.T) {
	chj, ok := NewColumnarHashJoin(
		&dualSource{rows: []Row{{iv(1)}}, ncols: 1},
		&dualSource{rows: []Row{{iv(1)}}, ncols: 1},
		keyCol(0), keyCol(0), false)
	if !ok {
		t.Fatal("NewColumnarHashJoin !ok")
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := chj.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	var r Row
	if _, err := chj.Next(&r); err == nil {
		t.Fatal("expected cancellation error from Next after cancel")
	}
	_ = chj.Close()
}

// TestColumnarHashJoin_NonChunkChildFallsBack confirms the wrapper is NOT built
// when a child is not a ChunkProducer (§6.2), so existing plans keep HashJoin.
func TestColumnarHashJoin_NonChunkChildFallsBack(t *testing.T) {
	// sliceSource (hash_join_test.go) is a plain Operator, not a ChunkProducer.
	rowOnly := &sliceSource{rows: []Row{{iv(1)}}}
	nodeID := &dualSource{rows: []Row{{iv(1)}}, ncols: 1}
	if _, ok := NewColumnarHashJoin(rowOnly, nodeID, keyCol(0), keyCol(0), false); ok {
		t.Fatal("expected !ok when build child is not a ChunkProducer")
	}
	if _, ok := NewColumnarHashJoin(nodeID, rowOnly, keyCol(0), keyCol(0), false); ok {
		t.Fatal("expected !ok when probe child is not a ChunkProducer")
	}
	// A ChunkProducer that is NOT a NodeIDColumnProducer must also fall back, so a
	// node-property projection above the join is never left re-boxing (#2105).
	chunkOnly := chunkOnlySource{}
	if _, ok := NewColumnarHashJoin(chunkOnly, nodeID, keyCol(0), keyCol(0), false); ok {
		t.Fatal("expected !ok when build child is a ChunkProducer but not a NodeIDColumnProducer")
	}
	if _, ok := NewColumnarHashJoin(nodeID, chunkOnly, keyCol(0), keyCol(0), false); ok {
		t.Fatal("expected !ok when probe child is a ChunkProducer but not a NodeIDColumnProducer")
	}
}
