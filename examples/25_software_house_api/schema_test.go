package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestSchemaEndpoint pins the deterministic schema FACTS surfaced by
// GET /schema at the seeded default: the label and relationship-type sets in
// use, the declared index set (including the UNIQUE constraint's __uniq__
// backing index), and the declared constraint set. Volatile detail is not
// present in this response, so every field is a fact.
func TestSchemaEndpoint(t *testing.T) {
	ts, c := newTestServer(t, true)
	st, raw := do(t, c, http.MethodGet, ts.URL+"/schema", "")
	if st != http.StatusOK {
		t.Fatalf("schema status = %d, want 200 (%s)", st, raw)
	}
	var sr schemaResponse
	mustJSON(t, raw, &sr)

	wantLabels := []string{
		"Code", "Component", "Developer", "Module", "People", "Repository",
		"Sprint", "Task", "Team", "Work", "WorkflowState",
	}
	if !slices.Equal(sr.Labels, wantLabels) {
		t.Errorf("labels = %v\nwant %v", sr.Labels, wantLabels)
	}

	wantRelTypes := []string{
		"ASSIGNED_TO", "BLOCKS", "CONTAINS", "DEPENDS_ON", "HAS_STATE",
		"IN_SPRINT", "MEMBER_OF", "NEXT", "SUBTASK_OF", "TOUCHES",
	}
	if !slices.Equal(sr.RelationshipTypes, wantRelTypes) {
		t.Errorf("relationshipTypes = %v\nwant %v", sr.RelationshipTypes, wantRelTypes)
	}

	// Property keys in use: a robust subset a change to the fixture would not
	// silently drop. The full set may include edge property keys, so pin a
	// representative subset rather than the exact set.
	for _, k := range []string{"key", "name", "status", "priority", "loc"} {
		if !slices.Contains(sr.PropertyKeys, k) {
			t.Errorf("propertyKeys missing %q; got %v", k, sr.PropertyKeys)
		}
	}

	// Two indexes: the plain Developer.key index and the UNIQUE constraint's
	// backing hash index. Sorted by name, "__uniq__..." precedes "developer...".
	wantIndexes := []schemaIndex{
		{Name: "__uniq__Component.key", Type: "hash"},
		{Name: indexDeveloperKeyName, Type: "hash"},
	}
	if !slices.Equal(sr.Indexes, wantIndexes) {
		t.Errorf("indexes = %+v\nwant %+v", sr.Indexes, wantIndexes)
	}

	wantConstraints := []schemaConstraint{
		{Name: constraintComponentKeyName, Type: "UNIQUE", Label: typeComponent, Property: "key"},
	}
	if !slices.Equal(sr.Constraints, wantConstraints) {
		t.Errorf("constraints = %+v\nwant %+v", sr.Constraints, wantConstraints)
	}
}

// TestConstraintViolationRejected drives the constraint-enforcement path: a
// CREATE that duplicates a UNIQUE-constrained Component.key is rejected with
// 409 Conflict and rolled back atomically (the Component count is unchanged).
func TestConstraintViolationRejected(t *testing.T) {
	ts, c := newTestServer(t, true)

	before := componentCount(t, c, ts.URL)

	// comp:platform/config.go is a seeded Component; re-creating it violates the
	// UNIQUE constraint on Component.key.
	body := `{"query":"CREATE (c:Component:Code {key:'comp:platform/config.go', name:'dup'})"}`
	st, raw := do(t, c, http.MethodPost, ts.URL+"/query", body)
	if st != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409 (%s)", st, raw)
	}
	var eb errorBody
	mustJSON(t, raw, &eb)
	if eb.Kind != "conflict" {
		t.Errorf("kind = %q, want %q", eb.Kind, "conflict")
	}
	if !strings.Contains(eb.Error, "UNIQUE") || !strings.Contains(eb.Error, "key") {
		t.Errorf("error message = %q, want it to mention the UNIQUE key violation", eb.Error)
	}

	// Atomic rollback: the rejected write left no Component behind.
	if after := componentCount(t, c, ts.URL); after != before {
		t.Errorf("Component count after rejected write = %d, want %d (rollback failed)", after, before)
	}

	// A DISTINCT, non-colliding Component key is still accepted.
	ok := `{"query":"CREATE (c:Component:Code {key:'comp:new/widget.go', name:'widget'}) RETURN c.key AS key"}`
	if st, raw := do(t, c, http.MethodPost, ts.URL+"/query", ok); st != http.StatusOK {
		t.Fatalf("distinct-key CREATE status = %d, want 200 (%s)", st, raw)
	}
}

