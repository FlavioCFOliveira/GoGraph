package graph

// mapper_appendids_3081_test.go — rmp #3081: Mapper.AppendIDs is a resumable
// form of Mapper.Walk. Layer: short.

import (
	"fmt"
	"testing"
)

// drainAppendIDs collects every id AppendIDs yields from the zero cursor in
// batches of max.
func drainAppendIDs(m *Mapper[string], max int) []NodeID {
	var out []NodeID
	var cur MapperCursor
	for !cur.Done() {
		out, cur = m.AppendIDs(out, cur, max)
	}
	return out
}

func walkIDs(m *Mapper[string]) []NodeID {
	var out []NodeID
	m.Walk(func(id NodeID, _ string) bool { out = append(out, id); return true })
	return out
}

// TestMapper_AppendIDs_MatchesWalk pins the order and the set: every batch size
// yields exactly Walk's sequence, holes included in the skip.
func TestMapper_AppendIDs_MatchesWalk(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	for i := 0; i < 3000; i++ {
		m.Intern(fmt.Sprintf("k%d", i))
	}
	want := walkIDs(m)
	for _, max := range []int{1, 3, 512, 1 << 20} {
		got := drainAppendIDs(m, max)
		if len(got) != len(want) {
			t.Fatalf("max=%d: %d ids, want %d", max, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("max=%d: id[%d]=%d, want %d (order differs from Walk)", max, i, got[i], want[i])
			}
		}
	}
	// A non-positive max appends nothing and does not move the cursor.
	if out, cur := m.AppendIDs(nil, MapperCursor{}, 0); len(out) != 0 || cur != (MapperCursor{}) {
		t.Fatalf("max=0 appended %d ids / moved the cursor to %+v", len(out), cur)
	}
}

// TestMapper_AppendIDs_SkipsHoles restores a shard with holes and checks that
// AppendIDs skips them exactly as Walk does.
func TestMapper_AppendIDs_SkipsHoles(t *testing.T) {
	t.Parallel()
	shard, keys := sameShardStrings(t, 3)
	m := NewMapper[string]()
	a, b := packNodeID(shard, 0), packNodeID(shard, 3)
	if err := m.LoadFrom([]MapperEntry[string]{{ID: b, Key: keys[1]}, {ID: a, Key: keys[0]}}, nil); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	for _, max := range []int{1, 2, 64} {
		got := drainAppendIDs(m, max)
		if len(got) != 2 || got[0] != a || got[1] != b {
			t.Fatalf("max=%d: AppendIDs = %v, want [%d %d]", max, got, a, b)
		}
	}
}
