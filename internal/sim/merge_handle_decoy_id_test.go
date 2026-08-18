package sim

// merge_handle_decoy_id_test.go — regression gate for rmp #2524.
//
// The handle-collision fixture (rmp #2515) needs a decoy node whose id can serve
// as a relationship's stable handle. Node id 0 cannot: it is the reserved
// no-handle sentinel. Whether the decoy draws id 0 is decided entirely OUTSIDE
// the scenario: a node's id is (intraShardIndex<<8 | shard), the shard comes from
// the hash of the synthetic key cypher/exec mints for the node, and that key
// counts up from a PROCESS-GLOBAL counter whose value here depends on how many
// nodes every earlier test in the process created. On roughly 0.4% of process
// histories the first decoy landed on id 0, and the bootstrap reported that as a
// GRAPH_INTEGRITY violation of GoGraph — a false positive that failed
// TestSchemaMutation_Scenario_Passes with no defect present.
//
// These tests pin the correction: the bootstrap must survive the id-0 draw by
// falling back to a second decoy candidate, and the collision it hands over must
// still be genuinely CONSTRUCTED, never quietly degraded.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mintOneSyntheticKey creates a single node through the engine and returns the
// numeric suffix of the synthetic key it received, which is the value of the
// process-global counter that node consumed.
func mintOneSyntheticKey(t *testing.T) uint64 {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	a := NewEngineAdapter(cypher.NewEngine(g))
	if _, err := a.RunWrite(context.Background(), tmplMergeHandleCreatePerson,
		map[string]any{"name": "counter-probe"}); err != nil {
		t.Fatalf("counter probe CREATE: %v", err)
	}
	var n uint64
	var found bool
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		v, err := strconv.ParseUint(key[len("__cx_"):], 16, 64)
		if err == nil {
			n, found = v, true
		}
		return true
	})
	if !found {
		t.Fatal("could not read the process-global node-key counter back from a minted key")
	}
	return n
}

// thirdKeyLandsOnNodeIDZero reports whether interning the three synthetic keys
// __cx_<n+1>, __cx_<n+2>, __cx_<n+3> into a FRESH graph — the order and the count
// the fixture bootstrap uses — puts the third one on node id 0.
//
// It answers the question by interning the keys for real rather than by
// recomputing the mapper's hash, so it cannot drift from the layout the mapper
// actually implements.
func thirdKeyLandsOnNodeIDZero(t *testing.T, n uint64) bool {
	t.Helper()
	probe := lpg.New[string, float64](adjlist.Config{Directed: true})
	var third string
	for i := uint64(1); i <= 3; i++ {
		key := "__cx_" + strconv.FormatUint(n+i, 16)
		if err := probe.AddNode(key); err != nil {
			t.Fatalf("probe AddNode(%q): %v", key, err)
		}
		third = key
	}
	id, ok := probe.AdjList().Mapper().Lookup(third)
	if !ok {
		t.Fatalf("probe key %q is not interned", third)
	}
	return id == 0
}

// alignCounterOntoDecoyIDZero burns synthetic keys until the next three the
// engine mints would place the fixture's decoy — the third of them — on node
// id 0. It is the deterministic stand-in for the unlucky process history that
// produced the rmp #2524 failure, and it needs neither -race nor a full-package
// run to reach it.
func alignCounterOntoDecoyIDZero(t *testing.T) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		if thirdKeyLandsOnNodeIDZero(t, mintOneSyntheticKey(t)) {
			return
		}
	}
	t.Fatal("could not align the process-global counter onto a decoy id of 0 within 5000 keys")
}

