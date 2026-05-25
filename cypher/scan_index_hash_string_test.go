package cypher_test

// scan_index_hash_string_test.go — T601: hash index seek on string equality.
//
// Uses newPersonGraph (defined in index_seek_test.go) which builds n Person
// nodes named "Person0".."Person{n-1}" plus one "Alice", and optionally
// installs the string hash index "person_name_hash" on the "name" property.

import (
	"context"
	"strings"
	"testing"

	"gograph/cypher/expr"
)

// TestScanIndexHash_AliceFound verifies that querying for Alice by name via a
// hash index returns exactly one row with name "Alice".
func TestScanIndexHash_AliceFound(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, true)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person {name: "Alice"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("Alice: want 1 row, got %d", len(rows))
	}
	v, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok {
		t.Fatalf("n.name: expected StringValue, got %T (%v)", rows[0]["n.name"], rows[0]["n.name"])
	}
	if string(v) != "Alice" {
		t.Errorf("n.name: want Alice, got %q", string(v))
	}
}

// TestScanIndexHash_Person42Found verifies that querying for "Person42" returns
// exactly one row with the correct name.
func TestScanIndexHash_Person42Found(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, true)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person {name: "Person42"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("Person42: want 1 row, got %d", len(rows))
	}
	v, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok {
		t.Fatalf("n.name: expected StringValue, got %T", rows[0]["n.name"])
	}
	if string(v) != "Person42" {
		t.Errorf("n.name: want Person42, got %q", string(v))
	}
}

// TestScanIndexHash_NotInGraph verifies that querying for a name that does not
// exist in the graph returns zero rows.
func TestScanIndexHash_NotInGraph(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, true)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person {name: "NotInGraph"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)
	if len(rows) != 0 {
		t.Errorf("NotInGraph: want 0 rows, got %d", len(rows))
	}
}

// TestScanIndexHash_UsesIndexSeekPlan verifies that the query planner uses
// NodeByIndexSeek (not a full label scan) when a hash index is present.
func TestScanIndexHash_UsesIndexSeekPlan(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(100, true)

	plan, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("expected NodeByIndexSeek in plan; got:\n%s", plan)
	}
}
