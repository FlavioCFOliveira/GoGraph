package graph

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// collectEntries returns all (NodeID, key) pairs emitted by Walk in the
// order Walk visits them. Two consecutive calls on an unchanged Mapper
// must return identical slices (determinism contract).
func collectEntries[N comparable](m *Mapper[N]) []MapperEntry[N] {
	entries := make([]MapperEntry[N], 0, m.Len())
	m.Walk(func(id NodeID, k N) bool {
		entries = append(entries, MapperEntry[N]{ID: id, Key: k})
		return true
	})
	return entries
}

// TestMapper_LoadFrom_Extended_Int64 verifies round-trip fidelity when
// the natural key type is int64: intern keys, collect entries via Walk,
// rebuild a fresh Mapper via LoadFrom, and assert every key resolves to
// its original NodeID.
func TestMapper_LoadFrom_Extended_Int64(t *testing.T) {
	t.Parallel()

	const n = 300
	src := NewMapper[int64]()
	want := make(map[int64]NodeID, n)
	for i := 0; i < n; i++ {
		k := int64(i*1_000_003 - 500_000) // spread across positive and negative
		want[k] = src.Intern(k)
	}

	entries := collectEntries(src)

	dst := NewMapper[int64]()
	if err := dst.LoadFrom(entries); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := dst.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
	for k, expectedID := range want {
		got, ok := dst.Lookup(k)
		if !ok {
			t.Fatalf("Lookup(%d) not found after LoadFrom", k)
		}
		if got != expectedID {
			t.Errorf("Lookup(%d) = %d, want %d", k, got, expectedID)
		}
		// Intern must be idempotent after LoadFrom.
		if id := dst.Intern(k); id != expectedID {
			t.Errorf("Intern(%d) after LoadFrom = %d, want %d", k, id, expectedID)
		}
	}
}

// TestMapper_LoadFrom_Extended_UUID verifies round-trip fidelity when
// the natural key type is [16]byte (UUID-like). The mapper has a
// dedicated fast path for this type in mapperShardFor.
func TestMapper_LoadFrom_Extended_UUID(t *testing.T) {
	t.Parallel()

	const n = 256
	src := NewMapper[[16]byte]()
	want := make(map[[16]byte]NodeID, n)
	for i := 0; i < n; i++ {
		var k [16]byte
		// Fill bytes with a simple deterministic pattern so every key
		// is distinct while exercising all 16 bytes of the hash path.
		k[0] = byte(i)
		k[1] = byte(i >> 8)
		k[7] = byte(i * 3)
		k[15] = byte(i * 7)
		want[k] = src.Intern(k)
	}

	entries := collectEntries(src)

	dst := NewMapper[[16]byte]()
	if err := dst.LoadFrom(entries); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := dst.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
	for k, expectedID := range want {
		got, ok := dst.Lookup(k)
		if !ok {
			t.Fatalf("Lookup(%v) not found after LoadFrom", k)
		}
		if got != expectedID {
			t.Errorf("Lookup(%v) = %d, want %d", k, got, expectedID)
		}
	}
}

// TestMapper_LoadFrom_TwoSaveCycles verifies that two consecutive Walk
// passes over the same Mapper produce identical []MapperEntry slices —
// i.e. the serialisation order is deterministic and the mapper state is
// not mutated between passes.
func TestMapper_LoadFrom_TwoSaveCycles(t *testing.T) {
	t.Parallel()

	const n = 400
	m := NewMapper[string]()
	for i := 0; i < n; i++ {
		m.Intern(string(rune('a'+i%26)) + string(rune('0'+i%10)) + "_key")
	}

	first := collectEntries(m)
	second := collectEntries(m)

	if len(first) != len(second) {
		t.Fatalf("cycle lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("position %d differs: first={%d,%q} second={%d,%q}",
				i, first[i].ID, first[i].Key, second[i].ID, second[i].Key)
		}
	}
}

// mapperRoundtripMode is the subproc handler name for
// TestMapper_LoadFrom_CrossProcess.
const mapperRoundtripMode = "mapper-roundtrip"

func init() {
	subproc.Register(mapperRoundtripMode, func(_ []string) int {
		m := NewMapper[int]()
		ids := make([]uint64, 1000)
		for i := 0; i < 1000; i++ {
			ids[i] = uint64(m.Intern(i))
		}
		if err := json.NewEncoder(os.Stdout).Encode(ids); err != nil {
			_, _ = os.Stderr.WriteString("mapper-roundtrip: encode: " + err.Error() + "\n")
			return 1
		}
		return 0
	})
}

// TestMapper_LoadFrom_CrossProcess spawns a child process that interns
// 1000 consecutive int keys and writes the resulting NodeIDs as JSON.
// The parent interns the same keys, then asserts every NodeID matches
// the child's output.
//
// This test verifies that mapperShardFor uses a process-stable hash
// (FNV-1a), so the NodeID layout is reproducible across independent
// binary executions — the prerequisite for snapshot portability.
func TestMapper_LoadFrom_CrossProcess(t *testing.T) {
	t.Parallel()

	// Build the expected NodeIDs in the parent.
	const n = 1000
	parent := NewMapper[int]()
	expected := make([]uint64, n)
	for i := 0; i < n; i++ {
		expected[i] = uint64(parent.Intern(i))
	}

	// Spawn child.
	stdout, stderr, err := subproc.Run(t, mapperRoundtripMode)
	if err != nil {
		t.Fatalf("child process failed: %v\nstderr: %s", err, stderr)
	}

	var childIDs []uint64
	if err := json.Unmarshal(stdout, &childIDs); err != nil {
		t.Fatalf("decode child output: %v\nraw: %s", err, stdout)
	}
	if len(childIDs) != n {
		t.Fatalf("child returned %d ids, want %d", len(childIDs), n)
	}

	for i, want := range expected {
		if childIDs[i] != want {
			t.Errorf("key %d: parent NodeID=%d, child NodeID=%d (hash not stable across processes)", i, want, childIDs[i])
		}
	}
}
