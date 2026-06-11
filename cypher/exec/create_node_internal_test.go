package exec

// create_node_internal_test.go — package-internal coverage for the synthetic
// node-key parser and the [globalNodeCounter] seeding helper used by
// [CreateNode] to defend against cross-process counter resets.

import (
	"strconv"
	"sync/atomic"
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
}

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
func (m *seedStubMutator) RemoveEdge(string, string)         { panic("unused") }
func (m *seedStubMutator) SetNodeLabel(string, string) error { panic("unused") }
func (m *seedStubMutator) RemoveNodeLabel(string, string)    { panic("unused") }
func (m *seedStubMutator) SetNodeProperty(string, string, lpg.PropertyValue) error {
	panic("unused")
}
func (m *seedStubMutator) DelNodeProperty(string, string) { panic("unused") }
func (m *seedStubMutator) NodeProperties(string) map[string]lpg.PropertyValue {
	panic("unused")
}
func (m *seedStubMutator) NodeLabels(string) []string          { panic("unused") }
func (m *seedStubMutator) HasEdge(string, string) bool         { panic("unused") }
func (m *seedStubMutator) SetEdgeLabel(string, string, string) { panic("unused") }
func (m *seedStubMutator) SetEdgeProperty(string, string, string, lpg.PropertyValue) error {
	panic("unused")
}
func (m *seedStubMutator) DelEdgeProperty(string, string, string) { panic("unused") }
func (m *seedStubMutator) EdgeProperties(string, string) map[string]lpg.PropertyValue {
	panic("unused")
}
func (m *seedStubMutator) EdgeLabels(string, string) []string           { panic("unused") }
func (m *seedStubMutator) IncEdgeCreateCount(string, string) int64      { return 0 }
func (m *seedStubMutator) EdgeCreateCount(string, string) int64         { return 0 }
func (m *seedStubMutator) DecEdgeCreateCount(string, string)            {}
func (m *seedStubMutator) SetEdgeLabelAt(string, string, int64, string) {}
func (m *seedStubMutator) EdgeLabelsAt(string, string, int64) []string  { return nil }
func (m *seedStubMutator) SetEdgePropertyAt(string, string, int64, string, lpg.PropertyValue) error {
	return nil
}
func (m *seedStubMutator) EdgePropertiesAt(string, string, int64) map[string]lpg.PropertyValue {
	return nil
}
func (m *seedStubMutator) RemoveEdgeInstance(string, string, int64)            {}
func (m *seedStubMutator) SetEdgeLabelByHandle(string, string, uint64, string) {}
func (m *seedStubMutator) EdgeLabelsByHandle(string, string, uint64) []string  { return nil }
func (m *seedStubMutator) SetEdgePropertyByHandle(string, string, uint64, string, lpg.PropertyValue) error {
	return nil
}
func (m *seedStubMutator) EdgePropertiesByHandle(string, string, uint64) map[string]lpg.PropertyValue {
	return nil
}
func (m *seedStubMutator) RemoveEdgeInstanceByHandle(string, string, uint64) {}
func (m *seedStubMutator) OutNeighbours(string) []string                     { panic("unused") }
func (m *seedStubMutator) InNeighbours(string) []string                      { panic("unused") }
func (m *seedStubMutator) RemoveAllEdgesFrom(string)                         { panic("unused") }
func (m *seedStubMutator) OutDegree(string) int                              { panic("unused") }
func (m *seedStubMutator) ResolveNodeID(string) (graph.NodeID, bool)         { panic("unused") }
func (m *seedStubMutator) RemoveNode(string)                                 { panic("unused") }
func (m *seedStubMutator) IsTombstoned(graph.NodeID) bool                    { return false }

// Compile-time check: seedStubMutator must satisfy GraphMutator so the
// production seedGlobalNodeCounter accepts it directly. If a future change
// adds a method to GraphMutator, this line fails to compile and the stub
// must be updated accordingly.
var _ GraphMutator = (*seedStubMutator)(nil)

