package sim

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// engineDeviationKinds is the family that means THE ENGINE MISBEHAVED. A gate
// reporting that a run never exercised its subject must not use any of them.
var engineDeviationKinds = []ViolationKind{
	ViolationACIDAtomicity,
	ViolationACIDConsistency,
	ViolationACIDIsolation,
	ViolationACIDDurability,
	ViolationGraphIntegrity,
	ViolationOracleDeviation,
	ViolationSearchDivergence,
}

// TestVacuousRunIsNotAnEngineDeviation is the whole point of rmp #2614, asserted
// on a deliberately vacuous run of a real gate.
//
// The gates run on an EMPTY graph, so every comparison they make is between
// empty sets: they agree, and they agree for the worst possible reason. That is
// the shape that used to be reported as ORACLE_DEVIATION — a kind that says the
// engine deviated from its oracle, when the engine was never driven at all.
//
// Measured cost of the old kind, 2026-08-25: 37 bolt-decode-swarm failures were
// triaged toward the engine when the cause was a co-resident cpu-starvation
// scenario clamping GOMAXPROCS (rmp #2613). The clause text was honest; the KIND
// was not, and the kind is what an operator reads first.
//
// The assertion is the DISTINCTION, not merely the value: it rejects every
// member of [engineDeviationKinds], so adding a new engine kind and pointing a
// vacuity gate at it fails here too.
func TestVacuousRunIsNotAnEngineDeviation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fire func(t *testing.T) []Violation
	}{
		{
			name: "IndexSeekResults on an empty graph",
			fire: func(t *testing.T) []Violation {
				t.Helper()
				g := lpg.New[string, float64](adjlist.Config{Directed: true})
				empty := NewEngineAdapter(cypher.NewEngine(g))
				k := fixedSeekResults()
				if v := k.Check(1, empty); len(v) != 0 {
					t.Fatalf("precondition: an empty-graph check must be clean, got:\n%v", v)
				}
				return k.Finish(2)
			},
		},
		{
			name: "MVCC substrate never sampled",
			fire: func(t *testing.T) []Violation {
				t.Helper()
				return checkMVCCSubstrateNonVacuity(1, &mvccSubstrateEvidence{label: "unsampled"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := tc.fire(t)
			if len(v) == 0 {
				t.Fatalf("the gate did not fire on a deliberately vacuous run, so this test " +
					"asserts nothing about the kind it would have used")
			}
			for _, got := range v {
				for _, bad := range engineDeviationKinds {
					if got.Kind == bad {
						t.Errorf("a run that never exercised its subject is reported as %s, which "+
							"names an engine-versus-oracle disagreement and points triage at the "+
							"engine (rmp #2614). Want %s. Message: %s",
							got.Kind, ViolationVacuousRun, got.Message)
					}
				}
				if got.Kind != ViolationVacuousRun {
					t.Errorf("violation kind = %s, want %s: %s",
						got.Kind, ViolationVacuousRun, got.Message)
				}
			}
		})
	}
}

// TestViolationKindFamiliesAreDisjoint pins the taxonomy itself: VACUOUS_RUN must
// not collide with any engine kind's string, because the coverage tracker buckets
// runs by that string and two kinds sharing one would silently merge the two
// families back together in every swarm summary.
func TestViolationKindFamiliesAreDisjoint(t *testing.T) {
	t.Parallel()
	for _, k := range engineDeviationKinds {
		if k == ViolationVacuousRun {
			t.Errorf("%s appears in BOTH families", k)
		}
		if strings.EqualFold(string(k), string(ViolationVacuousRun)) {
			t.Errorf("%q and %q differ only by case, so the coverage tracker would bucket "+
				"them apart while a reader could not tell them apart", k, ViolationVacuousRun)
		}
	}
	if string(ViolationVacuousRun) != "VACUOUS_RUN" {
		t.Errorf("ViolationVacuousRun = %q; the wire name is what a swarm summary prints and "+
			"what docs/dst.md documents", ViolationVacuousRun)
	}
}
