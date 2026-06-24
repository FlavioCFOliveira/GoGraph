package sim

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestEulerChecks_CleanOnFixtures asserts the EULER battery reports no violation
// on a correct engine across a spread of ticks. Each tick derives an independent
// Seed (so the seed-derived fixtures differ run to run) yet the engine is sound,
// so a clean result is the contract.
func TestEulerChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 1, 2, 7, 42, 1000, 99991} {
		if vs := eulerViolations(tick); vs != nil {
			t.Fatalf("eulerViolations(%d) = %v, want nil (engine is correct)", tick, vs)
		}
	}
}

// TestEulerChecks_Deterministic asserts the battery is a pure function of the
// tick: the same tick yields an identical fixture set and identical (empty)
// result, which is the property the whole DST harness relies on for replay.
func TestEulerChecks_Deterministic(t *testing.T) {
	t.Parallel()
	const tick = int64(123456)

	a := eulerFixtures(NewSeed(uint64(tick) ^ eulerSeedSalt))
	b := eulerFixtures(NewSeed(uint64(tick) ^ eulerSeedSalt))
	if len(a) != len(b) {
		t.Fatalf("fixture count differs across identical seeds: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].name != b[i].name || a[i].order != b[i].order ||
			a[i].circuit != b[i].circuit || len(a[i].edges) != len(b[i].edges) {
			t.Fatalf("fixture %d differs across identical seeds: %+v vs %+v", i, a[i], b[i])
		}
		for j := range a[i].edges {
			if a[i].edges[j] != b[i].edges[j] {
				t.Fatalf("fixture %q edge %d differs across identical seeds: %v vs %v",
					a[i].name, j, a[i].edges[j], b[i].edges[j])
			}
		}
	}
}

// TestEulerValidateTrail_AcceptsKnownGood proves the independent trail validator
// accepts a genuine Eulerian circuit. The fixture is the directed cycle
// 0->1->2->0; the trail [0 1 2 0] is the unique circuit up to rotation, so a
// correct validator must pass it.
func TestEulerValidateTrail_AcceptsKnownGood(t *testing.T) {
	t.Parallel()
	f := eulerSingleCycle("cycle3", 3)
	good := []graph.NodeID{0, 1, 2, 0}
	if vs := eulerValidateTrail(1, f, good); vs != nil {
		t.Fatalf("validator rejected a known-good Eulerian circuit: %v", vs)
	}
}

// TestEulerValidateTrail_RejectsTampered proves the validator REJECTS trails
// that break the Eulerian invariant in each distinct way. Every case must yield
// exactly one ViolationSearchDivergence tagged Op "search:Hierholzer"; without
// these rejections the clean-run test would be vacuous.
func TestEulerValidateTrail_RejectsTampered(t *testing.T) {
	t.Parallel()
	// Reference fixture: figure-eight 0->1->2->0 + 0->3->4->0 (E=6, circuit).
	f := eulerFigureEight("figure8")
	// A valid circuit for f, as a control: hub-reentering traversal.
	valid := []graph.NodeID{0, 1, 2, 0, 3, 4, 0}
	if vs := eulerValidateTrail(0, f, valid); vs != nil {
		t.Fatalf("control: validator rejected a valid circuit for the reference fixture: %v", vs)
	}

	tests := []struct {
		name  string
		trail []graph.NodeID
	}{
		{
			// Wrong length: drops the final hub return, length E (=6) not E+1 (=7).
			name:  "too short",
			trail: []graph.NodeID{0, 1, 2, 0, 3, 4},
		},
		{
			// Wrong length: an extra step, length E+2.
			name:  "too long",
			trail: []graph.NodeID{0, 1, 2, 0, 3, 4, 0, 0},
		},
		{
			// Right length, but an edge is REUSED (0->1 twice) while 0->3 is
			// never used: the multiset of consecutive pairs no longer matches
			// the graph's edge multiset.
			name:  "edge reused",
			trail: []graph.NodeID{0, 1, 2, 0, 1, 4, 0},
		},
		{
			// Right length, but a consecutive pair (2->4) is not a real edge of
			// the graph at all.
			name:  "non-edge step",
			trail: []graph.NodeID{0, 1, 2, 4, 3, 4, 0},
		},
		{
			// Out-of-range NodeID 9 (order is 5).
			name:  "out of range node",
			trail: []graph.NodeID{0, 1, 2, 0, 3, 9, 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs := eulerValidateTrail(5, f, tc.trail)
			if len(vs) != 1 {
				t.Fatalf("tampered trail %q: got %d violations, want exactly 1: %v", tc.name, len(vs), vs)
			}
			if vs[0].Kind != ViolationSearchDivergence {
				t.Errorf("tampered trail %q: Kind = %q, want %q", tc.name, vs[0].Kind, ViolationSearchDivergence)
			}
			if vs[0].Op != "search:Hierholzer" {
				t.Errorf("tampered trail %q: Op = %q, want %q", tc.name, vs[0].Op, "search:Hierholzer")
			}
			if vs[0].Tick != 5 {
				t.Errorf("tampered trail %q: Tick = %d, want 5", tc.name, vs[0].Tick)
			}
		})
	}
}

// TestEulerValidateTrail_OpenCircuitRejected proves that a graph declared as a
// circuit whose trail is NOT closed (first != last) is rejected. This exercises
// the dedicated circuit-closure branch independently of the edge-multiset check:
// the trail below uses every edge of the path 0->1->2 exactly once and is the
// right length, so only the circuit flag makes it invalid.
func TestEulerValidateTrail_OpenCircuitRejected(t *testing.T) {
	t.Parallel()
	// A path 0->1->2 carried in a fixture flagged (incorrectly) as a circuit.
	f := eulerFixture{
		name:    "path-flagged-circuit",
		order:   3,
		edges:   []eulerEdge{{Src: 0, Dst: 1}, {Src: 1, Dst: 2}},
		circuit: true,
	}
	open := []graph.NodeID{0, 1, 2} // length E+1 = 3, every edge used once, but 0 != 2
	vs := eulerValidateTrail(3, f, open)
	if len(vs) != 1 {
		t.Fatalf("open circuit: got %d violations, want exactly 1: %v", len(vs), vs)
	}
	if vs[0].Kind != ViolationSearchDivergence || vs[0].Op != "search:Hierholzer" {
		t.Errorf("open circuit: violation = %+v, want SearchDivergence / search:Hierholzer", vs[0])
	}
}

// TestEulerNonEulerian_YieldsErrNoEulerian exercises the precondition branch of
// the checker directly: a hand-built non-Eulerian graph must drive Hierholzer to
// return search.ErrNoEulerian, and the checker must therefore report no
// violation. It also confirms the engine's contract first-hand (independent of
// the checker), so a regression in either is localised.
func TestEulerNonEulerian_YieldsErrNoEulerian(t *testing.T) {
	t.Parallel()
	for _, f := range []eulerFixture{
		eulerImbalanced("imbalanced-degree-gap"),
		eulerTwoDisconnectedCycles("two-disconnected-cycles"),
		eulerThreeImbalancedVertices("three-odd-balance-vertices"),
	} {
		t.Run(f.name, func(t *testing.T) {
			// Engine contract, checked directly.
			c := eulerBuildCSR(f)
			if _, err := search.Hierholzer(c); !errors.Is(err, search.ErrNoEulerian) {
				t.Fatalf("Hierholzer on non-Eulerian %q: err = %v, want ErrNoEulerian", f.name, err)
			}
			// Checker contract: a non-Eulerian fixture (circuit=false) that
			// correctly errors yields no violation. Build a one-fixture battery
			// inline so the assertion is scoped to this single shape.
			vs := eulerCheckOne(0, f)
			if vs != nil {
				t.Fatalf("checker reported a violation on a correctly non-Eulerian graph %q: %v", f.name, vs)
			}
		})
	}
}

// TestEulerCheckOne_FlagsFalseNegative proves the checker FAILS when the engine
// wrongly accepts a non-Eulerian graph. We cannot make the real engine
// misbehave, so we assert the converse the checker guards: a fixture whose
// circuit flag is (incorrectly) true but which is genuinely non-Eulerian makes
// the checker demand a circuit, and the engine's ErrNoEulerian then surfaces as
// a divergence. This guarantees the non-Eulerian branch is not vacuous.
func TestEulerCheckOne_FlagsFalseNegative(t *testing.T) {
	t.Parallel()
	bad := eulerImbalanced("imbalanced")
	bad.circuit = true // lie: claim it is Eulerian
	vs := eulerCheckOne(9, bad)
	if len(vs) != 1 {
		t.Fatalf("expected exactly 1 violation when a non-Eulerian graph is asserted Eulerian, got %d: %v", len(vs), vs)
	}
	if vs[0].Kind != ViolationSearchDivergence || vs[0].Op != "search:Hierholzer" || vs[0].Tick != 9 {
		t.Errorf("violation = %+v, want SearchDivergence / search:Hierholzer / tick 9", vs[0])
	}
}

// eulerCheckOne runs the full checker contract (build CSR, call Hierholzer,
// dispatch on the circuit flag to either validate the trail or require
// ErrNoEulerian) against a single fixture. It mirrors eulerViolations' per-fixture
// body exactly, so the tests exercise the same decision logic the battery uses.
func eulerCheckOne(tick int64, f eulerFixture) []Violation {
	c := eulerBuildCSR(f)
	trail, err := search.Hierholzer(c)
	if f.circuit {
		if err != nil {
			return eulerDiverge(tick, f.name+": expected circuit, got error")
		}
		return eulerValidateTrail(tick, f, trail)
	}
	if !errors.Is(err, search.ErrNoEulerian) {
		return eulerDiverge(tick, f.name+": expected ErrNoEulerian")
	}
	return nil
}
