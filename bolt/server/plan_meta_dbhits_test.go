package server

// plan_meta_dbhits_test.go — the `dbHits` OMISSION on the Bolt wire (rmp #2760).
//
// The e2e gates in e2e_plan_prefix_test.go assert through the official driver,
// which is the only instrument that can prove the key names, value types and
// message placement are right. It cannot prove this property: the driver reads
// `dbHits` with a comma-ok assertion
// (`plan.DbHits, _ = profilex["dbHits"].(int64)`, neo4j-go-driver v5.28.4,
// neo4j/internal/bolt/hydrator.go), so an ABSENT key and a key carrying 0 arrive
// at ResultSummary.Profile() as the same 0. The distinction exists in the map
// this server builds, and the map is therefore where it has to be asserted.
//
// The rule being asserted is the one this file's production code already applied
// to the page-cache fields, which are omitted rather than zeroed because GoGraph
// measures none of them and a 0 would be a measurement claim. rmp #2760 applied
// the same rule to `dbHits`.
//
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// TestPlanNodeMetadata_OmitsUncountedDbHits is the acceptance gate: an operator
// whose storage accesses nobody counted must send NO `dbHits` key, and one that
// counted them must send the figure.
//
// Both directions are asserted, because a server that simply never sent the key
// would satisfy the first alone and would publish no db-hits at all.
//
// The uncounted node in the fixture used to be a ParallelScanProject. rmp #2762
// taught the morsel-parallel leaves to count, so that name would now describe an
// operator whose cell IS a figure — a fixture asserting the opposite of what the
// engine does, even though the map-building code under test never sees an
// operator. It is a Filter instead: an operator whose predicate can be a pattern
// predicate that walks adjacency, which nothing counts, so it remains genuinely
// UNKNOWN (see exec.noStorageAccess, "the bar for claiming it").
func TestPlanNodeMetadata_OmitsUncountedDbHits(t *testing.T) {
	t.Parallel()

	tree := exec.PlanNode{
		Name:     "Filter",
		Profiled: true, Rows: 2000, DbHits: 0, DbHitsKnown: false,
		Children: []exec.PlanNode{{
			Name: "NodeByLabelScan", Detail: "B",
			Profiled: true, Rows: 2000, DbHits: 2000, DbHitsKnown: true,
		}},
	}

	m := planNodeMetadata(&tree, true)

	if _, present := m[planKeyDbHits]; present {
		t.Errorf("the uncounted operator published %s=%v. Nothing counted the storage "+
			"its predicate can reach, so a value on this key is a measurement claim "+
			"the engine cannot stand behind — the same reason pageCacheHits/"+
			"pageCacheMisses are omitted rather than zeroed.", planKeyDbHits, m[planKeyDbHits])
	}
	// The measured fields that ARE known must still be published, or the omission
	// above would be indistinguishable from profiling not reaching the node.
	if got, present := m[planKeyRows]; !present || got != int64(2000) {
		t.Errorf("%s = %v (present=%v), want 2000: rows are measured for every operator "+
			"and are unaffected by the db-hits state", planKeyRows, got, present)
	}
	if _, present := m[planKeyTime]; !present {
		t.Errorf("%s is absent; time is measured for every operator", planKeyTime)
	}

	kids, ok := m[planKeyChildren].([]any)
	if !ok || len(kids) != 1 {
		t.Fatalf("%s = %#v, want one child map", planKeyChildren, m[planKeyChildren])
	}
	child, ok := kids[0].(map[string]any)
	if !ok {
		t.Fatalf("child metadata is %T, want a map", kids[0])
	}
	got, present := child[planKeyDbHits]
	if !present {
		t.Fatalf("the label scan published no %s key, so the omission above proves "+
			"nothing: a server that never sent the key would pass too", planKeyDbHits)
	}
	if got != int64(2000) {
		t.Errorf("%s = %v (%T), want int64(2000) — the driver asserts this value to "+
			"int64", planKeyDbHits, got, got)
	}
}

// TestPlanNodeMetadata_ExplainPublishesNoMeasurements is the control on the
// profiled flag: an EXPLAIN executed nothing, so none of the three measured keys
// may appear whatever the captured node's fields say.
func TestPlanNodeMetadata_ExplainPublishesNoMeasurements(t *testing.T) {
	t.Parallel()

	tree := exec.PlanNode{
		Name: "NodeByLabelScan", Detail: "B",
		Profiled: true, Rows: 2000, DbHits: 2000, DbHitsKnown: true,
	}
	m := planNodeMetadata(&tree, false)

	for _, key := range []string{planKeyRows, planKeyDbHits, planKeyTime} {
		if v, present := m[key]; present {
			t.Errorf("an EXPLAIN published %s=%v; it executed nothing", key, v)
		}
	}
	if m[planKeyOperatorType] != "NodeByLabelScan" {
		t.Errorf("%s = %v, want the operator name", planKeyOperatorType, m[planKeyOperatorType])
	}
}
