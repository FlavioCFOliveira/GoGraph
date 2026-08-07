package exec_test

// expand_columnar_filter_test.go — differential test + benchmark for a
// ColumnarFilter placed over an Expand ChunkProducer (rmp #2106): the
// "WHERE p.x > k" post-traversal shape. The filter reads the far node's raw NodeID
// from the Expand output's dstID column and compares a property against a constant
// WITHOUT boxing.
//
//   - ON  : scan-source → columnarExpand → ColumnarFilter, drained column-major.
//   - OFF : scan-source → Expand → Filter, drained row-at-a-time.
//
// Both must yield the same surviving-row multiset. The property is modelled by a
// dense []int64 keyed by NodeID (props[dstID]) so the benchmark isolates the
// operator-chain de-box win rather than graph property-store cost; the predicate
// logic is identical between the unboxed ChunkPredicate and the boxed FilterFn.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// farNodeProps builds the two predicate closures for `props[dstID] > k`, keyed by
// the dstID column at chunk/row column dstCol. Both are equivalent by construction.
func farNodeProps(props []int64, dstCol int, k int64) (exec.ChunkPredicate, exec.FilterFn) {
	chunkPred := func(src *exec.Chunk, row int) (keep, decided bool) {
		if !src.IsInt64Column(dstCol) {
			return false, false
		}
		id, valid := src.Int64(dstCol, row)
		if !valid || id < 0 || int(id) >= len(props) {
			return false, false
		}
		return props[id] > k, true
	}
	boxedFn := func(row exec.Row) (expr.Value, error) {
		iv, ok := row[dstCol].(expr.IntegerValue)
		if !ok {
			return expr.Null, nil
		}
		id := int64(iv)
		if id < 0 || int(id) >= len(props) {
			return expr.Null, nil
		}
		return expr.BoolValue(props[id] > k), nil
	}
	return chunkPred, boxedFn
}

// buildFanoutGraph makes a graph where every source in [0,n) has out-degree deg,
// plus a per-node property slice props[id] used by the WHERE predicate.
func buildFanoutGraph(n, deg int) (fwd, rev *staticCSR, ids, props []int64) {
	maxNode := n + deg + 1
	var edges [][2]int
	for src := 0; src < n; src++ {
		for k := 0; k < deg; k++ {
			edges = append(edges, [2]int{src, (src*deg + k + 1) % maxNode})
		}
	}
	fwd = buildCSR(maxNode, edges)
	rev = buildCSR(maxNode, nil)
	ids = make([]int64, n)
	for i := range ids {
		ids[i] = int64(i)
	}
	props = make([]int64, maxNode+1)
	for i := range props {
		props[i] = int64((i * 7) % 100)
	}
	return fwd, rev, ids, props
}

func TestExpandColumnarFilter_WherePostTraversal(t *testing.T) {
	fwd, rev, ids, props := buildFanoutGraph(40, 6)
	const dstCol = 3 // [n, srcID, edgeID, dstID] — the far node p is dstID at col 3
	const k = 50
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}

	// OFF: row-mode Expand → Filter.
	_, boxedFn := farNodeProps(props, dstCol, k)
	rowExpand := exec.NewExpand(&nodeIDChunkSource{ids: ids}, exec.StaticAdjacency(fwd, rev, nil), cfg)
	rowFilter := exec.NewFilter(rowExpand, boxedFn)
	wantRows, err := exec.Drain(context.Background(), rowFilter)
	if err != nil {
		t.Fatalf("row-mode Drain: %v", err)
	}
	want := sortedRowKeys(wantRows)

	// ON: columnarExpand → ColumnarFilter, drained column-major, over several
	// per-call caps and child batch sizes.
	for _, perCall := range []int{1, 5, 64, exec.DefaultChunkCapacity} {
		for _, batch := range []int{0, 1, 3} {
			chunkPred, boxed := farNodeProps(props, dstCol, k)
			base := exec.NewExpand(&nodeIDChunkSource{ids: ids, batch: batch}, exec.StaticAdjacency(fwd, rev, nil), cfg)
			cp, ok := exec.NewColumnarExpand(base)
			if !ok {
				t.Fatalf("NewColumnarExpand: not a ChunkProducer")
			}
			cf := exec.NewColumnarFilter(cp, boxed, chunkPred)
			if err := cf.Init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			dst := cf.NewOutputChunk(exec.DefaultChunkCapacity)
			for {
				before := dst.Len()
				n, ferr := cf.FillChunk(dst, perCall)
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
			if len(got) != len(want) {
				t.Fatalf("perCall=%d batch=%d: got %d survivors, want %d", perCall, batch, len(got), len(want))
			}
			if i, eq := equalKeys(want, got); !eq {
				t.Fatalf("perCall=%d batch=%d: survivor multiset mismatch at %d:\n want %q\n  got %q",
					perCall, batch, i, want[i], got[i])
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmark: WHERE p.x > k post-traversal over a high-degree graph.
// ─────────────────────────────────────────────────────────────────────────────

// benchFanout is sized once and reused so both sub-benchmarks measure the same work.
var (
	benchFwd, benchRev *staticCSR
	benchIDs           []int64
	benchProps         []int64
)

func benchInit() {
	if benchFwd == nil {
		benchFwd, benchRev, benchIDs, benchProps = buildFanoutGraph(2000, 32) // 64k edges
	}
}

// BenchmarkExpandFilter_Columnar drives scan → columnarExpand → ColumnarFilter
// column-major (the ON path): the far-node property read for every emitted edge is
// evaluated unboxed over the dstID int64 column.
func BenchmarkExpandFilter_Columnar(b *testing.B) {
	benchInit()
	const dstCol, k = 3, 50
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunkPred, boxed := farNodeProps(benchProps, dstCol, k)
		base := exec.NewExpand(&nodeIDChunkSource{ids: benchIDs}, exec.StaticAdjacency(benchFwd, benchRev, nil), cfg)
		cp, _ := exec.NewColumnarExpand(base)
		cf := exec.NewColumnarFilter(cp, boxed, chunkPred)
		if err := cf.Init(context.Background()); err != nil {
			b.Fatalf("Init: %v", err)
		}
		dst := cf.NewOutputChunk(exec.DefaultChunkCapacity)
		total := 0
		for {
			n, err := cf.FillChunk(dst, exec.DefaultChunkCapacity)
			if err != nil {
				b.Fatalf("FillChunk: %v", err)
			}
			total += n
			dst.Reset()
			if n < exec.DefaultChunkCapacity {
				break
			}
		}
		if err := cf.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		_ = total
	}
}

// BenchmarkExpandFilter_RowMode drives scan → Expand → Filter row-at-a-time (the
// OFF path): every emitted row is boxed into a Row and the property read boxes the
// far node's id per row.
func BenchmarkExpandFilter_RowMode(b *testing.B) {
	benchInit()
	const dstCol, k = 3, 50
	cfg := exec.ExpandConfig{Direction: exec.DirOut, InputCol: 0}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, boxed := farNodeProps(benchProps, dstCol, k)
		exp := exec.NewExpand(&nodeIDChunkSource{ids: benchIDs}, exec.StaticAdjacency(benchFwd, benchRev, nil), cfg)
		f := exec.NewFilter(exp, boxed)
		if err := f.Init(context.Background()); err != nil {
			b.Fatalf("Init: %v", err)
		}
		var row exec.Row
		total := 0
		for {
			ok, err := f.Next(&row)
			if err != nil {
				b.Fatalf("Next: %v", err)
			}
			if !ok {
				break
			}
			total++
		}
		if err := f.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		_ = total
	}
}
