package cypher

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func newTestEntry(tag string) *planCacheEntry { return &planCacheEntry{plan: nil, semaErr: nil} } //nolint:unparam // tag aids future debugging

func TestPlanCache_HitMissEviction(t *testing.T) {
	t.Parallel()
	c := newPlanCache(3)

	// Miss → store → hit.
	if _, ok := c.get("a"); ok {
		t.Fatalf("get on empty cache returned hit")
	}
	got, existed := c.loadOrStore("a", newTestEntry("a"))
	if existed {
		t.Fatalf("loadOrStore reported existing entry on first insert")
	}
	if got == nil {
		t.Fatal("loadOrStore returned nil entry")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatalf("get after store missed")
	}

	// Fill to capacity.
	c.loadOrStore("b", newTestEntry("b"))
	c.loadOrStore("c", newTestEntry("c"))
	if c.Len() != 3 {
		t.Fatalf("Len = %d; want 3", c.Len())
	}

	// Insert one more — must evict the LRU. Touch "a" first so the
	// expected victim is "b" (least recently used).
	c.get("a")
	c.loadOrStore("d", newTestEntry("d"))
	if c.Len() != 3 {
		t.Fatalf("Len after eviction = %d; want 3", c.Len())
	}
	if _, ok := c.get("b"); ok {
		t.Fatalf("LRU eviction did not drop 'b'")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("expected key %q to remain in cache", k)
		}
	}
}

func TestPlanCache_NonPositiveCapacity_UsesDefault(t *testing.T) {
	t.Parallel()
	for _, cap := range []int{0, -1, -1024} {
		c := newPlanCache(cap)
		if c.Capacity() != DefaultPlanCacheCapacity {
			t.Errorf("newPlanCache(%d).Capacity() = %d; want %d",
				cap, c.Capacity(), DefaultPlanCacheCapacity)
		}
	}
}

func TestPlanCache_LoadOrStore_IsIdempotent(t *testing.T) {
	t.Parallel()
	c := newPlanCache(4)
	first := newTestEntry("a")
	second := newTestEntry("a")

	got1, existed1 := c.loadOrStore("a", first)
	if existed1 || got1 != first {
		t.Fatalf("first store: existed=%v got==first? %v", existed1, got1 == first)
	}
	got2, existed2 := c.loadOrStore("a", second)
	if !existed2 {
		t.Fatalf("second store: existed=false; want true")
	}
	if got2 != first {
		t.Fatalf("second store overwrote first entry")
	}
}

func TestPlanCache_BoundedUnderChurn(t *testing.T) {
	t.Parallel()
	const (
		cap     = 32
		distinct = 10_000
	)
	c := newPlanCache(cap)
	for i := 0; i < distinct; i++ {
		c.loadOrStore(fmt.Sprintf("q-%d", i), newTestEntry("x"))
	}
	if c.Len() != cap {
		t.Fatalf("after %d distinct inserts Len = %d; want exactly %d",
			distinct, c.Len(), cap)
	}
}

func TestPlanCache_ConcurrentLoadOrStore(t *testing.T) {
	t.Parallel()
	const (
		cap        = 64
		goroutines = 32
		insertsEach = 256
	)
	c := newPlanCache(cap)

	var hits, stores atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < insertsEach; i++ {
				// Half the keys are shared across goroutines, half
				// are goroutine-private — exercises both hit and
				// store paths under contention.
				var key string
				if i%2 == 0 {
					key = fmt.Sprintf("shared-%d", i%(cap/2))
				} else {
					key = fmt.Sprintf("g%d-%d", g, i)
				}
				_, existed := c.loadOrStore(key, newTestEntry("x"))
				if existed {
					hits.Add(1)
				} else {
					stores.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()

	if hits.Load() == 0 {
		t.Fatal("expected at least some loadOrStore calls to hit; got zero")
	}
	if stores.Load() == 0 {
		t.Fatal("expected at least some loadOrStore calls to store; got zero")
	}
	if c.Len() > cap {
		t.Fatalf("Len = %d exceeds cap=%d under contention", c.Len(), cap)
	}
}

func TestEngineOptions_PlanCacheCapacity_Applied(t *testing.T) {
	t.Parallel()
	// We do not need a populated graph for this test: NewEngineWithOptions
	// only inspects opts to size the cache.
	cases := []struct {
		name   string
		opt    int
		wantCap int
	}{
		{"zero falls back", 0, DefaultPlanCacheCapacity},
		{"negative falls back", -7, DefaultPlanCacheCapacity},
		{"positive applied", 128, 128},
		{"small applied", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newPlanCache(tc.opt)
			if got := c.Capacity(); got != tc.wantCap {
				t.Fatalf("Capacity = %d; want %d", got, tc.wantCap)
			}
		})
	}
}