// TestMergeHandleCollision_DecoyNodeIDZeroIsCorrected drives the bootstrap on the
// exact process history that used to fail it. Before rmp #2524 this reported
// "fixture decoy \"hc-decoy\" received node id 0"; now it must build the
// collision on the alternate decoy instead.
func TestMergeHandleCollision_DecoyNodeIDZeroIsCorrected(t *testing.T) {
	alignCounterOntoDecoyIDZero(t)

	// newHandleCollisionSim fails the test if the bootstrap reports any violation,
	// which is precisely the old behaviour this gate forbids.
	sm, f := newHandleCollisionSim(t)
	g := sm.graph()

	if f.decoyName != mergeHandleDecoyAltName {
		t.Fatalf("decoyName = %q, want the alternate %q: the counter was aligned so the first candidate "+
			"draws node id 0, so the fallback must have been taken", f.decoyName, mergeHandleDecoyAltName)
	}
	if f.decoyID == 0 {
		t.Fatal("the alternate decoy also drew node id 0, which the first candidate's slot makes impossible")
	}
	// The collision must be genuinely constructed, not merely un-reported.
	if f.handle != uint64(f.decoyID) {
		t.Fatalf("handle = %d, want %d (the alternate decoy's node id)", f.handle, f.decoyID)
	}
	if got := personNameByID(g, graph.NodeID(f.handle)); got != f.decoyName {
		t.Fatalf("node id %d is %q, want the alternate decoy %q", f.handle, got, f.decoyName)
	}
	if v := CheckMergeHandleCollision(0, f, g, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("the freshly-built fixture must be clean: %v", v)
	}
	// The fourth node the fallback created must be modelled, or count parity —
	// which every tick of the scenario re-checks — would break.
	if v := NewInvariantChecker(NewSeed(1)).Check(0, sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("count parity after the fallback bootstrap: %v", v)
	}
}

// TestMergeHandleCollision_FallbackDetectorStillFires is the anti-degradation
// gate. Surviving the id-0 draw would be worthless if the fixture survived it by
// quietly becoming inert, so this drives the very defect the family exists to
// catch — a write misdirected onto the node whose id equals the relationship's
// stable handle — and requires the detector to still report it when that node is
// the FALLBACK decoy.
//
// Without it, TestMergeHandleCollision_DecoyNodeIDZeroIsCorrected would be
// consistent with a fixture that had been silently skipped.
func TestMergeHandleCollision_FallbackDetectorStillFires(t *testing.T) {
	alignCounterOntoDecoyIDZero(t)
	sm, f := newHandleCollisionSim(t)
	if f.decoyName != mergeHandleDecoyAltName {
		t.Fatalf("precondition: decoyName = %q, want the fallback %q", f.decoyName, mergeHandleDecoyAltName)
	}

	// A correct graph must be clean, or the "fires" result below would be
	// indistinguishable from a checker that always fires.
	if v := CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine); len(v) > 0 {
		t.Fatalf("the freshly-built fallback fixture must be clean: %v", v)
	}

	// Now the misdirection rmp #2515 fixed: the write lands on the decoy instead
	// of on the relationship.
	inject := Op{Kind: OpUpdate, Cypher: "MATCH (p:Person {name:'" + f.decoyName + "'}) SET p." +
		mergePairRelKey + " = 17"}
	if committed, _ := sm.executeCounted(context.Background(), inject); !committed {
		t.Fatal("injection did not commit")
	}
	var sawDecoy bool
	for _, viol := range CheckMergeHandleCollision(0, f, sm.graph(), sm.oracle, sm.engine) {
		if viol.Op == "merge handle-collision decoy" && strings.Contains(viol.Message, "MISDIRECTED") {
			sawDecoy = true
		}
	}
	if !sawDecoy {
		t.Fatal("the detector did not fire on a write misdirected onto the FALLBACK decoy: surviving the " +
			"id-0 draw has degraded the arm into one that proves nothing")
	}
}

// TestSchemaMutation_ScenarioSurvivesDecoyNodeIDZero is the end-to-end gate: the
// scenario TestSchemaMutation_Scenario_Passes runs must come back clean even on
// the process history that put the first decoy on node id 0.
func TestSchemaMutation_ScenarioSurvivesDecoyNodeIDZero(t *testing.T) {
	alignCounterOntoDecoyIDZero(t)

	sc := schemaMutationScenario()
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("schema-mutation run: %v", err)
	}
	if report != nil {
		t.Fatalf("schema-mutation reported a violation on a decoy-id-0 process history:\n%s", report)
	}
}
