package graph

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestMapper_InternAndResolve_String(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()

	a := m.Intern("alpha")
	b := m.Intern("beta")
	a2 := m.Intern("alpha")

	if a != a2 {
		t.Fatalf("Intern is not stable: %d != %d", a, a2)
	}
	if a == b {
		t.Fatalf("distinct keys produced the same NodeID: %d", a)
	}

	if v, ok := m.Resolve(a); !ok || v != "alpha" {
		t.Fatalf("Resolve(%d) = (%q, %v), want (alpha, true)", a, v, ok)
	}
	if v, ok := m.Resolve(b); !ok || v != "beta" {
		t.Fatalf("Resolve(%d) = (%q, %v), want (beta, true)", b, v, ok)
	}

	if got, want := m.Len(), 2; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

func TestMapper_InternAndResolve_Int(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 1024

	ids := make([]NodeID, n)
	for i := 0; i < n; i++ {
		ids[i] = m.Intern(i)
	}
	for i := 0; i < n; i++ {
		if got := m.Intern(i); got != ids[i] {
			t.Fatalf("Intern(%d) drifted: %d -> %d", i, ids[i], got)
		}
	}
	for i := 0; i < n; i++ {
		v, ok := m.Resolve(ids[i])
		if !ok || v != i {
			t.Fatalf("Resolve(%d) = (%d, %v), want (%d, true)", ids[i], v, ok, i)
		}
	}
	if got, want := m.Len(), n; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

func TestMapper_InternAndResolve_Struct(t *testing.T) {
	t.Parallel()
	type key struct {
		ns string
		id int
	}
	m := NewMapper[key]()
	k1 := key{ns: "users", id: 1}
	k2 := key{ns: "users", id: 2}

	id1 := m.Intern(k1)
	id2 := m.Intern(k2)
	if id1 == id2 {
		t.Fatalf("distinct struct keys collided: %d", id1)
	}
	v, ok := m.Resolve(id1)
	if !ok || v != k1 {
		t.Fatalf("Resolve(%d) = (%+v, %v), want (%+v, true)", id1, v, ok, k1)
	}
}

func TestMapper_Lookup(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	if _, ok := m.Lookup("absent"); ok {
		t.Fatalf("Lookup of unknown key should report ok=false")
	}
	id := m.Intern("present")
	got, ok := m.Lookup("present")
	if !ok || got != id {
		t.Fatalf("Lookup(present) = (%d, %v), want (%d, true)", got, ok, id)
	}
	if l := m.Len(); l != 1 {
		t.Fatalf("Lookup of unknown key must not insert: Len = %d", l)
	}
}

func TestMapper_Resolve_Unknown(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	if v, ok := m.Resolve(packNodeID(0, 9999)); ok || v != "" {
		t.Fatalf("Resolve of unknown id returned (%q, %v), want (\"\", false)", v, ok)
	}
}

// TestMapper_InternSlow_DoubleCheckHit deterministically exercises the
// post-write-lock double-check inside internSlow: an entry for k is
// already present, so internSlow must observe it and return without
// allocating a new NodeID. This branch is the race resolution between
// two goroutines that both missed the read-locked fast path; calling
// internSlow directly makes the path observable without depending on
// scheduler timing.
func TestMapper_InternSlow_DoubleCheckHit(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	key := "already-present"
	first := m.Intern(key)

	shardIdx := mapperShardFor(key)
	s := &m.shards[shardIdx]
	got := m.internSlow(s, shardIdx, key)
	if got != first {
		t.Fatalf("internSlow double-check returned %d, want %d", got, first)
	}
	if l := m.Len(); l != 1 {
		t.Fatalf("Len = %d, want 1 (double-check must not insert)", l)
	}
}

// TestMapper_Concurrent_DoubleCheck exercises the production race
// resolution: many goroutines released simultaneously race to intern
// the same fresh key. The first to acquire the write lock allocates
// the NodeID; every other goroutine must observe the same NodeID.
func TestMapper_Concurrent_DoubleCheck(t *testing.T) {
	t.Parallel()
	m := NewMapper[string]()
	const goroutines = 256
	key := "shared-new-key"

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		ids   = make([]NodeID, goroutines)
	)
	start.Add(1)
	done.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer done.Done()
			start.Wait()
			ids[g] = m.Intern(key)
		}(g)
	}
	start.Done()
	done.Wait()

	for g := 1; g < goroutines; g++ {
		if ids[g] != ids[0] {
			t.Fatalf("racing Intern produced divergent ids: ids[0]=%d ids[%d]=%d", ids[0], g, ids[g])
		}
	}
	if got, want := m.Len(), 1; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

func TestMapper_PackUnpack(t *testing.T) {
	t.Parallel()
	cases := []struct {
		shard, idx uint64
	}{
		{0, 0},
		{1, 0},
		{255, 0},
		{0, 1},
		{42, 12345},
		{255, (1 << 56) - 1},
	}
	for _, c := range cases {
		id := packNodeID(c.shard, c.idx)
		shard, idx := unpackNodeID(id)
		if shard != c.shard || idx != c.idx {
			t.Fatalf("pack/unpack drift: (%d,%d) -> %d -> (%d,%d)",
				c.shard, c.idx, id, shard, idx)
		}
	}
}

func TestMapper_Concurrent_InternResolve(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const (
		goroutines = 256
		perWorker  = 1024
	)

	var wg sync.WaitGroup
	errCount := atomic.Int64{}
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// Mix of unique-per-goroutine and shared keys to
				// exercise both fresh-insert and fast-path lookup.
				k := (g * perWorker) + i
				id := m.Intern(k)
				if v, ok := m.Resolve(id); !ok || v != k {
					errCount.Add(1)
					return
				}
				// Repeated intern must yield the same id.
				if id2 := m.Intern(k); id2 != id {
					errCount.Add(1)
					return
				}
				// Touch a shared "hot" key from every goroutine.
				_ = m.Intern(-1)
			}
		}(g)
	}
	wg.Wait()
	if got := errCount.Load(); got != 0 {
		t.Fatalf("concurrent test reported %d errors", got)
	}
	// +1 for the shared "hot" key (-1).
	if got, want := m.Len(), goroutines*perWorker+1; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

func TestMapper_ShardDistribution(t *testing.T) {
	t.Parallel()
	m := NewMapper[int]()
	const n = 1 << 14
	for i := 0; i < n; i++ {
		m.Intern(i)
	}
	emptyShards := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		if len(s.reverse) == 0 {
			emptyShards++
		}
		s.mu.RUnlock()
	}
	if emptyShards > mapperShardCount/8 {
		t.Fatalf("hash distribution looks too skewed: %d/%d shards empty after %d inserts",
			emptyShards, mapperShardCount, n)
	}
}

func BenchmarkMapper_Intern_HotKey(b *testing.B) {
	m := NewMapper[string]()
	const key = "hot"
	m.Intern(key) // prime
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Intern(key)
	}
}

func BenchmarkMapper_Intern_HotKey_Parallel(b *testing.B) {
	m := NewMapper[string]()
	const key = "hot"
	m.Intern(key)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = m.Intern(key)
		}
	})
}

func BenchmarkMapper_Resolve(b *testing.B) {
	m := NewMapper[int]()
	const n = 1024
	ids := make([]NodeID, n)
	for i := 0; i < n; i++ {
		ids[i] = m.Intern(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Resolve(ids[i%n])
	}
}
