//go:build threeway

// Package memprobe measures what GoGraph's own structural choices cost per
// node, in-process, where the Go runtime's accounting is exact.
//
// The containerised comparison in bench/comparison measures what GoGraph
// spends per node against Neo4j and Memgraph. It cannot say WHY. These probes
// answer the why by changing exactly one structural decision at a time and
// re-measuring, so that each figure is the cost of that decision and nothing
// else.
//
// # One arm per process
//
// Each probe is a SEPARATE top-level test and must be run on its own, one
// `go test -run` invocation per arm:
//
//	go test -count=1 -tags=threeway -run TestProbe_MapperStringKey -v ./bench/memprobe/
//
// That is not tidiness. A first version of this file ran several arms in one
// process and reported the SAME build at 93.06 B/node in one test and 44.53 in
// another. The difference, 44.09, was exactly the previous arm's graph: its
// only reference was a local in a frame that had already returned, the
// baseline reading still saw it live, and the collector reclaimed it during
// the next build — so the delta came out as new-graph MINUS old-graph. A
// process per arm makes the baseline the runtime's floor by construction, and
// probeFloor below asserts it.
package memprobe

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const probeNodes = 1_000_000

// probeFloor is the largest live heap a freshly started test process may hold
// before a probe's baseline reading is considered contaminated. A process that
// has only initialised the runtime and the testing package sits far below it;
// a process that is still holding an earlier arm's million-node graph does not.
const probeFloor = 8 << 20

// liveHeap returns the live heap after a full collection, with memory the
// runtime is merely holding released first.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// measure reports the live heap that build's result occupies, per element. The
// built value is kept alive across the reading, or the collector would reclaim
// the very thing being measured.
func measure(t *testing.T, name string, n int, build func() any) {
	t.Helper()
	before := liveHeap()
	if before > probeFloor {
		t.Fatalf("%s: baseline live heap is %d bytes, above the %d floor — this process is holding something else, and the delta would be this probe's graph minus whatever gets collected during the build",
			name, before, probeFloor)
	}
	got := build()
	after := liveHeap()
	if got == nil {
		t.Fatalf("%s: build returned nil — the graph would have been collected before it was measured", name)
	}
	t.Logf("PROBE %-32s live_heap_delta=%d bytes_per_element=%.2f n=%d baseline=%d",
		name, after-before, float64(after-before)/float64(n), n, before)

	// The profile must be written HERE, while got is still reachable. Go's
	// own -memprofile flag writes after every test has returned and forces a
	// collection first, by which point the structure being attributed has
	// been reclaimed and the profile describes an empty heap.
	if path := os.Getenv("PROBE_HEAP_PROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create heap profile: %v", err)
		}
		if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
			t.Fatalf("write heap profile: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close heap profile: %v", err)
		}
		t.Logf("heap profile written to %s", path)
	}
	runtime.KeepAlive(got)
}

// buildNodes interns probeNodes nodes under the key type the caller supplies,
// optionally giving each a label and a property.
func buildStringKeyed(t *testing.T, label, propKey string) any {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range probeNodes {
		k := "__cx_" + strconv.FormatUint(uint64(i), 16)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if label != "" {
			if err := g.SetNodeLabel(k, label); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		if propKey != "" {
			if err := g.SetNodeProperty(k, propKey, lpg.StringValue(fmt.Sprintf("p%08d", i))); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
	}
	return g
}

// TestProbe_MapperStringKey measures what GoGraph's EXTERNAL NODE KEY costs
// when it is the synthetic string Cypher creates.
//
// GoGraph identifies a node by a caller-supplied key of type N, interned by
// graph.Mapper into a dense NodeID. Cypher's CREATE has no caller-supplied
// key, so cypher/exec.CreateNode synthesises one — `__cx_` followed by a hex
// counter — and graph/mapper.go internSlowHook stores that string TWICE per
// node: once as the key of the shard's forward map and once in its reverse
// slice. The key is never visible to a Cypher caller; CreateNode's own
// documentation says so.
//
// Neo4j and Memgraph both identify a node by a dense integer and have no
// equivalent structure, so this is a cost GoGraph pays and they do not.
func TestProbe_MapperStringKey(t *testing.T) {
	measure(t, "mapper/string-synthetic-key", probeNodes, func() any {
		return buildStringKeyed(t, "", "")
	})
}

// TestProbe_MapperUint64Key is the counterfactual for the probe above: the
// same interning under an integer key, which is what the rivals use.
func TestProbe_MapperUint64Key(t *testing.T) {
	measure(t, "mapper/uint64-key", probeNodes, func() any {
		g := lpg.New[uint64, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := range probeNodes {
			if err := g.AddNode(uint64(i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		return g
	})
}

// TestProbe_NodesWithLabel adds one label to each node, so that the difference
// from TestProbe_MapperStringKey is the nodeLabelShards map entry and the
// label bitmap.
func TestProbe_NodesWithLabel(t *testing.T) {
	measure(t, "nodes+1label/string-key", probeNodes, func() any {
		return buildStringKeyed(t, "Bare", "")
	})
}

// TestProbe_NodesWithLabelAndProp adds one short string property on top, so
// that the difference from TestProbe_NodesWithLabel is the nodePropShards map
// entry, the propBag, and the value.
func TestProbe_NodesWithLabelAndProp(t *testing.T) {
	measure(t, "nodes+1label+1prop/string-key", probeNodes, func() any {
		return buildStringKeyed(t, "P1", "sid")
	})
}
