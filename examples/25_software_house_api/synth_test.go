package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestSynthScaleZeroIsBareFixture asserts the opt-in contract: a zero
// synthScale adds nothing, so seedFixtureScaled produces exactly the
// hand-authored 46-node / 106-edge fixture the rest of the suite pins.
func TestSynthScaleZeroIsBareFixture(t *testing.T) {
	ds, err := openStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = ds.Close() })

	seeded, err := seedFixtureScaled(ds.txnStore, synthScale{})
	if err != nil {
		t.Fatalf("seedFixtureScaled: %v", err)
	}
	if !seeded {
		t.Fatal("want seeded=true on a fresh store")
	}

	total := 0
	for _, lbl := range nodeTypeLabels {
		n, _ := queryRows(t, ds, "MATCH (n:"+lbl+") RETURN n", nil)
		total += n
	}
	if total != 46 {
		t.Errorf("zero-scale node total = %d, want 46 (the bare fixture)", total)
	}
	edges := 0
	for _, rt := range relTypes {
		n, _ := queryRows(t, ds, "MATCH ()-[r:"+rt+"]->() RETURN r", nil)
		edges += n
	}
	if edges != 106 {
		t.Errorf("zero-scale edge total = %d, want 106 (the bare fixture)", edges)
	}
}

// scaledTestConfig is the small synthetic scale this file's deterministic
// assertions are pinned against. Small enough to seed in well under a second
// and to keep the unbounded-VLE catalogue queries out of the picture.
var scaledTestConfig = synthScale{components: 50, tasks: 30, developers: 8, seed: 7}

// openScaled opens a fresh store and loads the base fixture plus the synthetic
// layer at scaledTestConfig.
func openScaled(t *testing.T) *dataStore {
	t.Helper()
	ds, err := openStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	seeded, err := seedFixtureScaled(ds.txnStore, scaledTestConfig)
	if err != nil {
		t.Fatalf("seedFixtureScaled: %v", err)
	}
	if !seeded {
		t.Fatal("want seeded=true on a fresh store")
	}
	return ds
}

// TestSynthScaleDeterministicCounts pins the exact node and edge counts the
// seeded generator produces at scaledTestConfig. These are deterministic
// FACTS: a fixed (counts, seed) reproduces them byte-for-byte across machines.
// Telemetry (heap, latency) is never asserted.
func TestSynthScaleDeterministicCounts(t *testing.T) {
	ds := openScaled(t)

	wantNodes := map[string]int{
		typeRepository: 2, typeModule: 8, typeComponent: 62,
		typeTask: 44, typeSprint: 3, typeWorkflowState: 4,
		typeDeveloper: 14, typeTeam: 3,
	}
	for _, lbl := range nodeTypeLabels {
		got, _ := queryRows(t, ds, "MATCH (n:"+lbl+") RETURN n", nil)
		if got != wantNodes[lbl] {
			t.Errorf("scaled node %s = %d, want %d", lbl, got, wantNodes[lbl])
		}
	}

	wantEdges := map[string]int{
		relContains: 70, relDependsOn: 194, relSubtaskOf: 2, relNext: 4,
		relBlocks: 7, relHasState: 44, relInSprint: 44, relMemberOf: 14,
		relAssignedTo: 56, relTouches: 87,
	}
	for _, rt := range relTypes {
		got, _ := queryRows(t, ds, "MATCH ()-[r:"+rt+"]->() RETURN r", nil)
		if got != wantEdges[rt] {
			t.Errorf("scaled edge %s = %d, want %d", rt, got, wantEdges[rt])
		}
	}
}

