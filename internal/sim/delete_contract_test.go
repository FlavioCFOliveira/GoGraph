package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildDeleteContractFixture creates a graph with BOTH degree classes in the
// engine and the oracle: Persons "conn" and "peer" joined by a KNOWS edge (so
// both have degree 1), and "lonely" with no edge at all. It is a directed
// multigraph, matching the scenario that hosts the probe.
func buildDeleteContractFixture(t *testing.T) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	a, o := NewEngineAdapter(cypher.NewEngine(g)), NewGraphOracle()
	ctx := context.Background()
	apply := func(query string, params map[string]any) {
		t.Helper()
		if _, err := a.RunWrite(ctx, query, params); err != nil {
			t.Fatalf("fixture write %q: %v", query, err)
		}
		o.ApplyCreate(query, params)
	}
	for i, name := range []string{"conn", "lonely", "peer"} {
		apply(tmplCreatePerson, map[string]any{"name": name, "age": int64(i)})
	}
	apply(tmplCreateKnows, map[string]any{"a": "conn", "b": "peer"})
	return a, o
}

// TestDeleteContract_BothArmsAdjudicated is the happy path: on a fixture holding
// a connected node and an isolated one, the probe must accept the isolated
// delete (counters adjudicated) and refuse the connected one with the typed
// error, reporting no violation and recording both arms.
func TestDeleteContract_BothArmsAdjudicated(t *testing.T) {
	a, o := buildDeleteContractFixture(t)
	var st deleteContractStats

	if v := probeDeleteContract(context.Background(), 0, o, a, &st); len(v) > 0 {
		t.Fatalf("delete-contract probe reported a violation on a conforming engine: %v", v)
	}
	if st.accepted != 1 {
		t.Errorf("accepted arm fired %d time(s), want 1", st.accepted)
	}
	if st.refused != 1 {
		t.Errorf("refused arm fired %d time(s), want 1", st.refused)
	}
	// The accepted delete must have advanced the model, the refused one must not.
	if _, still := o.byName["lonely"]; still {
		t.Error("the oracle still models the isolated node the engine deleted")
	}
	if _, gone := o.byName["conn"]; !gone {
		t.Error("the oracle dropped the connected node whose delete was REFUSED")
	}
	if v := deleteContractVacuity(0, &st); len(v) > 0 {
		t.Fatalf("vacuity gate fired although both arms ran: %v", v)
	}
}

// TestDeleteContract_TargetSelection pins the deterministic target choice: the
// lexicographically first connected and first isolated Person. A probe whose
// selection drifted would silently exercise a different pair each run.
func TestDeleteContract_TargetSelection(t *testing.T) {
	_, o := buildDeleteContractFixture(t)
	connected, isolated := deleteContractTargets(o)
	if connected != "conn" {
		t.Errorf("connected target = %q, want %q", connected, "conn")
	}
	if isolated != "lonely" {
		t.Errorf("isolated target = %q, want %q", isolated, "lonely")
	}
}

// TestDeleteContract_SensitivityToWrongDegree proves each arm FIRES when the
// oracle's degree disagrees with the engine's. The oracle adjacency is perturbed
// directly (the in-package test seam), so the probe's prediction is wrong while
// the engine behaves correctly — exactly the shape of an enforcement gap.
func TestDeleteContract_SensitivityToWrongDegree(t *testing.T) {
	t.Run("predicted accept, engine refuses", func(t *testing.T) {
		a, o := buildDeleteContractFixture(t)
		// Hide the KNOWS edge from the model: "conn" now looks isolated, so the
		// probe predicts a commit while the engine (which still has the edge)
		// refuses.
		delete(o.edges, edgeKey{src: o.byName["conn"], dst: o.byName["peer"], label: "KNOWS"})
		var st deleteContractStats
		v := probeDeleteContract(context.Background(), 0, o, a, &st)
		if len(v) == 0 {
			t.Fatal("probe FAILED to fire when the engine refused a delete the oracle predicted would commit")
		}
		if !strings.Contains(v[0].Message, "REFUSED") {
			t.Errorf("unexpected violation message: %s", v[0].Message)
		}
	})

	t.Run("predicted refuse, engine commits", func(t *testing.T) {
		// An EDGELESS engine paired with a model holding a phantom edge: the
		// probe picks the phantom-connected node for the reject arm and predicts
		// a refusal, but the engine — which really has no edge — deletes it.
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		a, o := NewEngineAdapter(cypher.NewEngine(g)), NewGraphOracle()
		ctx := context.Background()
		for i, name := range []string{"n1", "n2"} {
			if _, err := a.RunWrite(ctx, tmplCreatePerson,
				map[string]any{"name": name, "age": int64(i)}); err != nil {
				t.Fatalf("fixture write: %v", err)
			}
			o.ApplyCreate(tmplCreatePerson, map[string]any{"name": name, "age": int64(i)})
		}
		id1, id2 := o.byName["n1"], o.byName["n2"]
		o.edges[edgeKey{src: id1, dst: id2, label: "KNOWS"}] =
			&EdgeState{SrcID: id1, DstID: id2, Label: "KNOWS", Properties: map[string]any{}}

		var st deleteContractStats
		v := probeDeleteContract(ctx, 0, o, a, &st)
		if len(v) == 0 {
			t.Fatal("probe FAILED to fire when the engine committed a delete the oracle predicted would be refused")
		}
		if !strings.Contains(v[0].Message, "COMMITTED") {
			t.Errorf("unexpected violation message: %s", v[0].Message)
		}
	})
}

