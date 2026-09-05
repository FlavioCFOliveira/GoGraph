package sim

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// mvcc_sessions_parallel_edge_test.go — the regression gate for rmp #2695.
//
// The multi-session generator drew the endpoints of `CREATE (a)-[:KNOWS]->(b)`
// while consulting only the COMMITTED model and the DRAWING session's own
// uncommitted workspace. It could therefore draw a pair another session had
// already created in a still-open transaction: that edge is physically in the
// shared adjacency, the engine's parallel-edge guard is snapshot-blind, and the
// CREATE came back as cypher.ErrParallelEdgeInSimpleGraph. The harness concedes
// only mvcc.ErrSerializationConflict, so that error aborts the whole run.
// Measured at Ticks=60, Sessions=4, it aborted 2 of the first 300 seeds (102
// and 215), which makes the mode an instrument that fails on 0.7% of the seeds
// it is asked to measure.
//
// The two gates below are the property, not the symptom: a seed sweep in which
// EVERY run completes, and a non-vacuity floor proving the runs still create
// KNOWS edges — an exclusion so broad that it stopped generating relationships
// altogether would satisfy "no error" and prove nothing.
//
// Both seeds named in the task are inside the short sweep's range, so the gate
// carries its own reproducers.

// mvccParallelEdgeConfig is the exact shape the defect was measured in: small
// enough that a 300-seed sweep stays inside the short layer, large enough that
// transactions overlap and the collision is reachable.
func mvccParallelEdgeConfig(seed uint64) MVCCSessionsConfig {
	return MVCCSessionsConfig{Seed: seed, Ticks: 60, Sessions: 4}
}

// mvccParallelEdgeShortSeeds is the seed range the short layer sweeps. It ends
// past 215, so the two known reproducers (102 and 215) are both inside it.
const mvccParallelEdgeShortSeeds = 300

// mvccParallelEdgeSoakSeeds is the range the soak layer sweeps — the full
// 0..999 the acceptance criteria name.
const mvccParallelEdgeSoakSeeds = 1000

// sweepMVCCSessions runs the mode over seeds [0, n) and fails on the first seed
// that does not complete cleanly. It returns the total number of KNOWS edges
// the committed models held at the end of each run, and how many seeds held at
// least one, so the caller can assert the sweep was not vacuous.
func sweepMVCCSessions(t *testing.T, n uint64) (totalEdges, seedsWithEdges int) {
	t.Helper()
	ctx := context.Background()
	for seed := uint64(0); seed < n; seed++ {
		res, err := RunMVCCSessions(ctx, mvccParallelEdgeConfig(seed))
		if err != nil {
			// A hard fault here is the defect: the generator asked the engine
			// for something its own graph forbids, so the instrument aborted
			// instead of producing a measurement.
			t.Fatalf("seed %d aborted: %v", seed, err)
		}
		if !res.Clean() {
			t.Fatalf("seed %d unclean: violations=%v foldErrors=%v", seed, res.Violations, res.FoldErrors)
		}
		totalEdges += res.KnowsEdges
		if res.KnowsEdges > 0 {
			seedsWithEdges++
		}
	}
	return totalEdges, seedsWithEdges
}

// TestMVCCSessions_NoParallelEdgeAbort_SeedSweep is the short-layer gate: every
// seed in [0, 300) must complete. On the pre-fix generator seeds 102 and 215
// abort with ErrParallelEdgeInSimpleGraph, so this test fails there and passes
// here.
//
// The edge floor is structural, never proportional: the sweep must have
// committed AT LEAST ONE KNOWS edge. A fix that silenced the collision by
// never drawing an edge again would pass the no-error clause and fail this one.
func TestMVCCSessions_NoParallelEdgeAbort_SeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	totalEdges, seedsWithEdges := sweepMVCCSessions(t, mvccParallelEdgeShortSeeds)
	if totalEdges == 0 {
		t.Fatalf("vacuous sweep: %d seeds committed zero KNOWS edges between them — "+
			"the mode is no longer exercising the relationship-write path",
			mvccParallelEdgeShortSeeds)
	}
	t.Logf("seeds=%d committed KNOWS edges=%d seeds with >=1 edge=%d",
		mvccParallelEdgeShortSeeds, totalEdges, seedsWithEdges)
}

// TestMVCCSessions_NoParallelEdgeAbort_KnownSeeds pins the two seeds the defect
// was reported on. Seed 102 collided on an ordinary pair (session 1 drew a pair
// session 2 held uncommitted); seed 215 collided on a SELF pair (session 2 drew
// the self-loop session 3 held uncommitted). The self-ness is incidental: a
// BARE self-loop is legal on a simple graph, and the mode still draws them —
// only a DUPLICATE, self or not, is refused.
//
// Each seed must also still commit a KNOWS edge, so the fix cannot pass by
// having turned these seeds' edge draws into reads.
func TestMVCCSessions_NoParallelEdgeAbort_KnownSeeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	for _, seed := range []uint64{102, 215} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			res, err := RunMVCCSessions(ctx, mvccParallelEdgeConfig(seed))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !res.Clean() {
				t.Fatalf("violations=%v foldErrors=%v", res.Violations, res.FoldErrors)
			}
			if res.KnowsEdges < 1 {
				t.Fatalf("no KNOWS edge committed: %+v", res)
			}
		})
	}
}

// TestMVCCSessions_NoParallelEdgeAbort_SoakSweep is the full range the
// acceptance criteria name — seeds 0..999 — kept in the soak layer because
// `internal/sim` is already the module's heaviest short-layer package and the
// short sweep above holds both known reproducers.
func TestMVCCSessions_NoParallelEdgeAbort_SoakSweep(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	totalEdges, seedsWithEdges := sweepMVCCSessions(t, mvccParallelEdgeSoakSeeds)
	if totalEdges == 0 {
		t.Fatalf("vacuous sweep: %d seeds committed zero KNOWS edges between them",
			mvccParallelEdgeSoakSeeds)
	}
	t.Logf("seeds=%d committed KNOWS edges=%d seeds with >=1 edge=%d",
		mvccParallelEdgeSoakSeeds, totalEdges, seedsWithEdges)
}
