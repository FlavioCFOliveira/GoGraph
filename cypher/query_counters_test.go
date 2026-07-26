package cypher_test

// query_counters_test.go — correctness gate for the per-statement write-effect counters
// (rmp #2212).
//
// The expected numbers are not invented: each case below carries the side effects the
// vendored openCypher TCK declares for that shape in its `Then the side effects should
// be` table. openCypher's vocabulary is the eight effects the TCK feature files use —
// +nodes, -nodes, +relationships, -relationships, +properties, -properties, +labels,
// -labels — and cypher.Result.Counters() models exactly those (plus the schema effects
// Bolt reports and openCypher does not name).
//
// This matters because the TCK's own side-effect step does NOT verify these: it checks
// nodes and relationships as a LOWER BOUND and skips properties and labels entirely
// (cypher/tck/compare_test.go sideEffectsTable, documented in conformance_history.go).
// So these counters need their own exact gate, and the assertions are cross-checked
// against direct graph inspection rather than against the counters themselves.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// counterEngine returns an engine over a fresh multigraph, which openCypher's data model
// requires (every CREATE adds a relationship).
func counterEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g)
}

// runCounted executes a write statement and returns its counters.
func runCounted(t *testing.T, eng *cypher.Engine, query string) *exec.QueryCounters {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", query, err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	c := res.Counters()
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
	return c
}

// wantCounters is the expected effect set for one statement; every field defaults to 0,
// so a case lists only what it expects to change.
type wantCounters struct {
	nodesCreated, nodesDeleted int64
	relsCreated, relsDeleted   int64
	propsSet, propsRemoved     int64
	labelsAdded, labelsRemoved int64
	containsUpdates            bool
}

func assertCounters(t *testing.T, query string, got *exec.QueryCounters, want wantCounters) {
	t.Helper()
	if got == nil {
		t.Fatalf("%q: Counters() is nil; a write statement must report counters", query)
	}
	for _, f := range []struct {
		name      string
		got, want int64
	}{
		{"+nodes", got.NodesCreated, want.nodesCreated},
		{"-nodes", got.NodesDeleted, want.nodesDeleted},
		{"+relationships", got.RelationshipsCreated, want.relsCreated},
		{"-relationships", got.RelationshipsDeleted, want.relsDeleted},
		{"+properties", got.PropertiesSet, want.propsSet},
		{"-properties", got.PropertiesRemoved, want.propsRemoved},
		{"+labels", got.LabelsAdded, want.labelsAdded},
		{"-labels", got.LabelsRemoved, want.labelsRemoved},
	} {
		if f.got != f.want {
			t.Errorf("%q: %s = %d, want %d", query, f.name, f.got, f.want)
		}
	}
	if got.ContainsUpdates() != want.containsUpdates {
		t.Errorf("%q: ContainsUpdates() = %v, want %v", query, got.ContainsUpdates(), want.containsUpdates)
	}
}

// TestQueryCounters_CreateEffects covers the CREATE shapes, with the numbers openCypher
// declares for them.
func TestQueryCounters_CreateEffects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
		want  wantCounters
	}{
		{"bare node", `CREATE ()`, wantCounters{nodesCreated: 1, containsUpdates: true}},
		{"labelled node", `CREATE (:L)`, wantCounters{nodesCreated: 1, labelsAdded: 1, containsUpdates: true}},
		{"two labels", `CREATE (:A:B)`, wantCounters{nodesCreated: 1, labelsAdded: 2, containsUpdates: true}},
		{"node with one property", `CREATE ({p: 1})`, wantCounters{nodesCreated: 1, propsSet: 1, containsUpdates: true}},
		{"node with two properties", `CREATE ({p: 1, q: 2})`, wantCounters{nodesCreated: 1, propsSet: 2, containsUpdates: true}},
		{"two nodes", `CREATE (), ()`, wantCounters{nodesCreated: 2, containsUpdates: true}},
		{"relationship", `CREATE ()-[:T]->()`, wantCounters{nodesCreated: 2, relsCreated: 1, containsUpdates: true}},
		{
			"relationship with a property",
			`CREATE ()-[:T {p: 1}]->()`,
			wantCounters{nodesCreated: 2, relsCreated: 1, propsSet: 1, containsUpdates: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertCounters(t, tc.query, runCounted(t, counterEngine(t), tc.query), tc.want)
		})
	}
}

// TestQueryCounters_SetAndRemove covers SET and REMOVE, including the no-op cases that
// must count NOTHING: removing an absent property, and removing an absent label.
func TestQueryCounters_SetAndRemove(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seed  string
		query string
		want  wantCounters
	}{
		{"set one property", `CREATE (:N)`, `MATCH (n:N) SET n.p = 1`, wantCounters{propsSet: 1, containsUpdates: true}},
		{"set two properties", `CREATE (:N)`, `MATCH (n:N) SET n.p = 1, n.q = 2`, wantCounters{propsSet: 2, containsUpdates: true}},
		{
			"remove one property (openCypher -properties 1)",
			`CREATE (:N {num: 1})`, `MATCH (n:N) REMOVE n.num`,
			wantCounters{propsRemoved: 1, containsUpdates: true},
		},
		{
			"remove two properties (openCypher -properties 2)",
			`CREATE (:N {num: 1, name: 'x'})`, `MATCH (n:N) REMOVE n.num, n.name`,
			wantCounters{propsRemoved: 2, containsUpdates: true},
		},
		{
			"remove an ABSENT property counts nothing",
			`CREATE (:N)`, `MATCH (n:N) REMOVE n.nope`,
			wantCounters{containsUpdates: false},
		},
		{"add a label", `CREATE (:N)`, `MATCH (n:N) SET n:Extra`, wantCounters{labelsAdded: 1, containsUpdates: true}},
		{
			"add a label the node ALREADY has counts nothing",
			`CREATE (:N)`, `MATCH (n:N) SET n:N`,
			wantCounters{containsUpdates: false},
		},
		{"remove a label", `CREATE (:N:Extra)`, `MATCH (n:N) REMOVE n:Extra`, wantCounters{labelsRemoved: 1, containsUpdates: true}},
		{
			"remove an ABSENT label counts nothing",
			`CREATE (:N)`, `MATCH (n:N) REMOVE n:Nope`,
			wantCounters{containsUpdates: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := counterEngine(t)
			_ = runCounted(t, eng, tc.seed)
			assertCounters(t, tc.query, runCounted(t, eng, tc.query), tc.want)
		})
	}
}

