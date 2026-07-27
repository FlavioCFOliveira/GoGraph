package cypher_test

// merge_multimatch_invariant_test.go — the invariant that MERGE binds EVERY
// matching node, pinned before the MERGE access path is changed (task #2217,
// acceptance criterion 2).
//
// WHY THIS EXISTS. An earlier version of #2217 claimed that stopping the merge
// search at the first match was a one-line unconditional win. Measurement
// refuted it: MERGE binds every match, exactly as MATCH does, so an early exit
// would silently DROP rows and break openCypher semantics. The claim was
// withdrawn from the audit before implementation.
//
// The access-path work that follows narrows WHICH candidate nodes are examined
// (label posting list, then index seek) while still enumerating all of them.
// That is only safe if the multi-match property is nailed down first, which is
// what this file does. If a future change reintroduces an early exit, these
// tests fail rather than silently returning fewer rows.
//
// openCypher: MERGE first attempts to match the whole pattern; when the match
// succeeds it binds the matched rows and fires ON MATCH for each. Only a total
// absence of matches fires ON CREATE.

import (
	"context"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newMergeMultiEngine returns an engine holding k nodes that all match
// (:X {v: 1}), plus two decoys that must never be bound: a :X with a different
// property value, and a :Y carrying the same property value.
func newMergeMultiEngine(t *testing.T, k int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for i := 0; i < k; i++ {
		runSetup(t, eng, `CREATE (:X {v: 1, tag: 'match'})`)
	}
	runSetup(t, eng, `CREATE (:X {v: 2, tag: 'wrong-value'})`)
	runSetup(t, eng, `CREATE (:Y {v: 1, tag: 'wrong-label'})`)
	return eng
}

// TestMergeBindsEveryMatch asserts that a MERGE whose pattern matches k nodes
// yields k rows — not one.
func TestMergeBindsEveryMatch(t *testing.T) {
	t.Parallel()

	for _, k := range []int{1, 2, 3, 5} {
		t.Run("k="+string(rune('0'+k)), func(t *testing.T) {
			t.Parallel()
			eng := newMergeMultiEngine(t, k)

			res, err := eng.RunInTx(context.Background(), `MERGE (n:X {v: 1}) RETURN n.tag AS tag`, nil)
			if err != nil {
				t.Fatalf("MERGE: %v", err)
			}
			rows := collectRecords(t, res)
			if len(rows) != k {
				t.Fatalf("MERGE over %d matching nodes bound %d rows, want %d; an early exit in the merge search would look exactly like this", k, len(rows), k)
			}
			for _, row := range rows {
				if tag, ok := row["tag"].(expr.StringValue); !ok || string(tag) != "match" {
					t.Errorf("MERGE bound a decoy node: tag = %v, want \"match\"", row["tag"])
				}
			}
		})
	}
}

// TestMergeOnMatchAppliesToEveryMatch asserts that ON MATCH SET is applied to
// all k matched nodes, not just the first. This is the observable that a
// dropped row would corrupt silently: the query still succeeds, but some nodes
// keep their old value.
func TestMergeOnMatchAppliesToEveryMatch(t *testing.T) {
	t.Parallel()

	const k = 4
	eng := newMergeMultiEngine(t, k)

	res, err := eng.RunInTx(context.Background(),
		`MERGE (n:X {v: 1}) ON MATCH SET n.seen = true RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("MERGE ON MATCH: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("MERGE ON MATCH iterate: %v", err)
	}
	_ = res.Close()

	// Every matching node must carry seen = true.
	got := collectColumn(t, eng, `MATCH (n:X {v: 1}) WHERE n.seen = true RETURN n.tag AS name`, "name")
	if len(got) != k {
		t.Errorf("ON MATCH SET applied to %d of %d matched nodes; every match must be updated", len(got), k)
	}

	// The decoys must be untouched.
	untouched := collectColumn(t, eng, `MATCH (n) WHERE n.seen = true RETURN n.tag AS name`, "name")
	if len(untouched) != k {
		t.Errorf("ON MATCH SET touched %d nodes in total, want %d; a decoy was bound", len(untouched), k)
	}
}

// TestMergeCreatesOnlyWhenNoMatchExists asserts the other half of the
// contract: ON CREATE fires only on a total absence of matches, and fires
// exactly once.
func TestMergeCreatesOnlyWhenNoMatchExists(t *testing.T) {
	t.Parallel()

	eng := newMergeMultiEngine(t, 3)

	// A pattern with no match must create exactly one node.
	res, err := eng.RunInTx(context.Background(),
		`MERGE (n:X {v: 99}) ON CREATE SET n.tag = 'created' RETURN n.tag AS tag`, nil)
	if err != nil {
		t.Fatalf("MERGE create: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("MERGE with no match yielded %d rows, want exactly 1", len(rows))
	}

	created := collectColumn(t, eng, `MATCH (n:X {v: 99}) RETURN n.tag AS name`, "name")
	if !slices.Equal(created, []string{"created"}) {
		t.Errorf("ON CREATE result = %v, want [created]", created)
	}

	// Re-running must MATCH the node just created, not create a second one.
	res2, err := eng.RunInTx(context.Background(), `MERGE (n:X {v: 99}) RETURN n.tag AS tag`, nil)
	if err != nil {
		t.Fatalf("MERGE re-run: %v", err)
	}
	rows2 := collectRecords(t, res2)
	if len(rows2) != 1 {
		t.Errorf("MERGE re-run yielded %d rows, want 1", len(rows2))
	}
	all := collectColumn(t, eng, `MATCH (n:X {v: 99}) RETURN n.tag AS name`, "name")
	if len(all) != 1 {
		t.Errorf("MERGE re-run created a duplicate: %d nodes with v=99, want 1", len(all))
	}
}

// TestMergeMultiMatchLabelsOnly and the property-only variant cover the two
// degenerate pattern shapes, so the access-path change cannot regress a pattern
// that carries only labels or only properties.
func TestMergeMultiMatchLabelsOnly(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for i := 0; i < 3; i++ {
		runSetup(t, eng, `CREATE (:Z {i: 1})`)
	}
	runSetup(t, eng, `CREATE (:W {i: 1})`)

	res, err := eng.RunInTx(context.Background(), `MERGE (n:Z) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("MERGE labels-only: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("aggregate yielded %d rows, want 1", len(rows))
	}
	if c, ok := rows[0]["c"].(expr.IntegerValue); !ok || int64(c) != 3 {
		t.Errorf("MERGE (n:Z) bound %v nodes, want 3", rows[0]["c"])
	}
}

func TestMergeMultiMatchPropertiesOnly(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for i := 0; i < 3; i++ {
		runSetup(t, eng, `CREATE (:P1 {k: 7})`)
	}
	runSetup(t, eng, `CREATE (:P2 {k: 7})`)
	runSetup(t, eng, `CREATE (:P1 {k: 8})`)

	// No label in the pattern: every node with k = 7 matches, across labels.
	res, err := eng.RunInTx(context.Background(), `MERGE (n {k: 7}) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("MERGE properties-only: %v", err)
	}
	rows := collectRecords(t, res)
	if c, ok := rows[0]["c"].(expr.IntegerValue); !ok || int64(c) != 4 {
		t.Errorf("MERGE (n {k: 7}) bound %v nodes, want 4 (3 :P1 + 1 :P2)", rows[0]["c"])
	}
}

// TestMergeMultiMatchCrossTypeNumericEquality pins that the merge search keeps
// openCypher value equality across the integer/float divide, which an
// index-backed access path must preserve (CIP2016-06-14: 1 = 1.0).
func TestMergeMultiMatchCrossTypeNumericEquality(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:N {v: 1})`)
	runSetup(t, eng, `CREATE (:N {v: 1.0})`)

	// 1 and 1.0 are the same value, so both nodes match and nothing is created.
	res, err := eng.RunInTx(context.Background(), `MERGE (n:N {v: 1}) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("MERGE cross-type: %v", err)
	}
	rows := collectRecords(t, res)
	if c, ok := rows[0]["c"].(expr.IntegerValue); !ok || int64(c) != 2 {
		t.Errorf("MERGE (n:N {v: 1}) bound %v nodes, want 2 — 1 and 1.0 are equal", rows[0]["c"])
	}

	total := collectColumn(t, eng, `MATCH (n:N) RETURN toString(n.v) AS name`, "name")
	if len(total) != 2 {
		t.Errorf("MERGE created a node despite a cross-type match: %d :N nodes, want 2", len(total))
	}
}