// TestSeedGlobalNodeCounter_LocalLogic exercises the CAS-based advancement
// logic with a local counter so the test is robust against ordering with
// other tests in the same binary that may already have advanced
// [globalNodeCounter].
func TestSeedGlobalNodeCounter_LocalLogic(t *testing.T) {
	// Local replica of the seeder body, parameterised on the counter and
	// the input. Behaviour MUST match seedGlobalNodeCounter exactly; if
	// it ever drifts, [TestCreateNode_InitSeedsCounter] catches the gap.
	seedLocal := func(counter *atomic.Uint64, keys map[graph.NodeID]string) {
		var maxSeen uint64
		for _, key := range keys {
			if v, ok := parseSynthKeySuffix(key); ok && v > maxSeen {
				maxSeen = v
			}
		}
		for {
			cur := counter.Load()
			if cur >= maxSeen {
				return
			}
			if counter.CompareAndSwap(cur, maxSeen) {
				return
			}
		}
	}

	t.Run("empty input leaves counter at zero", func(t *testing.T) {
		var c atomic.Uint64
		seedLocal(&c, nil)
		if got := c.Load(); got != 0 {
			t.Fatalf("counter advanced from empty input: got %d", got)
		}
	})

	t.Run("mix of create, merge and user keys advances past hex max", func(t *testing.T) {
		var c atomic.Uint64
		keys := map[graph.NodeID]string{
			1:  "alice",
			2:  "bob",
			10: "__cx_1",
			11: "__cx_a",
			12: "__cx_ff",
			13: "__cx_merge_ffff", // counted: merge keys share globalNodeCounter
			14: "__cy_1",          // ignored: wrong prefix
		}
		seedLocal(&c, keys)
		if got := c.Load(); got != 0xffff {
			t.Fatalf("counter = %d, want %d", got, 0xffff)
		}
	})

	t.Run("counter never rolls backwards under CAS", func(t *testing.T) {
		var c atomic.Uint64
		c.Store(100)
		keys := map[graph.NodeID]string{1: "__cx_a"} // max = 10
		seedLocal(&c, keys)
		if got := c.Load(); got != 100 {
			t.Fatalf("counter regressed from 100 to %d", got)
		}
	})
}

// TestCreateNode_InitSeedsCounter drives the actual production seeder by
// calling [seedGlobalNodeCounter] directly on a stub mutator that contains
// a high synthetic key. The package-level [globalNodeCounter] is advanced
// monotonically, so calling the seeder multiple times in the same test
// process is safe even if a previous test already fired
// [globalNodeCounterSeededOnce]: this test never relies on the Once gate.
func TestCreateNode_InitSeedsCounter(t *testing.T) {
	const seededMax = uint64(0x1000)
	m := newSeedStubMutator(map[graph.NodeID]string{
		1: "alice",
		2: "__cx_1000",
	})

	// Call the production seeder directly. It is idempotent under CAS,
	// so this is safe to invoke regardless of the package Once state.
	seedGlobalNodeCounter(m)

	if got := globalNodeCounter.Load(); got < seededMax {
		t.Fatalf("globalNodeCounter = %#x after seed, want >= %#x", got, seededMax)
	}

	// Use a CreateNode receiver to call freshNodeKey through the same
	// path production code uses.
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

// TestSeedGlobalNodeCounter_NilMutator confirms the no-op contract for a
// nil mutator. Important so unit tests that construct CreateNode without a
// backing mutator still work after the seeding hook was added to Init.
func TestSeedGlobalNodeCounter_NilMutator(t *testing.T) {
	before := globalNodeCounter.Load()
	seedGlobalNodeCounter(nil)
	if after := globalNodeCounter.Load(); after != before {
		t.Fatalf("counter changed from %d to %d for nil mutator", before, after)
	}
}
