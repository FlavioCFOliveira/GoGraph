package exec

// merge_search_differential_test.go — differential coverage for the MERGE
// access path (task #2217, acceptance criterion 3).
//
// The match phase used to walk EVERY interned node and filter afterwards, which
// made its cost track the size of the whole graph. It now drives from a label
// posting list when the pattern carries a label. That is a change of ACCESS
// PATH only: the candidate set is a superset of the matches and every label and
// property is still verified per candidate, so the set of admitted nodes must be
// byte-for-byte the same as the walk's.
//
// These tests prove that by running BOTH paths over the same mutator and
// comparing the multiset of matched node IDs. A label source of nil selects the
// walk, so the two paths are exercised from one table with no build tags.
//
// The property that makes this necessary: MERGE binds EVERY match. A narrower
// candidate set must never drop one, because dropping a row is invisible in the
// success/failure of the query — it only shows up as a missing ON MATCH effect
// or a spurious duplicate node.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergeDiffMutator is the minimum [GraphMutator] the merge search touches:
// candidate enumeration plus label and property resolution. The embedded
// interface is nil, so any method the search does not use panics rather than
// silently returning a zero value.
type mergeDiffMutator struct {
	GraphMutator
	order  []string                                // insertion order, so WalkNodeIDs is deterministic
	ids    map[string]graph.NodeID                 // key → id
	byID   map[graph.NodeID]string                 // id → key
	labels map[string]map[string]bool              // key → label set
	props  map[string]map[string]lpg.PropertyValue // key → property bag
	nextID graph.NodeID
}

func newMergeDiffMutator() *mergeDiffMutator {
	return &mergeDiffMutator{
		ids:    map[string]graph.NodeID{},
		byID:   map[graph.NodeID]string{},
		labels: map[string]map[string]bool{},
		props:  map[string]map[string]lpg.PropertyValue{},
	}
}

func (m *mergeDiffMutator) add(key string, labels []string, props map[string]lpg.PropertyValue) {
	if _, ok := m.ids[key]; !ok {
		id := m.nextID
		m.nextID++
		m.ids[key] = id
		m.byID[id] = key
		m.order = append(m.order, key)
	}
	if len(labels) > 0 && m.labels[key] == nil {
		m.labels[key] = map[string]bool{}
	}
	for _, l := range labels {
		m.labels[key][l] = true
	}
	if len(props) > 0 && m.props[key] == nil {
		m.props[key] = map[string]lpg.PropertyValue{}
	}
	for k, v := range props {
		m.props[key][k] = v
	}
}

func (m *mergeDiffMutator) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, key := range m.order {
		if !fn(m.ids[key]) {
			return
		}
	}
}

func (m *mergeDiffMutator) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	key, ok := m.byID[id]
	return key, ok
}

func (m *mergeDiffMutator) NodeLabels(key string) []string {
	set := m.labels[key]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func (m *mergeDiffMutator) NodeProperties(key string) map[string]lpg.PropertyValue {
	return m.props[key]
}

// stubLabelSource is a faithful label index over the mutator: it reports exactly
// the node IDs whose label set contains the name. Being derived from the
// mutator's own state, it models an index perfectly in step with the graph —
// the precondition the real lpg label index satisfies, since its bitmap
// reflects uncommitted writes of the enclosing transaction.
type stubLabelSource struct {
	m *mergeDiffMutator
}

func (s *stubLabelSource) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	bm := roaring64.New()
	for key, id := range s.m.ids {
		if s.m.labels[key][name] {
			bm.Add(uint64(id))
		}
	}
	return bm
}

// countingLabelSource wraps stubLabelSource and also reports cardinality, so the
// min-cardinality driving-label choice in mergeDrivingLabel is exercised.
type countingLabelSource struct {
	*stubLabelSource
	counted map[string]int
}

func (s *countingLabelSource) ResolveLabelCount(name string) (int64, bool) {
	if s.counted == nil {
		s.counted = map[string]int{}
	}
	s.counted[name]++
	return int64(s.ResolveLabelBitmap(name).GetCardinality()), true
}

