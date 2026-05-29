package main

import (
	"context"
	"slices"
	"testing"
)

// openSeeded opens a fresh store in a temp dir and loads the fixture,
// failing the test on any error.
func openSeeded(t *testing.T) *dataStore {
	t.Helper()
	ds, err := openStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	seeded, err := seedFixture(ds.txnStore)
	if err != nil {
		t.Fatalf("seedFixture: %v", err)
	}
	if !seeded {
		t.Fatal("seedFixture: want seeded=true on a fresh store")
	}
	return ds
}

// queryRows runs a read query and returns its row count and columns.
func queryRows(t *testing.T, ds *dataStore, cypher string, params map[string]any) (rows int, cols []string) {
	t.Helper()
	res, err := ds.engine.RunAny(context.Background(), cypher, params)
	if err != nil {
		t.Fatalf("RunAny: %v\nquery: %s", err, cypher)
	}
	defer func() { _ = res.Close() }()
	cols = slices.Clone(res.Columns())
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate: %v\nquery: %s", err, cypher)
	}
	return rows, cols
}

func catalogue(t *testing.T, name string) maintenanceQuery {
	t.Helper()
	for _, q := range maintenanceCatalogue {
		if q.Name == name {
			return q
		}
	}
	t.Fatalf("unknown catalogue query %q", name)
	return maintenanceQuery{}
}

func TestSeedIdempotent(t *testing.T) {
	ds := openSeeded(t)
	again, err := seedFixture(ds.txnStore)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if again {
		t.Fatal("re-seed: want seeded=false (idempotent), got true")
	}
}

func TestSeedNodeAndEdgeCounts(t *testing.T) {
	ds := openSeeded(t)

	wantNodes := map[string]int{
		typeRepository: 1, typeModule: 5, typeComponent: 12,
		typeTask: 14, typeSprint: 2, typeWorkflowState: 4,
		typeDeveloper: 6, typeTeam: 2,
	}
	totalNodes := 0
	for _, lbl := range nodeTypeLabels {
		got, _ := queryRows(t, ds, "MATCH (n:"+lbl+") RETURN n", nil)
		if got != wantNodes[lbl] {
			t.Errorf("node count %s = %d, want %d", lbl, got, wantNodes[lbl])
		}
		totalNodes += got
	}
	if totalNodes != 46 {
		t.Errorf("total nodes = %d, want 46", totalNodes)
	}

	wantEdges := map[string]int{
		relContains: 17, relDependsOn: 17, relSubtaskOf: 2, relNext: 4,
		relBlocks: 3, relHasState: 14, relInSprint: 14, relMemberOf: 6,
		relAssignedTo: 16, relTouches: 13,
	}
	totalEdges := 0
	for _, rt := range relTypes {
		got, _ := queryRows(t, ds, "MATCH ()-[r:"+rt+"]->() RETURN r", nil)
		if got != wantEdges[rt] {
			t.Errorf("edge count %s = %d, want %d", rt, got, wantEdges[rt])
		}
		totalEdges += got
	}
	if totalEdges != 106 {
		t.Errorf("total edges = %d, want 106", totalEdges)
	}
}

func TestMaintenanceCatalogueNonEmpty(t *testing.T) {
	ds := openSeeded(t)
	for _, q := range maintenanceCatalogue {
		got, _ := queryRows(t, ds, q.Cypher, q.Example)
		if got == 0 {
			t.Errorf("%s returned 0 rows (want >=1): %s", q.Name, q.Question)
		}
	}
}

func TestMaintenanceSpecificCounts(t *testing.T) {
	ds := openSeeded(t)
	cases := []struct {
		name   string
		params map[string]any
		want   int
	}{
		{"Q3", nil, 9}, // bus-factor-1 components
		{"Q4", map[string]any{"dev": "dev:alice"}, 2},   // alice's open work (WS-9, WS-13)
		{"Q5", map[string]any{"task": "task:WS-12"}, 2}, // WS-10->WS-12 and WS-14->WS-10->WS-12
		{"Q7", nil, 2}, // orders/service + payments/service on a cycle
	}
	for _, c := range cases {
		got, _ := queryRows(t, ds, catalogue(t, c.name).Cypher, c.params)
		if got != c.want {
			t.Errorf("%s rows = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestCatalogueColumnsClean guards against the ORDER BY-on-variable quirk
// that would otherwise surface a stray pattern variable as an extra
// output column. Q4/Q8 order by their projection aliases for this reason.
func TestCatalogueColumnsClean(t *testing.T) {
	ds := openSeeded(t)
	want := map[string][]string{
		"Q4": {"task", "title", "status", "priority", "role"},
		"Q8": {"developer", "task", "change", "churn", "at"},
	}
	for name, cols := range want {
		q := catalogue(t, name)
		_, got := queryRows(t, ds, q.Cypher, q.Example)
		if !slices.Equal(got, cols) {
			t.Errorf("%s columns = %v, want %v", name, got, cols)
		}
	}
}
