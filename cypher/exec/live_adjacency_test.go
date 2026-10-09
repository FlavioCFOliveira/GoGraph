package exec

import (
	"context"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// live_adjacency_test.go — the operator half of rmp #2883's live forward runs:
// what Expand and VarLengthExpand do with a liveOutAdjacency, and above all the
// switch to a whole-graph adjacency for a relationship without a stable handle,
// which no graph built since rmp #2317 produces and a restored legacy one can.

// fakeLive is a liveOutAdjacency over fixed per-source runs, with a whole-graph
// fallback. It records the calls an operator makes.
type fakeLive struct {
	runs      map[graph.NodeID]fakeLiveRun
	fallback  CSRAdjacency
	begins    int
	fallbacks int
}

type fakeLiveRun struct {
	dsts      []graph.NodeID
	handles   []uint64
	handleCol bool
}

func (f *fakeLive) VerticesSlice() []uint64    { return nil }
func (f *fakeLive) EdgesSlice() []graph.NodeID { return nil }
func (f *fakeLive) HandlesSlice() []uint64     { return nil }

func (f *fakeLive) LiveBegin() (uint64, []uint32) {
	f.begins++
	return 0, nil
}

func (f *fakeLive) LiveOutRun(
	src graph.NodeID, _ uint64, dsts []graph.NodeID, handles []uint64, codes []uint32,
) ([]graph.NodeID, []uint64, []uint32, map[uint64][]uint32, bool) {
	r := f.runs[src]
	dsts = append(dsts, r.dsts...)
	handles = append(handles, r.handles...)
	codes = append(codes, make([]uint32, len(r.dsts))...)
	return dsts, handles, codes, nil, r.handleCol
}

func (f *fakeLive) LiveFallback(uint64) (CSRAdjacency, RelTypeAdmit) {
	f.fallbacks++
	return f.fallback, RelTypeAdmit{}
}

// liveRowsInput emits one row per source id.
type liveRowsInput struct {
	ids []int64
	i   int
}

func (o *liveRowsInput) Init(context.Context) error { o.i = 0; return nil }
func (o *liveRowsInput) Close() error               { return nil }
func (o *liveRowsInput) Next(out *Row) (bool, error) {
	if o.i >= len(o.ids) {
		return false, nil
	}
	*out = Row{expr.IntegerValue(o.ids[o.i])}
	o.i++
	return true, nil
}

func liveDrainTriplets(t *testing.T, op Operator) [][3]int64 {
	t.Helper()
	rows := drain(t, op)
	out := make([][3]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, [3]int64{
			int64(r[1].(expr.IntegerValue)), int64(r[2].(expr.IntegerValue)), int64(r[3].(expr.IntegerValue)),
		})
	}
	return out
}

// TestExpandLive_ServesRunsAndIdentifiesByHandle: runs with a handle column are
// served as they are, and each row's relationship identity is the slot's handle.
func TestExpandLive_ServesRunsAndIdentifiesByHandle(t *testing.T) {
	src := &fakeLive{runs: map[graph.NodeID]fakeLiveRun{
		1: {dsts: []graph.NodeID{2, 3}, handles: []uint64{11, 12}, handleCol: true},
		2: {dsts: []graph.NodeID{3}, handles: []uint64{21}, handleCol: true},
	}}
	op := NewExpand(&liveRowsInput{ids: []int64{1, 2, 4}},
		func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) { return src, nil, RelTypeAdmit{} },
		ExpandConfig{Direction: DirOut})
	got := liveDrainTriplets(t, op)
	want := [][3]int64{{1, 11, 2}, {1, 12, 3}, {2, 21, 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if src.begins != 1 || src.fallbacks != 0 {
		t.Fatalf("begins=%d fallbacks=%d, want 1 and 0", src.begins, src.fallbacks)
	}
}

// TestExpandLive_HandlelessBeforeAnyHandleSwitchesToWholeGraph: the first
// handle-less run of an Init that has seen no handle column switches the
// operator to the whole-graph adjacency, which then answers for that source and
// every later one — positions and all — exactly as it would have alone.
func TestExpandLive_HandlelessBeforeAnyHandleSwitchesToWholeGraph(t *testing.T) {
	whole := &fakeCSR{
		verts: []uint64{0, 0, 2, 3, 3, 3},
		edges: []graph.NodeID{2, 3, 3},
	}
	src := &fakeLive{
		runs: map[graph.NodeID]fakeLiveRun{
			1: {dsts: []graph.NodeID{2, 3}},
			2: {dsts: []graph.NodeID{3}},
		},
		fallback: whole,
	}
	op := NewExpand(&liveRowsInput{ids: []int64{1, 2}},
		func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) { return src, nil, RelTypeAdmit{} },
		ExpandConfig{Direction: DirOut})
	got := liveDrainTriplets(t, op)
	control := NewExpand(&liveRowsInput{ids: []int64{1, 2}},
		func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) { return whole, nil, RelTypeAdmit{} },
		ExpandConfig{Direction: DirOut})
	want := liveDrainTriplets(t, control)
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want the whole-graph answer %v", got, want)
	}
	if src.fallbacks != 1 {
		t.Fatalf("fallbacks = %d, want 1", src.fallbacks)
	}
}