// TestDeleteContract_VacuityFiresOnAMissingArm proves the terminal gate refuses
// a run in which either arm never executed — a population with no connected (or
// no isolated) node adjudicates nothing and must not report a silent pass.
func TestDeleteContract_VacuityFiresOnAMissingArm(t *testing.T) {
	cases := map[string]deleteContractStats{
		"no arm at all":     {},
		"only accepted":     {accepted: 3},
		"only refused":      {refused: 3},
		"both arms present": {accepted: 1, refused: 1},
	}
	for name, st := range cases {
		st := st
		t.Run(name, func(t *testing.T) {
			v := deleteContractVacuity(0, &st)
			wantFire := st.accepted == 0 || st.refused == 0
			if wantFire && len(v) == 0 {
				t.Fatalf("vacuity gate FAILED to fire for %+v", st)
			}
			if !wantFire && len(v) > 0 {
				t.Fatalf("vacuity gate fired although both arms ran: %v", v)
			}
		})
	}
}

// TestDeleteContract_CountersArmIsReachable proves the per-op counters oracle
// derives an EXACT expectation for the non-detach template rather than skipping
// it. An arm that reported itself inexact would silently accept any counters at
// all, which is how a previous arm sat dead.
func TestDeleteContract_CountersArmIsReachable(t *testing.T) {
	_, o := buildDeleteContractFixture(t)

	isolated := Op{Kind: OpDelete, Cypher: tmplDeleteNode, Params: map[string]any{"name": "lonely"}}
	want, exact := expectedOpCounters(isolated, o)
	if !exact {
		t.Fatal("counters arm for the non-detach DELETE reported itself INEXACT; it would accept any counters")
	}
	if want.NodesDeleted != 1 || want.RelationshipsDeleted != 0 {
		t.Errorf("isolated delete expectation = %+v, want NodesDeleted=1 and nothing else", want)
	}

	// A name the model does not hold: the MATCH yields nothing, so the exact
	// expectation is the all-zero effect set.
	miss := Op{Kind: OpDelete, Cypher: tmplDeleteNode, Params: map[string]any{"name": "nosuch"}}
	if want, exact := expectedOpCounters(miss, o); !exact || want != (exec.QueryCounters{}) {
		t.Errorf("missing-target expectation = %+v exact=%v, want all-zero and exact", want, exact)
	}
}

// TestDeleteContract_OracleRefusesConnectedApply proves the shadow model refuses
// to apply a non-detach delete to a connected node. Applying it would silently
// diverge the model from the engine, since the engine never commits one.
func TestDeleteContract_OracleRefusesConnectedApply(t *testing.T) {
	_, o := buildDeleteContractFixture(t)
	res := o.ApplyDelete(tmplDeleteNode, map[string]any{"name": "conn"})
	if res.ErrorMsg == "" {
		t.Fatal("oracle silently applied a non-detach DELETE to a connected node")
	}
	if _, gone := o.byName["conn"]; !gone {
		t.Error("the refused apply still removed the node from the model")
	}
}
