package sim

// wire_param_isolation_test.go — regression gate for rmp #2728.
//
// The Bolt parameter-matrix probe ran on a fixture with a FIXED label
// ("WireParam") and a FIXED id ("wp-1"). Several callers drive ONE shared
// [SimServer] concurrently — bench/contention's dst-concurrent-bolt runs `level`
// goroutines, each calling [RunConcurrent], and every call probes before it
// spawns any connection — so many nodes matching `MATCH (n:WireParam ...)` were
// live at once and each probe's SET and DETACH DELETE fanned out over every
// other probe's node.
//
// Two things broke, and this file gates both:
//
//   - WORK. Measured on the defective fixture, 64 concurrent probes over 1984
//     operations against one shared server issued 2,028,689 graph/lpg
//     delNodePropertyInfo calls — 1022.5 per operation, against 6.0 at one probe
//     at a time — of which 97.9% removed nothing. The published
//     dst-concurrent-bolt@64 scaling of 0.470 was that amplification, not engine
//     write scaling.
//
//   - TRUTH. The probe's own `count(*) == 1` and `count(*) == 0` assertions are
//     false when another probe's node is live, so the probe reported thousands
//     of divergences that were artefacts of its own fixture. That is what
//     [TestWireParamTypes_ConcurrentProbesDoNotCrossTalk] detects, and it is the
//     stronger gate of the two: it needs no engine instrumentation and it fails
//     hard on the defective fixture (measured: 1,916 spurious failures at two
//     concurrent probes, 13,888 at sixty-four).

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// wireParamIsolationProbes is the number of concurrent probe goroutines, and
// wireParamIsolationRounds how many probes each one runs in sequence.
//
// Sized to the base rate of the event observed, not for spectacle: on the
// defective fixture two concurrent probes already produced spurious failures, so
// sixteen makes the detection overwhelming while keeping the test inside the
// short layer's budget.
const (
	wireParamIsolationProbes = 16
	wireParamIsolationRounds = 4
)

// TestWireParamTypes_ConcurrentProbesDoNotCrossTalk runs many probes at once
// against ONE shared server and requires every one of them to be clean.
//
// This is the regression gate for rmp #2728. It FAILS on the fixed-label,
// fixed-id fixture — each probe then sees its neighbours' nodes through
// `MATCH (n:WireParam {f:$f})` and `MATCH (n:WireParam)` — and passes only while
// each probe's fixture is private to it.
//
// It also re-asserts population neutrality UNDER CONCURRENCY. Neutrality alone
// was never the missing property: the defective probe was neutral too, because
// its DETACH DELETE removed everyone's nodes rather than none.
func TestWireParamTypes_ConcurrentProbesDoNotCrossTalk(t *testing.T) {
	t.Parallel()
	srv := newWireRoundTripServer(t)

	before, err := queryNodeCount(srv)
	if err != nil {
		t.Fatalf("node count before: %v", err)
	}

	ctx := context.Background()
	fails := make([][]string, wireParamIsolationProbes)
	var wg sync.WaitGroup
	for i := 0; i < wireParamIsolationProbes; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for r := 0; r < wireParamIsolationRounds; r++ {
				fails[slot] = append(fails[slot], probeWireParamTypes(ctx, srv)...)
			}
		}(i)
	}
	wg.Wait()

	total := 0
	for i, f := range fails {
		total += len(f)
		if len(f) > 0 {
			t.Errorf("probe goroutine %d reported %d divergence(s):\n%s",
				i, len(f), strings.Join(f, "\n"))
		}
	}
	if total > 0 {
		t.Fatalf("%d concurrent probes x %d rounds produced %d divergence(s); a probe's "+
			"fixture is not private to it (rmp #2728)",
			wireParamIsolationProbes, wireParamIsolationRounds, total)
	}

	after, err := queryNodeCount(srv)
	if err != nil {
		t.Fatalf("node count after: %v", err)
	}
	if before != after {
		t.Fatalf("concurrent probes changed the population: %d node(s) before, %d after",
			before, after)
	}
}

// TestWireParamFixture_IsPrivateWhileHeld proves the slot pool's whole contract:
// slots held simultaneously are distinct, and a released slot is reused so the
// number of distinct labels the engine ever registers is bounded by peak
// concurrency rather than by the number of probes run.
//
// It exercises a private pool, not the package-level [wireParamSlots], so it
// neither depends on nor disturbs whatever else is probing concurrently.
func TestWireParamFixture_IsPrivateWhileHeld(t *testing.T) {
	t.Parallel()
	var pool wireParamSlotPool

	const held = 8
	slots := make([]int, held)
	seen := make(map[int]bool, held)
	for i := range slots {
		slots[i] = pool.acquire()
		if seen[slots[i]] {
			t.Fatalf("acquire returned slot %d twice while %d slot(s) were still held", slots[i], i)
		}
		seen[slots[i]] = true
	}

	for _, s := range slots {
		pool.release(s)
	}

	// Every slot is now free, so acquiring the same number again must not mint a
	// single new one: recycling is what bounds the label cardinality.
	for i := 0; i < held; i++ {
		s := pool.acquire()
		if !seen[s] {
			t.Fatalf("acquire minted a new slot %d after every slot was released; "+
				"the pool is not recycling and label cardinality is unbounded", s)
		}
	}
}

// TestWireParamFixture_IdentifiersCarryTheSlot pins the property the whole fix
// rests on: two fixtures held at the same time share NEITHER label NOR id.
//
// It is deliberately a separate, concurrency-free assertion. A refactor that
// reverted either identifier to a constant would still pass a lightly loaded
// concurrency test by luck; it cannot pass this one at all.
func TestWireParamFixture_IdentifiersCarryTheSlot(t *testing.T) {
	t.Parallel()

	a := newWireParamFixture()
	b := newWireParamFixture()
	defer wireParamSlots.release(a.slot)
	defer wireParamSlots.release(b.slot)

	if a.label == b.label {
		t.Errorf("two simultaneously held fixtures share the label %q (rmp #2728)", a.label)
	}
	if a.id == b.id {
		t.Errorf("two simultaneously held fixtures share the id %q (rmp #2728)", a.id)
	}
	if !strings.HasPrefix(a.label, wireParamLabelPrefix) {
		t.Errorf("fixture label %q does not carry the %q prefix, so a probe's nodes are no "+
			"longer distinguishable from a workload's", a.label, wireParamLabelPrefix)
	}
}