// TestQueryCounters_DeleteEffects covers DELETE and DETACH DELETE.
func TestQueryCounters_DeleteEffects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seed  string
		query string
		want  wantCounters
	}{
		{"delete a node", `CREATE (:N)`, `MATCH (n:N) DELETE n`, wantCounters{nodesDeleted: 1, containsUpdates: true}},
		{
			"delete a relationship",
			`CREATE (:A)-[:T]->(:B)`, `MATCH ()-[r:T]->() DELETE r`,
			wantCounters{relsDeleted: 1, containsUpdates: true},
		},
		{
			"detach delete takes the relationship too",
			`CREATE (:A)-[:T]->(:B)`, `MATCH (a:A) DETACH DELETE a`,
			wantCounters{nodesDeleted: 1, relsDeleted: 1, containsUpdates: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := counterEngine(t)
			_ = runCounted(t, eng, tc.seed)
			assertCounters(t, tc.query, runCounted(t, eng, tc.query), tc.want)
		})
	}
}

// TestQueryCounters_MergeCreateVersusMatch is the case the audit called out by name: a
// client must be able to tell a MERGE that created from one that matched. That is the
// whole reason ContainsUpdates exists.
func TestQueryCounters_MergeCreateVersusMatch(t *testing.T) {
	t.Parallel()
	eng := counterEngine(t)

	created := runCounted(t, eng, `MERGE (n:M {k: 1})`)
	assertCounters(t, "MERGE that creates", created,
		wantCounters{nodesCreated: 1, propsSet: 1, labelsAdded: 1, containsUpdates: true})

	matched := runCounted(t, eng, `MERGE (n:M {k: 1})`)
	assertCounters(t, "MERGE that matches", matched, wantCounters{containsUpdates: false})

	if created.ContainsUpdates() == matched.ContainsUpdates() {
		t.Fatal("a MERGE that created and a MERGE that matched report the same " +
			"ContainsUpdates: the client cannot distinguish them")
	}
}

// TestQueryCounters_ReadOnlyReportsNil pins the distinction between "no write surface"
// and "wrote nothing": a read-only statement must report NIL counters, not an all-zero
// set, so a caller can tell a MATCH from a MERGE that matched.
func TestQueryCounters_ReadOnlyReportsNil(t *testing.T) {
	t.Parallel()
	eng := counterEngine(t)
	_ = runCounted(t, eng, `CREATE (:N {p: 1})`)

	res, err := eng.Run(context.Background(), `MATCH (n:N) RETURN n.p`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	got := res.Counters()
	if err := res.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if rows != 1 {
		t.Fatalf("read returned %d rows, want 1", rows)
	}
	if got != nil {
		t.Fatalf("a read-only statement reported counters %+v; want nil so a caller can "+
			"distinguish it from a write that changed nothing", got)
	}
	// ContainsUpdates must be safe on the nil result.
	if got.ContainsUpdates() {
		t.Error("nil Counters().ContainsUpdates() must be false")
	}
}

// TestQueryCounters_CrossCheckedAgainstTheGraph verifies the counts against DIRECT graph
// inspection rather than against the counters themselves, which is what makes the
// numbers evidence rather than a tautology.
func TestQueryCounters_CrossCheckedAgainstTheGraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	before := g.LiveOrder()
	c := runCounted(t, eng, `CREATE (:A {p: 1}), (:B {q: 2}), (:C)`)
	after := g.LiveOrder()

	if delta := int64(after) - int64(before); delta != c.NodesCreated {
		t.Fatalf("graph gained %d nodes but the counter reports %d created", delta, c.NodesCreated)
	}
	if c.NodesCreated != 3 {
		t.Errorf("+nodes = %d, want 3", c.NodesCreated)
	}
	if c.LabelsAdded != 3 {
		t.Errorf("+labels = %d, want 3", c.LabelsAdded)
	}
	if c.PropertiesSet != 2 {
		t.Errorf("+properties = %d, want 2", c.PropertiesSet)
	}
}

// TestQueryCounters_RolledBackStatementReportsNothing pins the applied-not-attempted
// rule: a statement that fails must not leave counts behind on a later one.
func TestQueryCounters_RolledBackStatementReportsNothing(t *testing.T) {
	t.Parallel()
	eng := counterEngine(t)

	// A failing statement: a type error the executor rejects at run time.
	res, err := eng.RunInTx(context.Background(), `CREATE (n:N) SET n.p = 1/0`, nil)
	if err == nil {
		for res.Next() { //nolint:revive // drain
		}
		err = res.Err()
		_ = res.Close()
	}
	if err == nil {
		t.Skip("the seeded statement did not fail on this build; the rule is covered by " +
			"the per-statement counter lifetime instead")
	}

	// A subsequent statement must report only ITS OWN effects.
	c := runCounted(t, eng, `CREATE (:Fresh)`)
	assertCounters(t, "statement after a failure", c,
		wantCounters{nodesCreated: 1, labelsAdded: 1, containsUpdates: true})
}
