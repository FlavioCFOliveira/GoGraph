//go:build soak

package lpg

// mvcc_vacuum_soak_test.go — MVCC C2 (rmp #2308) acceptance criterion 3, at the
// soak layer: version memory shows no unbounded growth under a sustained churn
// workload with a long-lived reader present.
//
// # Why the short layer is not enough for this claim
//
// The short-layer gates assert the bound over tens of thousands of modifications,
// which is enough to catch a driver that never runs. It is not enough to catch a
// SLOW leak — a store whose reclaimer misses a case, a wake that is dropped one
// time in a thousand, a goroutine that restarts more often than it exits. Those
// show up as a trend, and a trend needs minutes.
//
// # What is measured
//
// Four quantities sampled across the run, each with a distinct failure mode:
//
//   - retained records, which must not TREND upwards between reader generations;
//   - the vacuum's start/exit balance, which must stay within one — a growing gap
//     is a goroutine leak, and a start count growing far faster than the pass
//     count is the restart storm the wake-drain fix closed;
//   - the horizon's unregistered-reader count, which must stay at zero, because
//     while it is non-zero nothing is reclaimed at all and every other number
//     here becomes meaningless;
//   - the settled total after the last reader leaves, which must reach the churn
//     bound.
//
// Layer: soak. Activate with `-tags=soak` or SOAK_FULL=1.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestVacuumSoak_NoUnboundedGrowthUnderSustainedChurn drives generations of churn,
// each overlapped by a long-lived reader, and asserts the substrate does not trend
// upwards across them.
//
// The reader is REPLACED each generation rather than held for the whole run: a
// reader held throughout would legitimately pin everything and the test would
// measure nothing but its own premise. Replacing it means each generation's
// garbage becomes reclaimable exactly once, so a reclaimer that misses a case
// leaves a residue that accumulates generation over generation — which is the
// trend this looks for.
func TestVacuumSoak_NoUnboundedGrowthUnderSustainedChurn(t *testing.T) {
	testlayers.RequireSoak(t)

	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() {
		if err := g.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	const nodes = 4096
	keys := make([]string, nodes)
	for i := range keys {
		keys[i] = nodeKey(i)
		if err := g.AddNode(keys[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}

	// Sized for the soak layer: 300 generations of 64 thresholds each is 78 643 200
	// modifications, measured at 52 s on a 10-core M4 (78 651 392 records reclaimed
	// across 12 570 passes, 300 of them hitting the per-pass bound, and every
	// generation settling to zero). The first calibration ran 60 generations of 4
	// thresholds and finished in 0.55 s — enough to prove the bound, far too little
	// to expose a trend.
	const generations = 300
	const perGeneration = reclaimThreshold * 64
	settled := make([]int64, 0, generations)

	for gen := 0; gen < generations; gen++ {
		// A reader that spans the whole generation's churn, then leaves.
		snap := g.BeginRead()
		for i := 0; i < perGeneration; i++ {
			k := keys[i%nodes]
			if err := g.ApplyAtomically(func() error {
				if err := g.SetNodeProperty(k, "w", Int64Value(int64(gen*perGeneration+i))); err != nil {
					return err
				}
				return g.SetNodeLabel(k, "L")
			}); err != nil {
				g.EndRead(snap)
				t.Fatalf("generation %d write %d: %v", gen, i, err)
			}
			// Sampled, not per write: MVCCStats scans the horizon's slots, and
			// calling it on every modification made the instrument a bigger cost
			// than the workload it measures.
			if i%4096 == 0 {
				if s := g.MVCCStats(); s.UnregisteredSnapshots != 0 {
					g.EndRead(snap)
					t.Fatalf("generation %d: %d readers failed to register, so reclamation is "+
						"suspended and every other measurement here is meaningless",
						gen, s.UnregisteredSnapshots)
				}
			}
		}
		g.EndRead(snap)

		// With the generation's reader gone and nothing further written, the
		// substrate must come back to the churn bound.
		deadline := time.Now().Add(30 * time.Second)
		for {
			s := g.MVCCStats()
			if s.WithinBound() {
				settled = append(settled, s.Total)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("generation %d did not settle: %d records held against a bound of %d "+
					"(ceiling %d), %d active readers, oldest reader age %d; vacuum %+v",
					gen, s.Total, s.Bound, s.Ceiling, s.ActiveSnapshots, s.OldestSnapshotAge(),
					g.VacuumStats())
			}
			time.Sleep(time.Millisecond)
		}

		// The vacuum's lifecycle must stay balanced, and its starts must not
		// outrun its passes — the signature of the restart storm a wake left
		// undrained produced (24 starts for 16 000 writes, one always alive).
		vs := g.VacuumStats()
		if vs.Starts > vs.Exits+1 {
			t.Fatalf("generation %d: %d vacuum starts against %d exits — goroutines are leaking",
				gen, vs.Starts, vs.Exits)
		}
		if vs.Passes > 0 && vs.Starts > vs.Passes {
			t.Fatalf("generation %d: %d vacuum starts for only %d passes — the sweeper is "+
				"restarting rather than working", gen, vs.Starts, vs.Passes)
		}
	}

	// THE TREND. The last quarter of the generations must not hold materially more
	// than the first quarter: a per-generation residue would compound.
	q := len(settled) / 4
	if q == 0 {
		t.Fatal("too few generations to establish a trend")
	}
	mean := func(xs []int64) float64 {
		var sum int64
		for _, x := range xs {
			sum += x
		}
		return float64(sum) / float64(len(xs))
	}
	first, last := mean(settled[:q]), mean(settled[len(settled)-q:])
	// An absolute floor as well as a ratio, so a first quarter that settled to
	// zero cannot make any later value an infinite regression.
	if last > first+float64(reclaimThreshold) {
		t.Errorf("settled version memory trended upwards across %d generations: first quarter "+
			"mean %.0f, last quarter mean %.0f (bound %d) — a per-generation residue is "+
			"accumulating; all samples: %v", generations, first, last, reclaimThreshold, settled)
	}
	t.Logf("settled means over %d generations: first quarter %.0f, last quarter %.0f (bound %d); "+
		"vacuum %+v", generations, first, last, reclaimThreshold, g.VacuumStats())
}

// nodeKey names the soak workload's nodes without pulling fmt into the hot loop.
func nodeKey(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "n0"
	}
	buf := make([]byte, 0, 8)
	for i > 0 {
		buf = append(buf, digits[i%10])
		i /= 10
	}
	out := make([]byte, 0, len(buf)+1)
	out = append(out, 'n')
	for j := len(buf) - 1; j >= 0; j-- {
		out = append(out, buf[j])
	}
	return string(out)
}