// TestSynthScaleStructuralInvariants asserts the model invariants that must
// hold at ANY scale — additivity over the base fixture and the one-edge-per-X
// relationships — so the topology stays faithful even if the constants change.
func TestSynthScaleStructuralInvariants(t *testing.T) {
	ds := openScaled(t)
	count := func(q string) int {
		n, _ := queryRows(t, ds, q, nil)
		return n
	}

	// Layer additivity: synthetic nodes add to the base fixture's counts.
	if got, want := count("MATCH (c:Component) RETURN c"), 12+scaledTestConfig.components; got != want {
		t.Errorf("Component count = %d, want base(12)+scale(%d)=%d", got, scaledTestConfig.components, want)
	}
	if got, want := count("MATCH (t:Task) RETURN t"), 14+scaledTestConfig.tasks; got != want {
		t.Errorf("Task count = %d, want base(14)+scale(%d)=%d", got, scaledTestConfig.tasks, want)
	}
	if got, want := count("MATCH (d:Developer) RETURN d"), 6+scaledTestConfig.developers; got != want {
		t.Errorf("Developer count = %d, want base(6)+scale(%d)=%d", got, scaledTestConfig.developers, want)
	}

	// One HAS_STATE and one IN_SPRINT per task; one MEMBER_OF per developer.
	tasks := count("MATCH (t:Task) RETURN t")
	if got := count("MATCH ()-[r:HAS_STATE]->() RETURN r"); got != tasks {
		t.Errorf("HAS_STATE = %d, want one per task (%d)", got, tasks)
	}
	if got := count("MATCH ()-[r:IN_SPRINT]->() RETURN r"); got != tasks {
		t.Errorf("IN_SPRINT = %d, want one per task (%d)", got, tasks)
	}
	devs := count("MATCH (d:Developer) RETURN d")
	if got := count("MATCH ()-[r:MEMBER_OF]->() RETURN r"); got != devs {
		t.Errorf("MEMBER_OF = %d, want one per developer (%d)", got, devs)
	}
}

// TestSynthScaleRealism asserts that the realism properties the maintenance
// queries depend on survive scaling: the dependency graph carries cycles
// (Q7-style), a heavy in-degree tail (Q6-style), bus-factor-1 components
// (Q3-style), and shallow BLOCKS chains (Q5-style). The cycle and blocked-work
// probes are DEPTH-CAPPED so they stay fast on the scaled graph — the
// unbounded catalogue variants are pathologically slow on a dense dependency
// graph, which is itself evidence the example surfaces at scale.
func TestSynthScaleRealism(t *testing.T) {
	ds := openScaled(t)
	count := func(q string) int {
		n, _ := queryRows(t, ds, q, nil)
		return n
	}

	// Cycles: the injected back-edges plus the base fixture's orders<->payments
	// cycle mean at least one component lies on a short dependency cycle.
	if got := count("MATCH (c:Component)-[:DEPENDS_ON*1..6]->(c) RETURN DISTINCT c"); got < 2 {
		t.Errorf("components on a cycle = %d, want >= 2 (cycle injection failed)", got)
	}

	// Heavy in-degree tail: at least one synthetic component is depended on by
	// several others (preferential attachment produced a hub).
	if got := count("MATCH (c:Component)<-[:DEPENDS_ON]-(d:Component) WITH c, count(d) AS deg WHERE deg >= 5 RETURN c"); got < 1 {
		t.Errorf("components with in-degree >= 5 = %d, want >= 1 (no dependency hub formed)", got)
	}

	// Bus-factor risk: the affinity coupling yields many single-owner
	// components, not a uniform spread.
	busFactor1 := count("MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[:TOUCHES]->(c:Component) " +
		"WITH c, count(DISTINCT dev) AS bf WHERE bf = 1 RETURN c")
	if busFactor1 < 1 {
		t.Errorf("bus-factor-1 components = %d, want >= 1 (ownership did not cluster)", busFactor1)
	}

	// BLOCKS chains stay shallow: a depth-7 reach finds the chains but they do
	// not run away, so the query stays bounded.
	if got := count("MATCH (root:Task)-[:BLOCKS*1..7]->(:Task) RETURN root"); got < 1 {
		t.Errorf("tasks on a BLOCKS chain = %d, want >= 1", got)
	}
}

// TestSynthScaleValidate covers the synthScale boundary checks.
func TestSynthScaleValidate(t *testing.T) {
	cases := []struct {
		name    string
		scale   synthScale
		wantErr bool
	}{
		{"zero", synthScale{}, false},
		{"positive", synthScale{components: 10, tasks: 5, developers: 2}, false},
		{"negative components", synthScale{components: -1}, true},
		{"negative tasks", synthScale{tasks: -1}, true},
		{"negative developers", synthScale{developers: -1}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.scale.validate()
			if (err != nil) != c.wantErr {
				t.Errorf("validate() err = %v, wantErr = %v", err, c.wantErr)
			}
			if c.scale.active() != (c.scale.components > 0 || c.scale.tasks > 0 || c.scale.developers > 0) {
				t.Errorf("active() inconsistent for %+v", c.scale)
			}
		})
	}
}

