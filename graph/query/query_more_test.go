package query

import (
	"sort"
	"testing"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
)

// TestQuery_NilPattern_NodeIDs verifies the public surface tolerates
// an unmaterialised working set: NodeIDs on a fresh, never-narrowed
// Pattern emits no NodeIDs and never panics.
func TestQuery_NilPattern_NodeIDs(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	p := e.Match() // no Vertex / Out — p.bm is nil

	count := 0
	for range p.NodeIDs() {
		count++
	}
	if count != 0 {
		t.Fatalf("nil-pattern NodeIDs emitted %d ids, want 0", count)
	}
}

// TestQuery_NilPattern_Cardinality covers the early-return branch of
// Cardinality when the working set has never been materialised.
func TestQuery_NilPattern_Cardinality(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	if got := e.Match().Cardinality(); got != 0 {
		t.Fatalf("Cardinality on un-seeded Match = %d, want 0", got)
	}
}

// TestQuery_NilPattern_Collect covers the early-return branch of
// Collect when the working set has never been materialised.
func TestQuery_NilPattern_Collect(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	if got := e.Match().Collect(); got != nil {
		t.Fatalf("Collect on un-seeded Match = %v, want nil", got)
	}
}

// TestQuery_Out_NilPattern triggers the early-return branch of Out
// when the working set has never been materialised.
func TestQuery_Out_NilPattern(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	p := e.Match().Out()
	if got := p.Cardinality(); got != 0 {
		t.Fatalf("Out on un-seeded Match: Cardinality = %d, want 0", got)
	}
}

// TestQuery_SecondVertex_AppliesWithLabel triggers the
// "withLabel.Match on a non-seed Vertex" path. The first Vertex seeds
// with one label; the second Vertex applies another WithLabel using
// the per-node filter (skipLabel=false) — exercising withLabel.Match
// on every NodeID in the working set.
func TestQuery_SecondVertex_AppliesWithLabel(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	e := New(g, c)
	got := e.Match().
		Vertex(WithLabel[string, int64]("Person")).
		Vertex(WithLabel[string, int64]("Admin")).
		Collect()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "alice" || got[1] != "dave" {
		t.Fatalf("second-Vertex WithLabel filter: got %v, want [alice dave]", got)
	}
}

// TestQuery_SecondVertex_WithLabel_UnknownLabel exercises the
// withLabel.Match fast-fail when the label is not registered on the
// candidate's bag (here: a label that exists in the registry but is
// not attached to any of the Person-seeded nodes).
func TestQuery_SecondVertex_WithLabel_UnknownLabel(t *testing.T) {
	t.Parallel()
	g, c := setupSocialGraph()
	// Register a label that no Person carries.
	g.SetNodeLabel("frank-the-bot", "Bot")
	e := New(g, c)
	got := e.Match().
		Vertex(WithLabel[string, int64]("Person")).
		Vertex(WithLabel[string, int64]("Bot")).
		Collect()
	if len(got) != 0 {
		t.Fatalf("Person ∩ Bot must be empty, got %v", got)
	}
}

// TestQuery_EqualValue_AllKinds exercises every kind branch of
// equalValue. We do this via WithProperty (which calls equalValue
// indirectly), covering equal, unequal, and kind-mismatch paths.
func TestQuery_EqualValue_AllKinds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	t0 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	bytesEq := []byte{0x01, 0x02, 0x03}
	bytesDiffLen := []byte{0x01, 0x02}
	bytesSameLenDiff := []byte{0x01, 0x02, 0xff}

	g.SetNodeLabel("a", "T")
	g.SetNodeProperty("a", "s", lpg.StringValue("hello"))
	g.SetNodeProperty("a", "i", lpg.Int64Value(42))
	g.SetNodeProperty("a", "f", lpg.Float64Value(3.14))
	g.SetNodeProperty("a", "b", lpg.BoolValue(true))
	g.SetNodeProperty("a", "t", lpg.TimeValue(t0))
	g.SetNodeProperty("a", "by", lpg.BytesValue(bytesEq))

	c := csr.BuildFromAdjList(g.AdjList())
	e := New(g, c)

	cases := []struct {
		name string
		key  string
		exp  lpg.PropertyValue
		want bool // whether "a" should be in the result
	}{
		{"string-eq", "s", lpg.StringValue("hello"), true},
		{"string-neq", "s", lpg.StringValue("world"), false},
		{"int64-eq", "i", lpg.Int64Value(42), true},
		{"int64-neq", "i", lpg.Int64Value(43), false},
		{"float64-eq", "f", lpg.Float64Value(3.14), true},
		{"float64-neq", "f", lpg.Float64Value(2.71), false},
		{"bool-eq", "b", lpg.BoolValue(true), true},
		{"bool-neq", "b", lpg.BoolValue(false), false},
		{"time-eq", "t", lpg.TimeValue(t0), true},
		{"time-neq", "t", lpg.TimeValue(t0.Add(time.Second)), false},
		{"bytes-eq", "by", lpg.BytesValue(bytesEq), true},
		{"bytes-neq-len", "by", lpg.BytesValue(bytesDiffLen), false},
		{"bytes-neq-content", "by", lpg.BytesValue(bytesSameLenDiff), false},
		// Kind mismatch (declared as String, queried as Int64).
		{"kind-mismatch", "s", lpg.Int64Value(0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := e.Match().
				Vertex(
					WithLabel[string, int64]("T"),
					WithProperty[string, int64](tc.key, tc.exp),
				).
				Collect()
			has := len(got) == 1 && got[0] == "a"
			if has != tc.want {
				t.Fatalf("case %q: got %v, want has=%v", tc.name, got, tc.want)
			}
		})
	}
}

// TestQuery_EqualValue_UnknownKind covers the final "return false"
// branch of equalValue: an unset (zero-value) PropertyValue carries
// kind 0, which matches no case in the switch. We build it directly
// since lpg has no constructor for the zero kind.
//
// We exercise it by querying for an unrecognised kind via the
// withProperty path: SetNodeProperty stores the value verbatim, and a
// later query whose expected value has kind 0 reaches the final
// "return false" — but only after the kind-mismatch short-circuit. To
// reach the fallthrough we set BOTH sides to kind 0.
func TestQuery_EqualValue_UnknownKind(t *testing.T) {
	t.Parallel()
	if equalValue(lpg.PropertyValue{}, lpg.PropertyValue{}) {
		t.Fatal("equalValue with two zero-kind values must return false (kind 0 is unhandled)")
	}
}

// TestQuery_WithLabel_Match_ResolveMiss exercises the defensive
// !ok branch in withLabel.Match: when the NodeID does not round-trip
// through the AdjList's mapper, the predicate must return false
// without panicking. This branch is unreachable under the v1
// query-engine semantics (every NodeID surfaced by the seed paths
// originates from the mapper), but the safety net is part of the
// function's contract — we drive it directly via the unexported
// withLabel struct from inside the package.
func TestQuery_WithLabel_Match_ResolveMiss(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	g.SetNodeLabel("alice", "Person")
	p := withLabel[string, int64]{name: "Person"}
	// An id far beyond MaxNodeID cannot Resolve.
	beyond := graph.NodeID(uint64(g.AdjList().MaxNodeID()) + 1<<20)
	if p.Match(g, beyond) {
		t.Fatal("withLabel.Match returned true for an unresolvable NodeID")
	}
}