// newDifferentialGraph builds a population designed to catch a candidate set
// that is too narrow: overlapping labels, a label of very different
// cardinality, matching and non-matching property values, an int/float pair
// that must compare equal, and nodes with no label at all.
func newDifferentialGraph(t *testing.T) *mergeDiffMutator {
	t.Helper()
	m := newMergeDiffMutator()
	add := m.add

	// Three nodes that all match (:A {v: 1}) — the multi-match core.
	add("a1", []string{"A"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	add("a2", []string{"A"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	add("a3", []string{"A"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	// Cross-type: 1.0 must compare equal to 1.
	add("a4", []string{"A"}, map[string]lpg.PropertyValue{"v": lpg.Float64Value(1.0)})
	// Same label, different value.
	add("a5", []string{"A"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(2)})
	// Overlapping labels: matches (:A) and (:B) and (:A:B).
	add("ab1", []string{"A", "B"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	add("ab2", []string{"A", "B"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(3)})
	// B only — must never satisfy an (:A) pattern.
	add("b1", []string{"B"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	// A large, unrelated label: the decoy population the walk used to examine.
	for i := 0; i < 50; i++ {
		add(fmt.Sprintf("c%d", i), []string{"C"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	}
	// No label at all, but a matching property — reachable only by a
	// property-only pattern, which has no label to drive from.
	add("bare", nil, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1)})
	// Extra property so a two-property pattern can be exercised.
	add("a6", []string{"A"}, map[string]lpg.PropertyValue{"v": lpg.Int64Value(1), "w": lpg.StringValue("x")})

	return m
}

// matchIDs runs searchMergeNodes and returns the matched node IDs, sorted so
// the comparison is order-insensitive. Order is not part of MERGE's contract;
// the SET of bound nodes is.
func matchIDs(t *testing.T, m *mergeDiffMutator, src MergeLabelSource, labels []string, props []propLiteral) []uint64 {
	t.Helper()
	rows, err := searchMergeNodes(context.Background(), m, src, labels, props)
	if err != nil {
		t.Fatalf("searchMergeNodes: %v", err)
	}
	out := make([]uint64, 0, len(rows))
	for _, r := range rows {
		id, ok := nodeIDFromValue(r[0])
		if !ok {
			t.Fatalf("row does not carry a node id: %#v", r)
		}
		out = append(out, uint64(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestMergeSearchAccessPathsAgree is the differential: for every pattern shape,
// the label-driven access path must admit exactly the nodes the full walk does.
func TestMergeSearchAccessPathsAgree(t *testing.T) {
	t.Parallel()
	m := newDifferentialGraph(t)

	mustProps := func(raw string) []propLiteral {
		t.Helper()
		if raw == "" {
			return nil
		}
		p, err := parsePropLiteral(raw)
		if err != nil {
			t.Fatalf("parsePropLiteral(%q): %v", raw, err)
		}
		return p
	}

	tests := []struct {
		name   string
		labels []string
		props  string
	}{
		{"labels-only-single", []string{"A"}, ""},
		{"labels-only-large", []string{"C"}, ""},
		{"labels-only-multi", []string{"A", "B"}, ""},
		{"labels-only-unknown", []string{"NoSuchLabel"}, ""},
		{"props-only", nil, `{v: 1}`},
		{"props-only-string", nil, `{w: "x"}`},
		{"props-only-nomatch", nil, `{v: 999}`},
		{"combined", []string{"A"}, `{v: 1}`},
		{"combined-multi-label", []string{"A", "B"}, `{v: 1}`},
		{"combined-two-props", []string{"A"}, `{v: 1, w: "x"}`},
		{"combined-cross-type-int", []string{"A"}, `{v: 1}`},
		{"combined-cross-type-float", []string{"A"}, `{v: 1.0}`},
		{"combined-nomatch-value", []string{"A"}, `{v: 42}`},
		{"combined-unknown-label", []string{"NoSuchLabel"}, `{v: 1}`},
		{"combined-label-without-prop", []string{"B"}, `{w: "x"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			props := mustProps(tc.props)

			viaWalk := matchIDs(t, m, nil, tc.labels, props)
			viaLabel := matchIDs(t, m, &stubLabelSource{m: m}, tc.labels, props)
			viaCounting := matchIDs(t, m, &countingLabelSource{stubLabelSource: &stubLabelSource{m: m}}, tc.labels, props)

			if !equalIDs(viaWalk, viaLabel) {
				t.Errorf("label-driven access path disagrees with the walk\n  walk:  %v\n  label: %v", viaWalk, viaLabel)
			}
			if !equalIDs(viaWalk, viaCounting) {
				t.Errorf("min-cardinality access path disagrees with the walk\n  walk:     %v\n  counting: %v", viaWalk, viaCounting)
			}
		})
	}
}

// TestMergeSearchLabelDrivenStillFindsEveryMatch states the multi-match
// property directly at the access-path level, independently of the engine
// tests: three nodes match (:A {v: 1}) plus the cross-type 1.0 and the
// overlapping :A:B node, so the pattern must admit five.
func TestMergeSearchLabelDrivenStillFindsEveryMatch(t *testing.T) {
	t.Parallel()
	m := newDifferentialGraph(t)

	props, err := parsePropLiteral(`{v: 1}`)
	if err != nil {
		t.Fatalf("parsePropLiteral: %v", err)
	}
	got := matchIDs(t, m, &stubLabelSource{m: m}, []string{"A"}, props)
	// a1, a2, a3 (int 1), a4 (float 1.0), ab1 (:A:B, 1), a6 (1 plus w) = 6.
	if len(got) != 6 {
		t.Errorf("(:A {v: 1}) matched %d nodes (%v), want 6; a narrower candidate set must never drop a match", len(got), got)
	}
}

// TestMergeDrivingLabelPicksSmallest asserts the cost decision: with several
// labels, the smallest posting list drives the enumeration. It is a cost
// choice only — every label is re-verified per candidate — but getting it wrong
// forfeits the point of the change.
func TestMergeDrivingLabelPicksSmallest(t *testing.T) {
	t.Parallel()
	m := newDifferentialGraph(t)
	src := &countingLabelSource{stubLabelSource: &stubLabelSource{m: m}}

	// :C has 50 nodes, :B has 3. The driving label must be :B.
	if got := mergeDrivingLabel(src, []string{"C", "B"}); got != "B" {
		t.Errorf("mergeDrivingLabel(C, B) = %q, want \"B\" (the smaller label)", got)
	}
	if got := mergeDrivingLabel(src, []string{"B", "C"}); got != "B" {
		t.Errorf("mergeDrivingLabel(B, C) = %q, want \"B\" regardless of order", got)
	}
	// A single label needs no cardinality lookup at all.
	src.counted = nil
	if got := mergeDrivingLabel(src, []string{"C"}); got != "C" {
		t.Errorf("mergeDrivingLabel(C) = %q, want \"C\"", got)
	}
	if len(src.counted) != 0 {
		t.Errorf("a single-label pattern consulted cardinality %v; it should short-circuit", src.counted)
	}
	// An unknown label has cardinality 0, so it is the cheapest to drive from
	// and correctly yields no candidates.
	if got := mergeDrivingLabel(src, []string{"C", "NoSuchLabel"}); got != "NoSuchLabel" {
		t.Errorf("mergeDrivingLabel(C, NoSuchLabel) = %q, want \"NoSuchLabel\" (cardinality 0)", got)
	}
}

// TestWalkMergeCandidatesFallsBackWithoutSource pins the two fallback
// conditions, because getting either wrong turns a match into a spurious
// create: a nil source, and a pattern with no labels to drive from.
func TestWalkMergeCandidatesFallsBackWithoutSource(t *testing.T) {
	t.Parallel()
	m := newDifferentialGraph(t)

	total := 0
	m.WalkNodeIDs(func(graph.NodeID) bool { total++; return true })

	for _, tc := range []struct {
		name   string
		src    MergeLabelSource
		labels []string
	}{
		{"nil source with labels", nil, []string{"A"}},
		{"source with no labels", &stubLabelSource{m: m}, nil},
		{"nil source and no labels", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seen := 0
			walkMergeCandidates(m, tc.src, tc.labels, func(graph.NodeID) bool { seen++; return true })
			if seen != total {
				t.Errorf("candidates = %d, want %d (the full walk); falling back must examine every node", seen, total)
			}
		})
	}
}

// TestWalkMergeCandidatesHonoursEarlyStop asserts that a false return stops the
// enumeration on both paths, which is how context cancellation propagates out
// of the search.
func TestWalkMergeCandidatesHonoursEarlyStop(t *testing.T) {
	t.Parallel()
	m := newDifferentialGraph(t)

	for _, tc := range []struct {
		name string
		src  MergeLabelSource
	}{
		{"walk", nil},
		{"label-driven", &stubLabelSource{m: m}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seen := 0
			walkMergeCandidates(m, tc.src, []string{"C"}, func(graph.NodeID) bool {
				seen++
				return false // stop immediately
			})
			if seen != 1 {
				t.Errorf("examined %d candidates after an immediate stop, want 1", seen)
			}
		})
	}
}

// equalIDs compares two sorted id slices.
func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
