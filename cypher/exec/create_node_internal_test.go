package exec

// create_node_internal_test.go — package-internal coverage for the synthetic
// node-key parser and the per-graph key-sequence seeding helper
// ([seedNodeKeySequence]) used by [CreateNode] to start past recovered keys.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestParseSynthKeySuffix verifies the recogniser used by
// [seedGlobalNodeCounter] to extract numeric suffixes from synthetic node
// keys produced by [CreateNode.freshNodeKey].
func TestParseSynthKeySuffix(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		wantV  uint64
		wantOK bool
	}{
		{"empty", "", 0, false},
		{"plain user key", "alice", 0, false},
		{"prefix only", "__cx_", 0, false},
		{"valid one", "__cx_1", 1, true},
		{"valid hex multi", "__cx_ff", 0xff, true},
		{"max uint64 hex", "__cx_ffffffffffffffff", 0xffffffffffffffff, true},
		{"merge form one", "__cx_merge_1", 1, true},
		{"merge form hex multi", "__cx_merge_ff", 0xff, true},
		{"merge prefix only", "__cx_merge_", 0, false},
		{"merge form non-hex tail", "__cx_merge_xyz", 0, false},
		{"non-hex tail char", "__cx_abz", 0, false},
		{"different prefix", "__cy_1", 0, false},
		{"trailing junk", "__cx_1.0", 0, false},
		{"negative literal", "__cx_-1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSynthKeySuffix(tc.key)
			if ok != tc.wantOK || got != tc.wantV {
				t.Fatalf("parseSynthKeySuffix(%q) = (%d, %v), want (%d, %v)", tc.key, got, ok, tc.wantV, tc.wantOK)
			}
		})
	}
}

// seedStubMutator is a minimal [GraphMutator] used by the seeding tests. It
// implements WalkNodeIDs / ResolveNodeLabel from a fixed map; the other
// methods panic so any future change to the seeder's surface fails loudly.
type seedStubMutator struct {
	keys map[graph.NodeID]string
	seq  lpg.KeySequence
}

// KeySequence returns the stub graph's key sequence.
func (m *seedStubMutator) KeySequence() *lpg.KeySequence { return &m.seq }

func newSeedStubMutator(keys map[graph.NodeID]string) *seedStubMutator {
	return &seedStubMutator{keys: keys}
}

func (m *seedStubMutator) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for id := range m.keys {
		if !fn(id) {
			return
		}
	}
}

func (m *seedStubMutator) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	k, ok := m.keys[id]
	return k, ok
}

func (m *seedStubMutator) AddNode(string) (graph.NodeID, error) { panic("unused") }
func (m *seedStubMutator) AddEdge(string, string, float64) (graph.NodeID, graph.NodeID, error) {
	panic("unused")
}
func (m *seedStubMutator) AddEdgeH(string, string, float64) (graph.NodeID, graph.NodeID, uint64, error) {
	panic("unused")
}
func (m *seedStubMutator) RemoveEdge(string, string)                 { panic("unused") }
func (m *seedStubMutator) RemoveEdgeByHandle(string, string, uint64) { panic("unused") }
func (m *seedStubMutator) SetNodeLabel(string, string) error         { panic("unused") }
func (m *seedStubMutator) RemoveNodeLabel(string, string) error      { panic("unused") }
func (m *seedStubMutator) SetNodeProperty(string, string, lpg.PropertyValue) error {
	panic("unused")
}
func (m *seedStubMutator) DelNodeProperty(string, string) error { panic("unused") }
func (m *seedStubMutator) NodeProperties(string) map[string]lpg.PropertyValue {
	panic("unused")
}
func (m *seedStubMutator) NodeLabels(string) []string                { panic("unused") }
func (m *seedStubMutator) HasEdge(string, string) bool               { panic("unused") }
func (m *seedStubMutator) SetEdgeLabel(string, string, string) error { panic("unused") }
func (m *seedStubMutator) SetEdgeProperty(string, string, string, lpg.PropertyValue) error {
	panic("unused")
}
func (m *seedStubMutator) DelEdgeProperty(string, string, string) error { panic("unused") }
func (m *seedStubMutator) EdgeProperties(string, string) map[string]lpg.PropertyValue {
	panic("unused")
}
func (m *seedStubMutator) EdgeLabels(string, string) []string                 { panic("unused") }
func (m *seedStubMutator) IncEdgeCreateCount(string, string) int64            { return 0 }
func (m *seedStubMutator) EdgeCreateCount(string, string) int64               { return 0 }
func (m *seedStubMutator) DecEdgeCreateCount(string, string)                  {}
func (m *seedStubMutator) SetEdgeLabelAt(string, string, int64, string) error { return nil }
func (m *seedStubMutator) EdgeLabelsAt(string, string, int64) []string        { return nil }
func (m *seedStubMutator) SetEdgePropertyAt(string, string, int64, string, lpg.PropertyValue) error {
	return nil
}
func (m *seedStubMutator) EdgePropertiesAt(string, string, int64) map[string]lpg.PropertyValue {
	return nil
}
func (m *seedStubMutator) RemoveEdgeInstance(string, string, int64)                  {}
func (m *seedStubMutator) SetEdgeLabelByHandle(string, string, uint64, string) error { return nil }
func (m *seedStubMutator) EdgeLabelsByHandle(string, string, uint64) []string        { return nil }
func (m *seedStubMutator) SetEdgePropertyByHandle(string, string, uint64, string, lpg.PropertyValue) error {
	return nil
}
func (m *seedStubMutator) DelEdgePropertyByHandle(string, string, uint64, string) error { return nil }
func (m *seedStubMutator) EdgePropertiesByHandle(string, string, uint64) map[string]lpg.PropertyValue {
	return nil
}
func (m *seedStubMutator) RemoveEdgeInstanceByHandle(string, string, uint64) {}
func (m *seedStubMutator) FirstEdgeHandle(string, string) (uint64, bool)     { return 0, false }
func (m *seedStubMutator) EdgeHandles(_, _ string, buf []uint64) []uint64    { return buf }
func (m *seedStubMutator) HasEdgeHandle(string, string, uint64) bool         { return false }
func (m *seedStubMutator) OutNeighbours(string) []string                     { panic("unused") }
func (m *seedStubMutator) InNeighbours(string) []string                      { panic("unused") }
func (m *seedStubMutator) RemoveAllEdgesFrom(string)                         { panic("unused") }
func (m *seedStubMutator) OutDegree(string) int                              { panic("unused") }