// TestExplainEndpoint proves the declared schema is used: an equality lookup on
// a constrained/indexed property plans as a NodeByIndexSeek, while the same
// shape on an un-indexed property falls back to a NodeByLabelScan.
func TestExplainEndpoint(t *testing.T) {
	ts, c := newTestServer(t, true)

	seekCases := []struct{ name, body string }{
		{
			"component key (constraint-backed)",
			`{"query":"MATCH (c:Component) WHERE c.key = $k RETURN c","params":{"k":"comp:platform/config.go"}}`,
		},
		{
			"developer key (secondary index)",
			`{"query":"MATCH (d:Developer) WHERE d.key = $k RETURN d","params":{"k":"dev:alice"}}`,
		},
	}
	for _, tc := range seekCases {
		t.Run(tc.name, func(t *testing.T) {
			st, raw := do(t, c, http.MethodPost, ts.URL+"/explain", tc.body)
			if st != http.StatusOK {
				t.Fatalf("explain status = %d, want 200 (%s)", st, raw)
			}
			var er explainResponse
			mustJSON(t, raw, &er)
			if !er.UsesIndexSeek {
				t.Errorf("uses_index_seek = false, want true; plan:\n%s", er.Plan)
			}
			if !strings.Contains(er.Plan, "NodeByIndexSeek") {
				t.Errorf("plan missing NodeByIndexSeek:\n%s", er.Plan)
			}
		})
	}

	// An un-indexed property must fall back to a full label scan.
	body := `{"query":"MATCH (c:Component) WHERE c.name = $n RETURN c","params":{"n":"router.go"}}`
	st, raw := do(t, c, http.MethodPost, ts.URL+"/explain", body)
	if st != http.StatusOK {
		t.Fatalf("explain (scan) status = %d, want 200 (%s)", st, raw)
	}
	var er explainResponse
	mustJSON(t, raw, &er)
	if er.UsesIndexSeek {
		t.Errorf("uses_index_seek = true for un-indexed property, want false; plan:\n%s", er.Plan)
	}
	if !strings.Contains(er.Plan, "NodeByLabelScan") {
		t.Errorf("plan missing NodeByLabelScan (expected scan fallback):\n%s", er.Plan)
	}
}

// TestSchemaSurvivesReopen proves the schema is durable across an in-process
// close/reopen: after reopening the same data directory, the UNIQUE constraint
// still rejects a duplicate and the index seek still fires. This exercises the
// NewEngineWithStoreAndSchema wiring in openStore — the plain NewEngineWithStore
// would silently lose both after restart.
func TestSchemaSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	ds1, err := openStore(ctx, dir)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if _, err := ds1.seed(ctx, synthScale{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ds1.snapshotNow(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := ds1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	ds2, err := openStore(ctx, dir)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	t.Cleanup(func() { _ = ds2.Close() })

	// Constraint still enforced.
	res, err := ds2.engine.RunAny(ctx, "CREATE (c:Component:Code {key:'comp:platform/config.go'})", nil)
	if err == nil {
		for res.Next() {
		}
		err = res.Err()
		_ = res.Close()
	}
	if err == nil {
		t.Error("duplicate Component.key accepted after reopen: constraint not recovered")
	}

	// Index seek still planned.
	plan, err := ds2.engine.Explain(explainDemoQuery, nil)
	if err != nil {
		t.Fatalf("explain after reopen: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("index seek not planned after reopen; index not recovered:\n%s", plan)
	}
}

// componentCount returns the Component node count via GET /stats.
func componentCount(t *testing.T, c *http.Client, baseURL string) int64 {
	t.Helper()
	st, raw := do(t, c, http.MethodGet, baseURL+"/stats", "")
	if st != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", st)
	}
	var sr statsResponse
	mustJSON(t, raw, &sr)
	return sr.Nodes[typeComponent]
}
