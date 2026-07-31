package snapshot_test

// empty_registry_shortcircuit_test.go — the label and property component
// writers must not walk the graph when the registry proves there is nothing to
// find (rmp #2271).
//
// Layer: short.
//
// # Why this is a durability-adjacent test, not a micro-optimisation
//
// Both writers run inside the checkpoint's phase-1 exclusion window, which
// rmp #2269 widened deliberately: making the capture atomic — every component
// serialised at one instant — is what stopped a checkpoint publishing a partial
// transaction. The cost was that the window stalls every writer for as long as
// the capture takes, and on 200 000 nodes carrying NO labels and NO properties
// the two writers spent 11.886 ms and 14.578 ms walking a graph that had
// nothing for them. That is 26 ms of writer stall, every checkpoint, buying
// nothing.
//
// The gate is the registry itself. Labels and property keys can only be
// attached through their registry, which is append-only and never reassigns an
// id, so an EMPTY table proves no node and no edge carries one. The converse is
// deliberately NOT assumed: a name outlives the last value that used it, so a
// non-empty table still walks.
//
// # What is asserted, and why it needed an engagement counter
//
// Two properties. First, the OUTPUT must be unchanged — the short-circuit skips
// a walk whose result was empty, so every byte must be identical, and a
// component whose bytes changed would be a snapshot-format break, not a
// speed-up. Second, the walk must actually be SKIPPED.
//
// The second one is where the first draft of this test was wrong, and the
// mistake is worth recording. It asserted allocations, on the reasoning that a
// walk over 200 000 nodes must allocate more than no walk at all. It does not:
// the collectors were already allocation-optimised, defining their visit
// closures once and streaming through them, so the walk costs CPU and almost no
// heap. The test duly PASSED against the unfixed code — a gate that could not
// fail, which is the failure mode this project keeps re-learning.
//
// The skip changes no byte, no allocation, and only wall clock, which is not
// admissible as a gate on a shared machine (rmp #2268). So the production code
// carries an explicit engagement counter, and that is what is asserted here.
// Wall clock is recorded in the certification document, not gated:
// CaptureGraph on 200 000 attribute-free nodes went 35.853 ms to 8.972 ms,
// while the same graph WITH labels and properties was unchanged at ~73 ms.

