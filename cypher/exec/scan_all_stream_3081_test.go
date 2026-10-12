package exec_test

// scan_all_stream_3081_test.go — rmp #3081: the streamed AllNodesScan path, driven
// by a stub streamer over a real graph.Mapper. Layer: short.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// mapperStreamer streams a Mapper's ids, hiding the ones in dead. WalkNodeIDs is
// its materialising twin and panics if the scan ever falls back to it.
type mapperStreamer struct {
	m      *graph.Mapper[string]
	dead   map[graph.NodeID]bool
	walked bool
}

func (s *mapperStreamer) WalkNodeIDs(func(graph.NodeID) bool) { s.walked = true }
func (s *mapperStreamer) StreamsNodeIDs() bool                { return true }
func (s *mapperStreamer) NodeIDUpperBound() int               { return s.m.Len() }
func (s *mapperStreamer) NextNodeIDs(cur *graph.MapperCursor, dst []graph.NodeID) []graph.NodeID {
	dst = dst[:0]
	for len(dst) == 0 && !cur.Done() {
		dst, *cur = s.m.AppendIDs(dst, *cur, cap(dst))
		k := 0
		for _, id := range dst {
			if !s.dead[id] {
				dst[k] = id
				k++
			}
		}
		dst = dst[:k]
	}
	return dst
}

func newMapperStreamer(n int) (*mapperStreamer, []graph.NodeID) {
	s := &mapperStreamer{m: graph.NewMapper[string](), dead: map[graph.NodeID]bool{}}
	for i := 0; i < n; i++ {
		s.m.Intern(fmt.Sprintf("k%d", i))
	}
	var want []graph.NodeID
	i := 0
	s.m.Walk(func(id graph.NodeID, _ string) bool {
		if i%5 == 0 {
			s.dead[id] = true
		} else {
			want = append(want, id)
		}
		i++
		return true
	})
	return s, want
}

func TestAllNodesScan_Streamed_NextFillChunkReinitClose_3081(t *testing.T) {
	t.Parallel()
	s, want := newMapperStreamer(5000)
	ctx := context.Background()
	op := exec.NewAllNodesScan(s)

	// Row drain: exact ids, exact order. Run performs the Init.
	rs := exec.Run(ctx, op, []string{"n"})
	if s.walked {
		t.Fatal("Init fell back to WalkNodeIDs on a streaming walker")
	}
	if n, ok := rs.RowCountHint(); !ok || n < len(want) {
		t.Fatalf("rowCountHint = %d,%v; want an upper bound >= %d", n, ok, len(want))
	}
	var got []graph.NodeID
	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, graph.NodeID(row[0].(expr.IntegerValue)))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("streamed Next drain: %d ids, want %d (or order differs)", len(got), len(want))
	}
	if ok, _ := op.Next(&row); ok {
		t.Fatal("Next after exhaustion returned a row")
	}

	// Re-Init restarts the stream; FillChunk yields the same ids column-major.
	if err := op.Init(ctx); err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	ch := op.NewOutputChunk(700)
	for {
		ch.Reset()
		n, err := op.FillChunk(ch, 700)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		for r := 0; r < n; r++ {
			v, _ := ch.Int64(0, r)
			got = append(got, graph.NodeID(v))
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("streamed FillChunk drain: %d ids, want %d (or order differs)", len(got), len(want))
	}

	// Close mid-stream exhausts the operator.
	if err := op.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := op.Next(&row); !ok {
		t.Fatal("no first row after re-Init")
	}
	_ = op.Close()
	if ok, _ := op.Next(&row); ok {
		t.Fatal("Next after Close returned a row")
	}

	// A cancelled context is honoured by Init and Next.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := op.Init(cctx); err == nil {
		t.Fatal("Init with a cancelled context returned nil")
	}
}