// ResolveNodeID answers mintNodeKey's probe (rmp #3015) from the stub's keys.
func (m *seedStubMutator) ResolveNodeID(k string) (graph.NodeID, bool) {
	for id, v := range m.keys {
		if v == k {
			return id, true
		}
	}
	return 0, false
}
func (m *seedStubMutator) RemoveNode(string)              { panic("unused") }
func (m *seedStubMutator) IsTombstoned(graph.NodeID) bool { return false }

// Compile-time check: seedStubMutator must satisfy GraphMutator so the
// production seedNodeKeySequence accepts it directly. If a future change
// adds a method to GraphMutator, this line fails to compile and the stub
// must be updated accordingly.
var _ GraphMutator = (*seedStubMutator)(nil)

// TestSeedNodeKeySequence_AdvancesPastHexMax: the seed raises the graph's
// sequence past the largest suffix of either synthetic key form and ignores
// every other key.
func TestSeedNodeKeySequence_AdvancesPastHexMax(t *testing.T) {
	m := newSeedStubMutator(map[graph.NodeID]string{
		1:  "alice",
		2:  "bob",
		10: "__cx_1",
		11: "__cx_a",
		12: "__cx_ff",
		13: "__cx_merge_ffff", // counted: merge keys share the sequence
		14: "__cy_1",          // ignored: wrong prefix
	})
	seedNodeKeySequence(m)
	if got := m.seq.Load(); got != 0xffff {
		t.Fatalf("sequence = %#x, want %#x", got, 0xffff)
	}
}

// TestSeedNodeKeySequence_NeverRollsBackAndRunsOnce: a seed below the current
// value leaves it, and a second seed of the same graph does not scan again.
func TestSeedNodeKeySequence_NeverRollsBackAndRunsOnce(t *testing.T) {
	m := newSeedStubMutator(map[graph.NodeID]string{1: "__cx_a"}) // max = 10
	m.seq.Add(100)
	seedNodeKeySequence(m)
	if got := m.seq.Load(); got != 100 {
		t.Fatalf("sequence regressed from 100 to %d", got)
	}
	m.keys[2] = "__cx_fffff"
	seedNodeKeySequence(m)
	if got := m.seq.Load(); got != 100 {
		t.Fatalf("a second seed of one graph scanned again: sequence %d", got)
	}
}

// TestCreateNode_InitSeedsSequence drives the seeder through CreateNode.Init's
// helper and mints through the production path: the minted suffix is past the
// seeded maximum.
func TestCreateNode_InitSeedsSequence(t *testing.T) {
	const seededMax = uint64(0x1000)
	m := newSeedStubMutator(map[graph.NodeID]string{
		1: "alice",
		2: "__cx_1000",
	})
	seedNodeKeySequence(m)
	if got := m.seq.Load(); got < seededMax {
		t.Fatalf("sequence = %#x after seed, want >= %#x", got, seededMax)
	}
	op := &CreateNode{mutator: m}
	next := op.freshNodeKey()
	suffix, ok := parseSynthKeySuffix(next)
	if !ok {
		t.Fatalf("freshNodeKey returned non-synthetic key %q", next)
	}
	if suffix <= seededMax {
		t.Fatalf("freshNodeKey suffix %#x not strictly greater than seeded max %#x", suffix, seededMax)
	}
	if _, err := strconv.ParseUint(next[len(synthKeyPrefix):], 16, 64); err != nil {
		t.Fatalf("freshNodeKey %q has non-hex suffix: %v", next, err)
	}
}

// TestMintNodeKey_PerGraphDeterminism: two graphs in one process mint the same
// keys for the same history. Before the sequence moved onto the graph, the
// second graph continued the first one's process-global counter, so a
// simulator replaying one seed twice in a process created different keys —
// different mapper shards, hence different WAL id reservations (WAL v2 step 4).
func TestMintNodeKey_PerGraphDeterminism(t *testing.T) {
	mint := func() []string {
		m := newSeedStubMutator(map[graph.NodeID]string{})
		seedNodeKeySequence(m)
		out := make([]string, 0, 4)
		for i := 0; i < 4; i++ {
			k := mintNodeKey(m, "")
			m.keys[graph.NodeID(i+1)] = k
			out = append(out, k)
		}
		return out
	}
	a, b := mint(), mint()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("graph 2 minted %v, graph 1 minted %v", b, a)
		}
	}
	if a[0] != synthKeyPrefix+"1" {
		t.Fatalf("a fresh graph minted %q first, want %q", a[0], synthKeyPrefix+"1")
	}
}

// TestSeedNodeKeySequence_NilMutator confirms the no-op contract for a nil
// mutator, so unit tests that construct CreateNode without a backing mutator
// still work, and that minting without a mutator still yields a synthetic key.
func TestSeedNodeKeySequence_NilMutator(t *testing.T) {
	seedNodeKeySequence(nil)
	if k := mintNodeKey(nil, ""); k != synthKeyPrefix+"1" {
		t.Fatalf("mintNodeKey(nil) = %q", k)
	}
}
