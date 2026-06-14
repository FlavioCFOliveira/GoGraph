package lpg

// metadata_lockfree_test.go — task #1503 (sprint S-PA2).
//
// Covers the lock-free copy-on-write label-name / property-key-name read path.
//
//   - BenchmarkNodeMetadataReadParallel demonstrates the contention win: many
//     goroutines concurrently materialise a node's labels and property names,
//     each resolving L+P interned ids to strings. Before #1503 every Resolve
//     took an RWMutex RLock/RUnlock cycle, so the parallel readers serialised
//     on the registry lock; after #1503 the read path is a single lock-free
//     atomic.Pointer load, so it scales with cores. Run with -cpu=1,8 (or rely
//     on b.RunParallel + GOMAXPROCS) and compare ns/op with benchstat.
//
//   - TestRegistryConcurrentInternResolve is the -race correctness gate: a
//     reader that observes an id (returned by Intern) MUST be able to Resolve
//     it in any snapshot it loads afterwards. This pins the copy-on-write
//     ordering argument: Intern publishes the snapshot carrying names[id]
//     before returning id, so the id is never referenceable before its name is
//     resolvable.
//
// Layer: short. Race-clean.

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// buildMetadataGraph returns a graph with one node carrying nLabels labels and
// nProps properties, plus the external key of that node.
func buildMetadataGraph(tb testing.TB, nLabels, nProps int) (*Graph[string, float64], string) {
	tb.Helper()
	g := New[string, float64](adjlist.Config{Directed: true})
	const key = "n0"
	for i := 0; i < nLabels; i++ {
		if err := g.SetNodeLabel(key, "Label"+strconv.Itoa(i)); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
	}
	for i := 0; i < nProps; i++ {
		if err := g.SetNodeProperty(key, "prop"+strconv.Itoa(i), Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g, key
}

// BenchmarkNodeMetadataReadParallel measures concurrent resolution of a node's
// labels and property-key names through the registry read path. The win of
// #1503 (lock-free COW vs per-item RWMutex) shows as flat or improving ns/op as
// parallelism rises, instead of the lock-contention cliff of the RWMutex.
func BenchmarkNodeMetadataReadParallel(b *testing.B) {
	g, key := buildMetadataGraph(b, 4, 8)
	id, ok := g.AdjList().Mapper().Lookup(key)
	if !ok {
		b.Fatal("node not mapped")
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var sink int
		for pb.Next() {
			labels := g.NodeLabelsByID(id)
			sink += len(labels)
			g.NodePropertiesByIDFunc(id, func(name string, _ PropertyValue) {
				sink += len(name)
			})
		}
		if sink < 0 {
			b.Fatal("unreachable")
		}
	})
}

// TestRegistryConcurrentInternResolve gates the COW ordering invariant under
// the race detector: every id handed back by Intern resolves to its name in
// every snapshot the caller subsequently loads, concurrently with other
// interns and resolves.
func TestRegistryConcurrentInternResolve(t *testing.T) {
	t.Run("label", func(t *testing.T) {
		reg := NewLabelRegistry()
		var wg sync.WaitGroup
		const writers, perWriter, readers = 8, 64, 8
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					name := fmt.Sprintf("L%d", w*perWriter+i)
					id := reg.Intern(name)
					got, ok := reg.Resolve(id)
					if !ok || got != name {
						t.Errorf("Resolve(%d)=%q,%v want %q", id, got, ok, name)
						return
					}
				}
			}(w)
		}
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWriter*writers; i++ {
					// Resolve a range of ids; any present id must round-trip.
					if name, ok := reg.Resolve(LabelID(i % 16)); ok && name == "" {
						t.Errorf("present id %d resolved to empty name", i%16)
						return
					}
				}
			}()
		}
		wg.Wait()
		// Final vocabulary is exactly the distinct names interned (bounded).
		want := writers * perWriter
		if _, ok := reg.Resolve(LabelID(want - 1)); !ok {
			t.Fatalf("expected %d distinct labels interned", want)
		}
		if _, ok := reg.Resolve(LabelID(want)); ok {
			t.Fatalf("registry grew beyond %d distinct names", want)
		}
	})

	t.Run("propertyKey", func(t *testing.T) {
		reg := NewPropertyKeyRegistry()
		var wg sync.WaitGroup
		const writers, perWriter = 8, 64
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					name := fmt.Sprintf("p%d", w*perWriter+i)
					id := reg.Intern(name)
					got, ok := reg.Resolve(id)
					if !ok || got != name {
						t.Errorf("Resolve(%d)=%q,%v want %q", id, got, ok, name)
						return
					}
				}
			}(w)
		}
		wg.Wait()
		want := writers * perWriter
		if _, ok := reg.Resolve(PropertyKeyID(want - 1)); !ok {
			t.Fatalf("expected %d distinct keys interned", want)
		}
		if _, ok := reg.Resolve(PropertyKeyID(want)); ok {
			t.Fatalf("registry grew beyond %d distinct names", want)
		}
	})
}

// TestRegistryInternIsBounded confirms the write path never grows beyond the
// distinct-name count: re-interning the same name returns the same id and does
// not extend the snapshot.
func TestRegistryInternIsBounded(t *testing.T) {
	reg := NewPropertyKeyRegistry()
	first := reg.Intern("dup")
	for i := 0; i < 1000; i++ {
		if got := reg.Intern("dup"); got != first {
			t.Fatalf("Intern(dup) #%d = %d, want %d", i, got, first)
		}
	}
	if _, ok := reg.Resolve(first + 1); ok {
		t.Fatal("registry grew on repeated intern of the same name")
	}
}
