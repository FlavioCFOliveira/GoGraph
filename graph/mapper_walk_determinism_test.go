package graph

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// TestMapper_Walk_Determinism verifies that two successive Walk calls on
// the same Mapper yield identical (NodeID, key) sequences: same length,
// same order, same values.
func TestMapper_Walk_Determinism(t *testing.T) {
	t.Parallel()

	const n = 500
	m := NewMapper[string]()
	for i := 0; i < n; i++ {
		m.Intern(fmt.Sprintf("key-%04d", i))
	}

	type pair struct {
		id  NodeID
		key string
	}

	run1 := make([]pair, 0, n)
	m.Walk(func(id NodeID, k string) bool {
		run1 = append(run1, pair{id, k})
		return true
	})

	run2 := make([]pair, 0, n)
	m.Walk(func(id NodeID, k string) bool {
		run2 = append(run2, pair{id, k})
		return true
	})

	if len(run1) != n {
		t.Fatalf("run1 length = %d, want %d", len(run1), n)
	}
	if len(run2) != n {
		t.Fatalf("run2 length = %d, want %d", len(run2), n)
	}
	for i := range run1 {
		if run1[i] != run2[i] {
			t.Fatalf("position %d: run1=(%d,%q) run2=(%d,%q)",
				i, run1[i].id, run1[i].key, run2[i].id, run2[i].key)
		}
	}
}

// TestMapper_Walk_Determinism_PropertyBased uses rapid to verify the
// determinism invariant across arbitrary mapper sizes (0..200 keys).
func TestMapper_Walk_Determinism_PropertyBased(t *testing.T) {
	t.Parallel()

	type pair struct {
		id  NodeID
		key int
	}

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 200).Draw(rt, "n")
		m := NewMapper[int]()
		for i := 0; i < n; i++ {
			m.Intern(i)
		}

		run1 := make([]pair, 0, n)
		m.Walk(func(id NodeID, k int) bool {
			run1 = append(run1, pair{id, k})
			return true
		})

		run2 := make([]pair, 0, n)
		m.Walk(func(id NodeID, k int) bool {
			run2 = append(run2, pair{id, k})
			return true
		})

		if len(run1) != n {
			rt.Fatalf("run1 length = %d, want %d", len(run1), n)
		}
		if len(run2) != n {
			rt.Fatalf("run2 length = %d, want %d", len(run2), n)
		}
		for i := range run1 {
			if run1[i] != run2[i] {
				rt.Fatalf("position %d: run1=(%d,%d) run2=(%d,%d)",
					i, run1[i].id, run1[i].key, run2[i].id, run2[i].key)
			}
		}
	})
}
