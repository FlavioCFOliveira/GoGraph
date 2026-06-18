package lpg

import (
	"runtime"
	"testing"
)

// propbag_mem_test.go — the resident-heap regression guard for the per-node
// property-bag tier (sprint 207, #1587), mirroring graph/index's
// nodeset_mem_test.go.
//
// It measures LIVE heap bytes per node-property entry for the common
// small-property-set shape — 1M nodes each carrying TWO properties — two ways:
// the OLD nested map[PropertyKeyID]PropertyValue representation (the audited
// ~330 B/node baseline, dominated by the per-node inner Go map) and the NEW
// by-value propBag (sorted-free small-slice tier). The probe pins the
// populated structure across two GC cycles so HeapAlloc reflects only retained
// memory, then divides by the node count.
//
// These tests are NOT parallel: they read process-global runtime.MemStats. The
// numbers are coarse (resident heap is noisy), so the assertions use wide
// margins — they guard against an order-of-magnitude regression, not single-
// byte drift.

const memProbeNodes = 1_000_000

// twoProps is the property pair every probe node carries: a string id and a
// string name, matching the example-26 SetNodeProperty consumer pattern.
var twoProps = [2]PropertyValue{StringValue("0123456789abcdef01234567"), StringValue("Olivia Smith")}

// liveHeapBytes returns HeapAlloc after two GC cycles, with keep referenced so
// the populated structure cannot be collected before the measurement.
func liveHeapBytes(keep any) uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	runtime.KeepAlive(keep)
	return ms.HeapAlloc
}

// bytesPerNode measures the retained heap delta of building want via build,
// divided by memProbeNodes.
func bytesPerNode(t *testing.T, build func() any) float64 {
	t.Helper()
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	obj := build()
	after := liveHeapBytes(obj)

	delta := float64(after) - float64(before.HeapAlloc)
	if delta < 0 {
		delta = 0
	}
	return delta / float64(memProbeNodes)
}

// oldNestedMapBytesPerNode builds the audited baseline shape — 1M nodes, each a
// distinct inner map[PropertyKeyID]PropertyValue of two entries — and returns
// its retained bytes per node. This is the representation the propBag replaces.
func oldNestedMapBytesPerNode(t *testing.T) float64 {
	t.Helper()
	return bytesPerNode(t, func() any {
		m := make(map[uint64]map[PropertyKeyID]PropertyValue, memProbeNodes)
		for i := uint64(0); i < memProbeNodes; i++ {
			inner := make(map[PropertyKeyID]PropertyValue, 2)
			inner[0] = twoProps[0]
			inner[1] = twoProps[1]
			m[i] = inner
		}
		return m
	})
}

// newPropBagBytesPerNode builds 1M nodes each holding two properties in a
// by-value propBag and returns its retained bytes per node.
func newPropBagBytesPerNode(t *testing.T) float64 {
	t.Helper()
	return bytesPerNode(t, func() any {
		m := make(map[uint64]propBag, memProbeNodes)
		for i := uint64(0); i < memProbeNodes; i++ {
			var b propBag
			b.set(0, twoProps[0])
			b.set(1, twoProps[1])
			m[i] = b
		}
		return m
	})
}

// TestMem_OldNestedMapBaseline records the audited baseline as the control,
// asserting only that it is large so the propBag guard can compare against it.
func TestMem_OldNestedMapBaseline(t *testing.T) {
	perNode := oldNestedMapBytesPerNode(t)
	t.Logf("OLD nested map per 2-property node: %.1f B", perNode)
	if perNode < 150 {
		t.Fatalf("baseline unexpectedly small (%.1f B/node) — control invalid", perNode)
	}
}

// TestMem_NewPropBagTwoProps is the regression guard for the by-value propBag.
// A 2-property node must retain a clear, multiple-fold-smaller footprint than
// the nested-map baseline (the inner-map allocation is eliminated, replaced by
// one small slice backing). The threshold is deliberately conservative: the
// change targets a >= 2x reduction for the common small case.
func TestMem_NewPropBagTwoProps(t *testing.T) {
	baseline := oldNestedMapBytesPerNode(t)
	perNode := newPropBagBytesPerNode(t)
	t.Logf("NEW propBag per 2-property node: %.1f B (baseline %.1f B, ratio %.2f)",
		perNode, baseline, perNode/baseline)

	// Hard upper bound independent of the baseline: a 2-property bag is one
	// outer-map slot plus a 2-element kv backing (2 x (uint32 + 24-byte value),
	// padded to 32 B) ~= 64 B backing + the by-value bag header in the slot,
	// PLUS the two string values themselves (each StringValue boxes a string in
	// the PropertyValue.v `any`, ~40 B incl. header + data — the residual that
	// the deferred scalar/string de-boxing lever would attack). ~200 B/node
	// leaves slack for that boxing, map load-factor, and allocator rounding
	// while still catching a per-node inner-map regression (the baseline is
	// ~370 B and an inner map alone is ~300 B).
	const maxBytesPerNode = 200
	if perNode > maxBytesPerNode {
		t.Fatalf("propBag 2-property node = %.1f B/node, want <= %d B; the small "+
			"tier is not eliminating the per-node inner-map allocation", perNode, maxBytesPerNode)
	}

	// Relative guard: at least a 2x reduction versus the nested-map baseline.
	const maxFractionOfBaseline = 0.5
	if perNode > maxFractionOfBaseline*baseline {
		t.Fatalf("propBag 2-property node = %.1f B/node is %.0f%% of the %.1f B "+
			"nested-map baseline, want <= %.0f%%; the small tier did not deliver "+
			"the expected reduction", perNode, 100*perNode/baseline, baseline, 100*maxFractionOfBaseline)
	}
}