import (
	"bytes"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// bareGraph builds n nodes and e edges carrying no labels and no properties.
func bareGraph(t *testing.T, n, e int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < e; i++ {
		if err := g.AddEdge(fmt.Sprintf("n%d", i%n), fmt.Sprintf("n%d", (i*7+1)%n), 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	return g
}

// skipProbe counts the two short-circuit engagements.
type skipProbe struct{ labels, props atomic.Uint64 }

func (p *skipProbe) IncCounter(name string, delta uint64) {
	switch name {
	case snapshot.MetricLabelsSkippedEmptyRegistry:
		p.labels.Add(delta)
	case snapshot.MetricPropertiesSkippedEmptyRegistry:
		p.props.Add(delta)
	}
}
func (p *skipProbe) ObserveLatency(string, time.Duration) {}

// withSkipProbe installs a metrics backend for the duration of fn.
func withSkipProbe(fn func()) *skipProbe {
	p := &skipProbe{}
	metrics.SetBackend(p)
	defer metrics.SetBackend(nil)
	fn()
	return p
}

// TestEmptyRegistry_WalkIsSkipped is the gate: on a graph whose registries are
// empty, both writers must take the short-circuit, and on a graph that carries
// attributes neither may.
//
// The second half matters as much as the first. A short-circuit that fired
// whenever it felt like it would drop real labels from a snapshot, so the
// attributed case is the guard against over-eager skipping.
func TestEmptyRegistry_WalkIsSkipped(t *testing.T) {
	t.Run("attribute-free graph skips both walks", func(t *testing.T) {
		g := bareGraph(t, 200000, 100000)
		p := withSkipProbe(func() {
			if _, _, err := snapshot.WriteLabels(io.Discard, g); err != nil {
				t.Fatalf("WriteLabels: %v", err)
			}
			if _, _, err := snapshot.WriteProperties(io.Discard, g); err != nil {
				t.Fatalf("WriteProperties: %v", err)
			}
		})
		if p.labels.Load() != 1 {
			t.Fatalf("WriteLabels did not skip the walk (%s fired %d times, want 1): it is walking "+
				"200 000 nodes inside the checkpoint's exclusion window to find labels an empty "+
				"registry already proves cannot exist",
				snapshot.MetricLabelsSkippedEmptyRegistry, p.labels.Load())
		}
		if p.props.Load() != 1 {
			t.Fatalf("WriteProperties did not skip the walk (%s fired %d times, want 1)",
				snapshot.MetricPropertiesSkippedEmptyRegistry, p.props.Load())
		}
	})

	t.Run("a graph carrying attributes still walks", func(t *testing.T) {
		g := bareGraph(t, 200, 100)
		if err := g.SetNodeLabel("n1", "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty("n1", "w", lpg.Int64Value(1)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		p := withSkipProbe(func() {
			if _, _, err := snapshot.WriteLabels(io.Discard, g); err != nil {
				t.Fatalf("WriteLabels: %v", err)
			}
			if _, _, err := snapshot.WriteProperties(io.Discard, g); err != nil {
				t.Fatalf("WriteProperties: %v", err)
			}
		})
		if p.labels.Load() != 0 || p.props.Load() != 0 {
			t.Fatalf("a writer short-circuited on a graph that DOES carry attributes "+
				"(labels %d, properties %d): the snapshot would lose them",
				p.labels.Load(), p.props.Load())
		}
	})
}

// TestEmptyRegistry_OutputIsUnchanged pins the other half: the short-circuit
// must be invisible in the bytes. It covers the attribute-free case that takes
// the new path and three attributed cases that must still take the old one, so
// a future change that short-circuits too eagerly fails here.
func TestEmptyRegistry_OutputIsUnchanged(t *testing.T) {
	mk := func(nodeAttrs, edgeAttrs bool) *lpg.Graph[string, float64] {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < 500; i++ {
			k := fmt.Sprintf("n%d", i)
			if err := g.AddNode(k); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			if nodeAttrs {
				if err := g.SetNodeLabel(k, "P"); err != nil {
					t.Fatalf("SetNodeLabel: %v", err)
				}
				if err := g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i))); err != nil {
					t.Fatalf("SetNodeProperty: %v", err)
				}
			}
		}
		for i := 0; i < 300; i++ {
			s, d := fmt.Sprintf("n%d", i%500), fmt.Sprintf("n%d", (i*7+1)%500)
			if err := g.AddEdge(s, d, 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			if edgeAttrs {
				g.SetEdgeLabel(s, d, "K")
				if err := g.SetEdgeProperty(s, d, "z", lpg.Int64Value(int64(i))); err != nil {
					t.Fatalf("SetEdgeProperty: %v", err)
				}
			}
		}
		g.RemoveNode("n3")
		return g
	}

	// Captured from the pre-fix code on the identical fixtures, so these are a
	// genuine before/after comparison rather than a re-recording of whatever
	// the current code happens to emit.
	cases := []struct {
		name                 string
		nodeAttrs, edgeAttrs bool
		labelSize            int64
		labelCRC             uint32
		propSize             int64
		propCRC              uint32
	}{
		{"no attributes", false, false, 32, 0x67a372c0, 32, 0x58fe37c2},
		{"node attributes only", true, false, 6037, 0x87b0a745, 12537, 0xa924fd4a},
		{"node and edge attributes", true, true, 13242, 0x4e4ec8cf, 22442, 0x2fbe205d},
		{"edge attributes only", false, true, 7237, 0xbe2c9127, 9937, 0xd6942f7a},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := mk(c.nodeAttrs, c.edgeAttrs)
			var bl, bp bytes.Buffer
			szL, crcL, err := snapshot.WriteLabels(&bl, g)
			if err != nil {
				t.Fatalf("WriteLabels: %v", err)
			}
			szP, crcP, err := snapshot.WriteProperties(&bp, g)
			if err != nil {
				t.Fatalf("WriteProperties: %v", err)
			}
			if szL != c.labelSize || crcL != c.labelCRC {
				t.Errorf("labels.bin changed: %d B crc %08x, want %d B crc %08x",
					szL, crcL, c.labelSize, c.labelCRC)
			}
			if szP != c.propSize || crcP != c.propCRC {
				t.Errorf("properties.bin changed: %d B crc %08x, want %d B crc %08x",
					szP, crcP, c.propSize, c.propCRC)
			}
		})
	}
}
