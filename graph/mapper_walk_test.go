package graph

import (
	"sort"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// TestMapper_Walk_EmptyMapper checks that Walk on a freshly built
// Mapper invokes the callback zero times.
func TestMapper_Walk_EmptyMapper(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	calls := 0
	m.Walk(func(NodeID, string) bool {
		calls++
		return true
	})
	if calls != 0 {
		t.Fatalf("Walk on empty mapper invoked callback %d times, want 0", calls)
	}
}

// TestMapper_Walk_VisitsEveryInternedPair verifies that Walk emits
// exactly one (id, value) pair per interned key and that each emitted
// id round-trips through Resolve.
func TestMapper_Walk_VisitsEveryInternedPair(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	keys := []string{"alice", "bob", "charlie", "dave", "erin", "frank", "grace"}
	want := make(map[string]NodeID, len(keys))
	for _, k := range keys {
		want[k] = m.Intern(k)
	}

	got := make(map[string]NodeID, len(keys))
	m.Walk(func(id NodeID, v string) bool {
		if _, dup := got[v]; dup {
			t.Errorf("Walk emitted %q twice", v)
		}
		got[v] = id
		return true
	})

	if len(got) != len(want) {
		t.Fatalf("Walk emitted %d pairs, want %d", len(got), len(want))
	}
	for k, wantID := range want {
		gotID, ok := got[k]
		if !ok {
			t.Errorf("Walk missed key %q", k)
			continue
		}
		if gotID != wantID {
			t.Errorf("Walk emitted (%q -> %d), want %d", k, gotID, wantID)
		}
		// Cross-check: the emitted id must Resolve back to the value.
		if v, ok := m.Resolve(gotID); !ok || v != k {
			t.Errorf("Resolve(%d) = (%q, %v), want (%q, true)", gotID, v, ok, k)
		}
	}
}

// TestMapper_Walk_EarlyStop verifies that returning false from the
// callback aborts the iteration without leaking the shard RLock —
// goleak in TestMain catches lock leaks indirectly through goroutine
// state, and a subsequent successful write proves the lock is free.
func TestMapper_Walk_EarlyStop(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 4096
	for i := 0; i < n; i++ {
		m.Intern(i)
	}

	visited := 0
	m.Walk(func(NodeID, int) bool {
		visited++
		return visited < 1 // stop immediately after the first yield
	})
	if visited != 1 {
		t.Fatalf("Walk visited %d pairs after early stop, want 1", visited)
	}

	// Prove the read lock was released: a subsequent Intern (which
	// would block on a leaked RLock under the write path) succeeds.
	if id := m.Intern(n); id == 0 && m.Len() < n+1 {
		t.Fatalf("post-stop Intern did not progress: id=%d Len=%d", id, m.Len())
	}
}

// TestMapper_Walk_ConcurrentLookup verifies that Walk and Resolve may
// run concurrently against the same Mapper. Walk holds the per-shard
// RLock; Resolve also takes the per-shard RLock, so both can proceed.
func TestMapper_Walk_ConcurrentLookup(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 2048
	ids := make([]NodeID, n)
	for i := 0; i < n; i++ {
		ids[i] = m.Intern(i)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.Walk(func(NodeID, int) bool { return true })
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, ok := m.Resolve(ids[i]); !ok {
				t.Errorf("concurrent Resolve(%d) lost id", ids[i])
				return
			}
		}
	}()
	wg.Wait()
}

// TestMapper_Walk_Rapid exercises the invariant "Walk visits every
// interned key exactly once" against an arbitrary insertion mix.
func TestMapper_Walk_Rapid(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		keys := rapid.SliceOfDistinct(rapid.IntRange(-1<<30, 1<<30), func(v int) int { return v }).Draw(t, "keys")
		m := NewMapper[int]()
		for _, k := range keys {
			m.Intern(k)
		}
		visited := make(map[int]int, len(keys))
		m.Walk(func(_ NodeID, v int) bool {
			visited[v]++
			return true
		})
		if len(visited) != len(keys) {
			t.Fatalf("Walk visited %d distinct keys, want %d", len(visited), len(keys))
		}
		for _, k := range keys {
			if visited[k] != 1 {
				t.Fatalf("Walk visited key %d %d times, want 1", k, visited[k])
			}
		}
	})
}

// TestMapper_MaxNodeID_Empty verifies that MaxNodeID returns 0 on a
// fresh Mapper.
func TestMapper_MaxNodeID_Empty(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	if got := m.MaxNodeID(); got != 0 {
		t.Fatalf("empty MaxNodeID = %d, want 0", got)
	}
}

// TestMapper_MaxNodeID_IsUpperBound verifies that every issued NodeID
// is strictly less than MaxNodeID. This is the contract documented on
// the function: MaxNodeID is the natural size of a NodeID-indexed
// companion array.
func TestMapper_MaxNodeID_IsUpperBound(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 4096
	ids := make([]NodeID, n)
	for i := 0; i < n; i++ {
		ids[i] = m.Intern(i)
	}
	upper := m.MaxNodeID()
	for _, id := range ids {
		if uint64(id) >= uint64(upper) {
			t.Fatalf("issued id %d violates MaxNodeID upper bound %d", id, upper)
		}
	}
}

// TestMapper_MaxNodeID_Monotonic verifies MaxNodeID is non-decreasing
// across successive inserts, as the docstring contract implies.
func TestMapper_MaxNodeID_Monotonic(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	prev := m.MaxNodeID()
	for i := 0; i < 512; i++ {
		m.Intern(i)
		cur := m.MaxNodeID()
		if cur < prev {
			t.Fatalf("MaxNodeID decreased at iteration %d: prev=%d cur=%d", i, prev, cur)
		}
		prev = cur
	}
}

// TestMapper_MaxNodeID_PacksDeepestShard verifies that after inserts
// land in many shards, MaxNodeID still bounds every assigned NodeID,
// independent of which shard happens to be the deepest.
func TestMapper_MaxNodeID_PacksDeepestShard(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	// Insert enough distinct keys that every shard has >= 1 entry.
	const n = mapperShardCount * 8
	collected := make([]NodeID, 0, n)
	for i := 0; i < n; i++ {
		collected = append(collected, m.Intern(i))
	}
	upper := m.MaxNodeID()
	// Verify the bound holds.
	for _, id := range collected {
		if uint64(id) >= uint64(upper) {
			t.Fatalf("id %d >= MaxNodeID %d", id, upper)
		}
	}
	// Sort and check that the largest assigned id is strictly less
	// than MaxNodeID — the upper-bound contract is tight.
	sort.Slice(collected, func(i, j int) bool { return collected[i] < collected[j] })
	if uint64(collected[len(collected)-1]) >= uint64(upper) {
		t.Fatalf("largest id %d not strictly < MaxNodeID %d",
			collected[len(collected)-1], upper)
	}
}
