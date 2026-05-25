package cypher_test

// union_test.go — end-to-end tests for UNION and UNION ALL operators (T699).
//
// UNION / UNION ALL are parsed and translated to *ir.Union / *ir.UnionAll by
// the IR layer, but buildPlanEngine currently rejects any plan whose root is
// not *ir.ProduceResults. As a result all tests below are skipped until the
// execution engine gains UNION support.
//
// Documented behavior when implemented:
//   - UNION:     deduplicates rows across both legs (set semantics).
//   - UNION ALL: preserves all rows including duplicates (bag semantics).
//
// The skip guard uses a live Run call so the tests auto-enable once the
// engine is wired.

import (
	"context"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// isUnionUnsupportedErr returns true when the error originates from the engine
// rejecting a UNION plan root, which is the expected failure mode until UNION
// execution is implemented.
func isUnionUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "*ir.Union") || strings.Contains(s, "*ir.UnionAll") ||
		strings.Contains(s, "unsupported IR node")
}

// newUnionGraph creates a directed graph with 2 Person nodes (alice, bob) and
// 2 Movie nodes (matrix, inception). All nodes are created via RunInTx so the
// engine's CREATE path is exercised identically to the other suites.
func newUnionGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	setup := []string{
		`CREATE (:Person {name: 'alice'})`,
		`CREATE (:Person {name: 'bob'})`,
		`CREATE (:Movie {title: 'matrix'})`,
		`CREATE (:Movie {title: 'inception'})`,
	}
	for _, q := range setup {
		runSetup(t, eng, q)
	}
	return eng
}

// TestUnion_TwoBranches_Dedup verifies that UNION merges two MATCH branches
// and eliminates duplicates when the same value appears on both sides.
//
// Query:
//
//	MATCH (n:Person) RETURN n.name AS name
//	UNION
//	MATCH (m:Movie)  RETURN m.title AS name
//
// Expected: 4 distinct rows (alice, bob, matrix, inception).
func TestUnion_TwoBranches_Dedup(t *testing.T) {
	t.Parallel()
	eng := newUnionGraph(t)

	const q = `MATCH (n:Person) RETURN n.name AS name UNION MATCH (m:Movie) RETURN m.title AS name`
	res, err := eng.Run(context.Background(), q, nil)
	if isUnionUnsupportedErr(err) {
		t.Skip("UNION not yet implemented in execution engine")
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 4 {
		t.Errorf("UNION two branches: got %d rows, want 4", len(rows))
	}
}

// TestUnionAll_TwoBranches_NoDedup verifies that UNION ALL preserves all rows
// across both legs without deduplication when no duplicates exist.
//
// Query:
//
//	MATCH (n:Person) RETURN n.name AS name
//	UNION ALL
//	MATCH (m:Movie)  RETURN m.title AS name
//
// Expected: 4 rows (no duplicates in this graph, so same count as UNION).
func TestUnionAll_TwoBranches_NoDedup(t *testing.T) {
	t.Parallel()
	eng := newUnionGraph(t)

	const q = `MATCH (n:Person) RETURN n.name AS name UNION ALL MATCH (m:Movie) RETURN m.title AS name`
	res, err := eng.Run(context.Background(), q, nil)
	if isUnionUnsupportedErr(err) {
		t.Skip("UNION ALL not yet implemented in execution engine")
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 4 {
		t.Errorf("UNION ALL two branches: got %d rows, want 4", len(rows))
	}
}

// TestUnion_DedupAcrossLabels verifies that UNION removes duplicate values
// when the same string appears on both sides of the union.
//
// Graph extension: add a node with label Actor and name 'alice'. The Person
// branch and the Actor branch both produce 'alice'; UNION must deduplicate it.
//
// Expected: alice (deduped), bob, charlie, matrix, inception → 5 rows.
func TestUnion_DedupAcrossLabels(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Persons: alice, bob. Actors: alice (duplicate), charlie.
	// Movies: matrix, inception.
	for _, q := range []string{
		`CREATE (:Person {name: 'alice'})`,
		`CREATE (:Person {name: 'bob'})`,
		`CREATE (:Actor  {name: 'alice'})`,   // duplicate of alice
		`CREATE (:Actor  {name: 'charlie'})`, // unique
		`CREATE (:Movie  {title: 'matrix'})`,
		`CREATE (:Movie  {title: 'inception'})`,
	} {
		runSetup(t, eng, q)
	}

	// UNION across three branches:
	//   Person.name: alice, bob
	//   Actor.name:  alice, charlie
	//   Movie.title: matrix, inception
	// Distinct values: alice, bob, charlie, matrix, inception → 5 rows.
	const q = `
		MATCH (n:Person) RETURN n.name    AS name
		UNION
		MATCH (n:Actor)  RETURN n.name    AS name
		UNION
		MATCH (m:Movie)  RETURN m.title   AS name
	`
	res, err := eng.Run(context.Background(), q, nil)
	if isUnionUnsupportedErr(err) {
		t.Skip("UNION not yet implemented in execution engine")
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 5 {
		t.Errorf("UNION dedup across labels: got %d rows, want 5 (alice deduped)", len(rows))
	}
}

// TestUnionAll_PreservesDuplicates verifies that UNION ALL does NOT deduplicate
// when the same value appears on multiple branches.
//
// Using the same graph as TestUnion_DedupAcrossLabels:
//   - Person.name:  alice, bob  → 2 rows
//   - Actor.name:   alice, charlie → 2 rows
//   - Total UNION ALL: 4 rows (alice appears twice).
func TestUnionAll_PreservesDuplicates(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for _, q := range []string{
		`CREATE (:Person {name: 'alice'})`,
		`CREATE (:Person {name: 'bob'})`,
		`CREATE (:Actor  {name: 'alice'})`,
		`CREATE (:Actor  {name: 'charlie'})`,
	} {
		runSetup(t, eng, q)
	}

	// UNION ALL: 2 + 2 = 4 rows, alice present twice.
	const q = `
		MATCH (n:Person) RETURN n.name AS name
		UNION ALL
		MATCH (n:Actor)  RETURN n.name AS name
	`
	res, err := eng.Run(context.Background(), q, nil)
	if isUnionUnsupportedErr(err) {
		t.Skip("UNION ALL not yet implemented in execution engine")
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 4 {
		t.Errorf("UNION ALL preserve duplicates: got %d rows, want 4", len(rows))
	}
}