// TestStatsTelemetryPresent asserts the GET /stats response carries a
// well-formed telemetry block alongside the deterministic counts. It checks
// only that the telemetry fields are PRESENT and well-formed (a forced GC
// makes heap non-zero; the stats sweep this response measured is recorded) —
// never their volatile values.
func TestStatsTelemetryPresent(t *testing.T) {
	ts, c := newTestServer(t, true)

	// Drive one query so the query counters are non-zero.
	do(t, c, http.MethodPost, ts.URL+"/query", `{"query":"MATCH (c:Component) RETURN count(c) AS n"}`)

	st, raw := do(t, c, http.MethodGet, ts.URL+"/stats", "")
	if st != http.StatusOK {
		t.Fatalf("stats status = %d, want 200 (%s)", st, raw)
	}

	// The facts must still decode into statsResponse exactly as before.
	var sr statsResponse
	mustJSON(t, raw, &sr)
	if sr.Nodes[typeComponent] != 12 {
		t.Errorf("stats facts nodes[Component] = %d, want 12", sr.Nodes[typeComponent])
	}
	if sr.Telemetry == nil {
		t.Fatal("stats response has no telemetry block")
	}

	// Assert the telemetry block is PRESENT and well-formed, never its values.
	// The "telemetry" key must exist; the heap fields must be self-consistent;
	// the counters must reflect that at least this /stats (and the query above)
	// were served.
	var envelope map[string]json.RawMessage
	mustJSON(t, raw, &envelope)
	if _, ok := envelope["telemetry"]; !ok {
		t.Error("response is missing the \"telemetry\" object")
	}
	tel := sr.Telemetry
	if tel.HeapAllocBytes == 0 {
		t.Error("telemetry heap_alloc_bytes = 0, want > 0 after a forced GC")
	}
	if tel.HeapAllocHuman == "" {
		t.Error("telemetry heap_alloc_human is empty")
	}
	if tel.StatsCount < 1 {
		t.Errorf("telemetry stats_count = %d, want >= 1", tel.StatsCount)
	}
	if tel.QueryCount < 1 {
		t.Errorf("telemetry query_count = %d, want >= 1 (one query was issued)", tel.QueryCount)
	}
	if tel.BytesPerElement <= 0 {
		t.Errorf("telemetry bytes_per_element = %v, want > 0 on a non-empty graph", tel.BytesPerElement)
	}
}

// TestSeedEndpointScale drives the synthetic scale through the POST /seed
// request body and asserts the resulting deterministic counts plus the echoed
// scale fields.
func TestSeedEndpointScale(t *testing.T) {
	ts, c := newTestServer(t, false)

	body := `{"scale_components":50,"scale_tasks":30,"scale_developers":8,"scale_seed":7}`
	st, raw := do(t, c, http.MethodPost, ts.URL+"/seed", body)
	if st != http.StatusOK {
		t.Fatalf("scaled seed status = %d, want 200 (%s)", st, raw)
	}
	var resp map[string]any
	mustJSON(t, raw, &resp)
	if resp["seeded"] != true {
		t.Errorf("seeded = %v, want true", resp["seeded"])
	}
	if resp["scale_components"] != float64(50) {
		t.Errorf("echoed scale_components = %v, want 50", resp["scale_components"])
	}

	// The scaled facts must match the pinned counts.
	st, raw = do(t, c, http.MethodGet, ts.URL+"/stats", "")
	if st != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", st)
	}
	var sr statsResponse
	mustJSON(t, raw, &sr)
	if sr.Nodes[typeComponent] != 62 {
		t.Errorf("scaled nodes[Component] = %d, want 62 (12 base + 50 synthetic)", sr.Nodes[typeComponent])
	}
	if sr.Nodes[typeDeveloper] != 14 {
		t.Errorf("scaled nodes[Developer] = %d, want 14 (6 base + 8 synthetic)", sr.Nodes[typeDeveloper])
	}
}

// TestSeedEndpointRejectsNegativeScale asserts a negative scale field is a 400.
func TestSeedEndpointRejectsNegativeScale(t *testing.T) {
	ts, c := newTestServer(t, false)
	st, raw := do(t, c, http.MethodPost, ts.URL+"/seed", `{"scale_components":-5}`)
	if st != http.StatusBadRequest {
		t.Fatalf("negative scale status = %d, want 400 (%s)", st, raw)
	}
	var eb errorBody
	mustJSON(t, raw, &eb)
	if eb.Kind != "bad_request" {
		t.Errorf("kind = %q, want bad_request", eb.Kind)
	}
}