// TestExpandLive_HandlelessAfterAHandleIsIdentityZero: once the Init has read a
// run with a handle column, the whole-graph build at the same instant would have
// a handle column in which a handle-less slot holds 0, so the live path serves
// the run with identity 0 and does not switch.
func TestExpandLive_HandlelessAfterAHandleIsIdentityZero(t *testing.T) {
	src := &fakeLive{runs: map[graph.NodeID]fakeLiveRun{
		1: {dsts: []graph.NodeID{2}, handles: []uint64{11}, handleCol: true},
		2: {dsts: []graph.NodeID{3, 4}},
	}}
	op := NewExpand(&liveRowsInput{ids: []int64{1, 2}},
		func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) { return src, nil, RelTypeAdmit{} },
		ExpandConfig{Direction: DirOut})
	got := liveDrainTriplets(t, op)
	want := [][3]int64{{1, 11, 2}, {2, 0, 3}, {2, 0, 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if src.fallbacks != 0 {
		t.Fatalf("fallbacks = %d, want 0", src.fallbacks)
	}
}

// TestExpandLive_NotUsedForIncoming: a live adjacency handed to an incoming hop is
// not read per source; the operator treats it as the (empty) whole-graph
// adjacency it describes, which is why the planner never hands one to such a hop.
func TestExpandLive_NotUsedForIncoming(t *testing.T) {
	src := &fakeLive{runs: map[graph.NodeID]fakeLiveRun{
		1: {dsts: []graph.NodeID{2}, handles: []uint64{11}, handleCol: true},
	}}
	op := NewExpand(&liveRowsInput{ids: []int64{1}},
		func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) { return src, src, RelTypeAdmit{} },
		ExpandConfig{Direction: DirIn})
	if got := liveDrainTriplets(t, op); len(got) != 0 || src.begins != 0 {
		t.Fatalf("an incoming hop read the live adjacency: rows=%v begins=%d", got, src.begins)
	}
}

// TestVarLengthExpandLive_MatchesWholeGraph: a forward variable-length expansion
// over live runs finds the same paths, with the same identities, as over the
// whole-graph adjacency the runs describe.
func TestVarLengthExpandLive_MatchesWholeGraph(t *testing.T) {
	whole := &fakeCSR{
		verts:   []uint64{0, 0, 2, 3, 4, 4},
		edges:   []graph.NodeID{2, 3, 3, 1},
		handles: []uint64{11, 12, 21, 31},
	}
	src := &fakeLive{runs: map[graph.NodeID]fakeLiveRun{
		1: {dsts: []graph.NodeID{2, 3}, handles: []uint64{11, 12}, handleCol: true},
		2: {dsts: []graph.NodeID{3}, handles: []uint64{21}, handleCol: true},
		3: {dsts: []graph.NodeID{1}, handles: []uint64{31}, handleCol: true},
	}}
	run := func(adj CSRAdjacency) []string {
		op := NewVarLengthExpand(&liveRowsInput{ids: []int64{1}},
			func() (CSRAdjacency, CSRAdjacency, RelTypeAdmit) { return adj, nil, RelTypeAdmit{} },
			&VarLengthConfig{Direction: DirOut, MinHops: 1, MaxHops: 4})
		rows := drain(t, op)
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r[1].String()+"->"+r[2].String())
		}
		return out
	}
	got, want := run(src), run(whole)
	if !slices.Equal(got, want) || len(got) == 0 {
		t.Fatalf("live paths %v, whole-graph paths %v", got, want)
	}
	if src.begins != 1 {
		t.Fatalf("begins = %d, want 1", src.begins)
	}
}
