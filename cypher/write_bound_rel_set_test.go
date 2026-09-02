package cypher_test

// write_bound_rel_set_test.go — regression battery for rmp #2705: a SET or
// REMOVE on a relationship variable bound by a WRITE clause (CREATE / MERGE)
// was silently discarded.
//
// # The defect
//
// Three write operators published the emitted RelationshipValue's ID as a
// synthetic `src<<32|dst` packing of the endpoint NodeIDs:
// CreateRelationship, MergeRelationship.emitRow and MergePattern.emitRow.
// Every consumer of a post-projection relationship binding reads that field AS
// the stable per-edge handle (cypher/exec/set.go, set_all.go, remove.go,
// merge_outer_target.go), and `id(r)` returns it verbatim. So a standalone
// `SET r.k = v` mirrored its write into a by-handle property bag keyed by the
// packing — a bag no read ever consults, because the read path routes a bound
// relationship's properties EXCLUSIVELY by the handle stamped on its adjacency
// slot. The per-pair write it made alongside was then shadowed by that
// exclusive routing, so the value came back null. Silent: no error, no
// notification, and Counters().PropertiesSet reported the write as done.
//
// # Why the openCypher TCK did not catch it
//
// Across all 220 feature files, every scenario that binds a relationship in
// MERGE and later writes to it uses the ON CREATE / ON MATCH form, which
// targets the edge through a different code path and always worked. No
// scenario exercises a standalone SET / REMOVE on a write-clause-bound
// relationship variable, so the shape sits outside TCK-covered semantics and
// the 3897/3897 compliance claim and this defect were consistent.
//
// # The invariant these tests pin
//
// A relationship binding's ID is the stable per-edge handle THE ADJACENCY
// ACTUALLY HOLDS for that edge — the handle the write operator recorded its own
// by-handle type and property metadata under, and the handle a MATCH of the
// same edge resolves. That single identity is what makes a write land where a
// read looks.
//
// Layer: short. Every engine and graph is local, so the suite is goleak-clean.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
)

// wbrGraph builds the storage the Cypher engine requires: directed, and a
// multigraph (openCypher's data model — every CREATE adds a relationship).
func wbrGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

// wbrEngine pairs a fresh graph with an in-memory engine over it.
func wbrEngine() (*cypher.Engine, *lpg.Graph[string, float64]) {
	g := wbrGraph()
	return cypher.NewEngine(g), g
}

// wbrWrite runs one write statement in its own auto-commit transaction and
// drains it, failing the test on any error.
func wbrWrite(t *testing.T, eng *cypher.Engine, query string, params map[string]any) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, params)
	if err != nil {
		t.Fatalf("RunInTxAny(%q): %v", query, err)
	}
	for res.Next() { //nolint:revive // drain to run the write to completion
	}
	if err := res.Err(); err != nil {
		t.Fatalf("RunInTxAny(%q) result error: %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
}

// wbrScalars runs a read query and returns the named column of every row,
// rendered with fmtAny.
func wbrScalars(t *testing.T, eng *cypher.Engine, query, col string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunAny(%q): %v", query, err)
	}
	rows := collectRecords(t, res)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmtAny(row[col]))
	}
	return out
}

// wbrNodeKey resolves the synthetic storage key of the node whose `n` property
// equals val. CREATE / MERGE mint synthetic keys, so the user-facing identity
// is the property.
func wbrNodeKey(t *testing.T, g *lpg.Graph[string, float64], val string) string {
	t.Helper()
	var found string
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, k string) bool {
		if pv, ok := g.NodeProperties(k)["n"]; ok {
			if s, sok := pv.String(); sok && s == val {
				found = k
				return false
			}
		}
		return true
	})
	if found == "" {
		t.Fatalf("no node with n=%q", val)
	}
	return found
}

// wbrSliceHandles returns every stable handle the ADJACENCY holds for the
// ordered pair (a -> b), in slot order. This is the set of identities a MATCH
// can resolve; a by-handle bag keyed outside this set is unreachable.
func wbrSliceHandles(t *testing.T, g *lpg.Graph[string, float64], srcKey, dstKey string) []uint64 {
	t.Helper()
	srcID, ok := g.AdjList().Mapper().Lookup(srcKey)
	if !ok {
		t.Fatalf("src key %q not interned", srcKey)
	}
	dstID, ok := g.AdjList().Mapper().Lookup(dstKey)
	if !ok {
		t.Fatalf("dst key %q not interned", dstKey)
	}
	var out []uint64
	g.WalkEdgeHandles(func(tr lpg.EdgeHandleTriple) bool {
		if tr.Src == srcID && tr.Dst == dstID {
			out = append(out, tr.Handle)
		}
		return true
	})
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// The write must land, for every shape that binds the relationship in a write
// clause
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteBoundRelationship_SetIsNotLost is the primary oracle: after each
// statement, a MATCH with an inline property predicate must find the edge.
//
// The oracle is not vacuous — the identical assertion PASSES on the shapes the
// defect never touched (`ON CREATE SET`, `MATCH … SET`, inline properties),
// which are covered by the `control_*` cases below. Every non-control case
// FAILED before the three producers were fixed.
func TestWriteBoundRelationship_SetIsNotLost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		setup  string
		stmt   string
		params map[string]any
		// extra is an optional second assertion run after the statement, used
		// by the mixed node+relationship clause to prove BOTH items landed.
		extra string
	}{
		// MergeRelationship / MergePattern — the reported shapes.
		{name: "merge_create_branch_fresh_endpoints",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`},
		{name: "merge_create_branch_bound_endpoints", setup: `CREATE (:P {n:'a'}), (:P {n:'b'})`,
			stmt: `MATCH (a:P {n:'a'}), (b:P {n:'b'}) MERGE (a)-[r:T]->(b) SET r.since = 2020`},
		{name: "merge_match_branch", setup: `CREATE (:P {n:'a'})-[:T]->(:P {n:'b'})`,
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`},
		{name: "merge_match_branch_bound_endpoints", setup: `CREATE (:P {n:'a'})-[:T]->(:P {n:'b'})`,
			stmt: `MATCH (a:P {n:'a'}), (b:P {n:'b'}) MERGE (a)-[r:T]->(b) SET r.since = 2020`},

		// Plain CREATE — no MERGE anywhere. This case is what refutes the
		// "MERGE defect" framing: the identity, not MERGE, is the fault.
		{name: "create_then_set",
			stmt: `CREATE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`},
		{name: "create_bound_endpoints_then_set", setup: `CREATE (:P {n:'a'}), (:P {n:'b'})`,
			stmt: `MATCH (a:P {n:'a'}), (b:P {n:'b'}) CREATE (a)-[r:T]->(b) SET r.since = 2020`},

		// Whole-entity SET forms (SetAllProperties).
		{name: "merge_set_plus_equals",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r += {since: 2020}`},
		{name: "merge_set_replace",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r = {since: 2020}`},
		{name: "create_set_plus_equals",
			stmt: `CREATE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r += {since: 2020}`},

		// Across a WITH boundary: the binding is re-projected before the SET.
		{name: "merge_with_then_set",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) WITH r SET r.since = 2020`},
		{name: "create_with_then_set",
			stmt: `CREATE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) WITH r SET r.since = 2020`},

		// Multi-hop pattern — routes through MergePattern rather than
		// MergeRelationship, on both its create and its match branch.
		{name: "merge_pattern_create_branch",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'})-[:U]->(c:P {n:'c'}) SET r.since = 2020`},
		{name: "merge_pattern_match_branch", setup: `CREATE (:P {n:'a'})-[:T]->(:P {n:'b'})-[:U]->(:P {n:'c'})`,
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'})-[:U]->(c:P {n:'c'}) SET r.since = 2020`},

		// Right-hand-side shapes: literal (above), parameter, expression.
		{name: "param_rhs",
			stmt:   `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = $v`,
			params: map[string]any{"v": 2020}},
		{name: "expression_rhs",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2000 + 20`},
		{name: "param_rhs_set_plus_equals",
			stmt:   `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r += {since: $v}`,
			params: map[string]any{"v": 2020}},

		// One clause, one row, a node item AND a relationship item. This is
		// the tightest localisation of the defect: the node item landed and
		// the relationship item was lost.
		{name: "mixed_node_and_relationship_items",
			stmt:  `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET a.since = 1, r.since = 2020`,
			extra: `MATCH (a:P {n:'a'}) WHERE a.since = 1 RETURN count(a) AS n`},
		{name: "mixed_relationship_first",
			stmt:  `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020, a.since = 1`,
			extra: `MATCH (a:P {n:'a'}) WHERE a.since = 1 RETURN count(a) AS n`},

		// Controls — shapes the defect never reached. They keep the oracle
		// honest: it must be able to PASS, and it must pass here on the
		// defective build too.
		{name: "control_on_create_set",
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) ON CREATE SET r.since = 2020`},
		{name: "control_on_match_set", setup: `CREATE (:P {n:'a'})-[:T]->(:P {n:'b'})`,
			stmt: `MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) ON MATCH SET r.since = 2020`},
		{name: "control_inline_properties",
			stmt: `MERGE (a:P {n:'a'})-[r:T {since: 2020}]->(b:P {n:'b'})`},
		{name: "control_match_then_set", setup: `CREATE (:P {n:'a'})-[:T]->(:P {n:'b'})`,
			stmt: `MATCH (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, _ := wbrEngine()
			ctx := context.Background()

			if c.setup != "" {
				wbrWrite(t, eng, c.setup, nil)
			}
			wbrWrite(t, eng, c.stmt, c.params)

			assertCount(ctx, t, eng,
				`MATCH (:P {n:'a'})-[r:T {since: 2020}]->(:P {n:'b'}) RETURN count(r) AS n`, 1)
			if c.extra != "" {
				assertCount(ctx, t, eng, c.extra, 1)
			}
		})
	}
}

// TestWriteBoundRelationship_SetInsideExplicitTransaction repeats the primary
// shape inside a caller-driven explicit transaction rather than the
// one-statement auto-commit form, so the fix is not an artefact of the
// auto-commit commit path.
func TestWriteBoundRelationship_SetInsideExplicitTransaction(t *testing.T) {
	t.Parallel()
	eng, _ := wbrEngine()
	ctx := context.Background()

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.ExecAny(`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("ExecAny: %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("result error: %v", err)
	}
	if err := res.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	assertCount(ctx, t, eng,
		`MATCH (:P {n:'a'})-[r:T {since: 2020}]->(:P {n:'b'}) RETURN count(r) AS n`, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// REMOVE and SET-to-null
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteBoundRelationship_RemoveIsNotLost covers the removal side. With a
// synthetic id, `delRelProp` / the RemoveProperty operator handed the bogus
// value to DelEdgePropertyOnInstance, which removed the property from the
// per-pair store and from the ORPHAN bag — leaving the real handle's bag,
// which reads route to, still holding the old value. The removal was therefore
// invisible.
func TestWriteBoundRelationship_RemoveIsNotLost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup string
		stmt  string
	}{
		{"remove_on_merge_match_branch", `CREATE (:P {n:'a'})-[:T {since: 99}]->(:P {n:'b'})`,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) REMOVE r.since`},
		{"remove_on_merge_create_branch", ``,
			`MERGE (a:P {n:'a'})-[r:T {since: 99}]->(b:P {n:'b'}) REMOVE r.since`},
		{"remove_on_create_branch", ``,
			`CREATE (a:P {n:'a'})-[r:T {since: 99}]->(b:P {n:'b'}) REMOVE r.since`},
		{"remove_across_with", `CREATE (:P {n:'a'})-[:T {since: 99}]->(:P {n:'b'})`,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) WITH r REMOVE r.since`},
		{"set_null_on_merge_match_branch", `CREATE (:P {n:'a'})-[:T {since: 99}]->(:P {n:'b'})`,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = null`},
		{"remove_on_merge_pattern", `CREATE (:P {n:'a'})-[:T {since: 99}]->(:P {n:'b'})-[:U]->(:P {n:'c'})`,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'})-[:U]->(c:P {n:'c'}) REMOVE r.since`},
		// Control: the MATCH-bound form, which always worked.
		{"control_remove_on_match", `CREATE (:P {n:'a'})-[:T {since: 99}]->(:P {n:'b'})`,
			`MATCH (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) REMOVE r.since`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, _ := wbrEngine()
			ctx := context.Background()

			if c.setup != "" {
				wbrWrite(t, eng, c.setup, nil)
			}
			wbrWrite(t, eng, c.stmt, nil)

			// The property must be gone on the bound instance, and the edge
			// itself must still be there (a removal, not a deletion).
			assertCount(ctx, t, eng,
				`MATCH (:P {n:'a'})-[r:T]->(:P {n:'b'}) WHERE r.since IS NULL RETURN count(r) AS n`, 1)
			assertCount(ctx, t, eng,
				`MATCH (:P {n:'a'})-[r:T]->(:P {n:'b'}) RETURN count(r) AS n`, 1)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Read-your-own-writes inside the writing statement
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteBoundRelationship_SetVisibleInSameStatement pins the second half of
// the defect: with the durable write fixed, `SET r.k = v RETURN r.k` still read
// null, because the row slot a WRITE clause materialises carries a property
// snapshot taken BEFORE the SET and a projection never re-reads the graph for
// that shape. The node arm of the refresh had always existed
// ([SetProperty.refreshNodeRowProperties]) — which is precisely why the same
// clause landed for `SET a.since = 1` and read back stale for `SET r.since`.
func TestWriteBoundRelationship_SetVisibleInSameStatement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		stmt string
	}{
		{"merge_single_property",
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020 RETURN r.since AS v, properties(r) AS p`},
		{"create_single_property",
			`CREATE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020 RETURN r.since AS v, properties(r) AS p`},
		{"merge_pattern_single_property",
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'})-[:U]->(c:P {n:'c'}) SET r.since = 2020 RETURN r.since AS v, properties(r) AS p`},
		{"merge_set_plus_equals",
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r += {since: 2020} RETURN r.since AS v, properties(r) AS p`},
		{"merge_set_replace",
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r = {since: 2020} RETURN r.since AS v, properties(r) AS p`},
		{"merge_across_with",
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) WITH r SET r.since = 2020 RETURN r.since AS v, properties(r) AS p`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, _ := wbrEngine()

			res, err := eng.RunInTxAny(context.Background(), c.stmt, nil)
			if err != nil {
				t.Fatalf("RunInTxAny: %v", err)
			}
			rows := collectRecords(t, res)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1", len(rows))
			}
			if got := fmtAny(rows[0]["v"]); got != "2020" {
				t.Errorf("same-statement r.since = %s, want 2020", got)
			}
			// properties(r) must be non-empty; an empty map renders as "{}".
			if got := fmtAny(rows[0]["p"]); got == "{}" || got == "<nil>" {
				t.Errorf("same-statement properties(r) = %s, want non-empty", got)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// id(r) agreement
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteBoundRelationship_IDAgreesWithMatch pins the user-visible secondary
// symptom: `MERGE (a)-[r:T]->(b) RETURN id(r)` returned the synthetic packing
// (a value like 721554505869) where the same relationship read back through
// MATCH reported its handle. fnID returns RelationshipValue.ID verbatim, so the
// two disagreed for the same edge.
func TestWriteBoundRelationship_IDAgreesWithMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup string
		write string
	}{
		{"merge_create_branch", ``,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) RETURN id(r) AS i`},
		{"merge_match_branch", `CREATE (:P {n:'a'})-[:T]->(:P {n:'b'})`,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) RETURN id(r) AS i`},
		{"create", ``,
			`CREATE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) RETURN id(r) AS i`},
		{"merge_pattern", ``,
			`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'})-[:U]->(c:P {n:'c'}) RETURN id(r) AS i`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, g := wbrEngine()

			if c.setup != "" {
				wbrWrite(t, eng, c.setup, nil)
			}
			res, err := eng.RunInTxAny(context.Background(), c.write, nil)
			if err != nil {
				t.Fatalf("RunInTxAny: %v", err)
			}
			rows := collectRecords(t, res)
			if len(rows) != 1 {
				t.Fatalf("write returned %d rows, want 1", len(rows))
			}
			fromWrite := fmtAny(rows[0]["i"])

			fromMatch := wbrScalars(t, eng,
				`MATCH (:P {n:'a'})-[r:T]->(:P {n:'b'}) RETURN id(r) AS i`, "i")
			if len(fromMatch) != 1 {
				t.Fatalf("MATCH returned %d rows, want 1", len(fromMatch))
			}
			if fromWrite != fromMatch[0] {
				t.Errorf("id(r) from write = %s, from MATCH = %s; the two must agree",
					fromWrite, fromMatch[0])
			}

			// And the id must be an identity the ADJACENCY actually holds,
			// not merely self-consistent between two row builders.
			slotHandles := wbrSliceHandles(t, g,
				wbrNodeKey(t, g, "a"), wbrNodeKey(t, g, "b"))
			if len(slotHandles) != 1 {
				t.Fatalf("pair a->b holds %d handled slots, want 1", len(slotHandles))
			}
			if want := fmtAny(int64(slotHandles[0])); fromWrite != want {
				t.Errorf("id(r) = %s, but the adjacency slot holds handle %s", fromWrite, want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// No orphan by-handle entry
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteBoundRelationship_SetLeavesNoOrphanByHandleEntry asserts directly
// against the graph, not through Cypher, that the SET wrote to the instance the
// adjacency holds and to nothing else.
//
// Two clauses, and both are load-bearing:
//
//   - the bag under the handle the adjacency holds carries the value (the write
//     reached the instance reads resolve), and
//   - the bag under the old synthetic packing is EMPTY (the orphan this defect
//     created on every such SET is gone).
//
// The second clause is what fails on the defective build. It also fixes the
// scope of the "do existing stores need migrating?" question: with no new
// orphans created, any orphan already in a store is unreachable — no adjacency
// slot carries the packed value, so no read can route to it, and the snapshot
// writer enumerates live slot handles ([lpg.Graph.WalkEdgeHandlesAsOf]) rather
// than the by-handle store, so the next snapshot does not carry it forward.
func TestWriteBoundRelationship_SetLeavesNoOrphanByHandleEntry(t *testing.T) {
	t.Parallel()

	for _, stmt := range []string{
		`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`,
		`CREATE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r.since = 2020`,
		`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'}) SET r += {since: 2020}`,
		`MERGE (a:P {n:'a'})-[r:T]->(b:P {n:'b'})-[:U]->(c:P {n:'c'}) SET r.since = 2020`,
	} {
		t.Run(stmt, func(t *testing.T) {
			t.Parallel()
			eng, g := wbrEngine()
			wbrWrite(t, eng, stmt, nil)

			srcKey, dstKey := wbrNodeKey(t, g, "a"), wbrNodeKey(t, g, "b")
			srcID, _ := g.AdjList().Mapper().Lookup(srcKey)
			dstID, _ := g.AdjList().Mapper().Lookup(dstKey)

			handles := wbrSliceHandles(t, g, srcKey, dstKey)
			if len(handles) != 1 {
				t.Fatalf("pair a->b holds %d handled slots, want 1", len(handles))
			}
			bag := g.EdgePropertiesByHandleID(srcID, dstID, handles[0])
			pv, ok := bag["since"]
			if !ok {
				t.Fatalf("handle %d carries no `since`; bag = %v", handles[0], bag)
			}
			if iv, iok := pv.Int64(); !iok || iv != 2020 {
				t.Errorf("handle %d carries since = %v, want 2020", handles[0], pv)
			}

			// The synthetic identity the three producers used to publish. It is
			// not, and must never be, a key in the by-handle store.
			packed := uint64(srcID)<<32 | uint64(dstID)
			if orphan := g.EdgePropertiesByHandleID(srcID, dstID, packed); len(orphan) != 0 {
				t.Errorf("orphan by-handle bag under the synthetic packing %d holds %v; "+
					"the write must target the handle the adjacency holds", packed, orphan)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Relationship identity: Equal / Hash / DISTINCT
// ─────────────────────────────────────────────────────────────────────────────

// TestParallelCreatedRelationships_HaveDistinctIdentity covers the first of the
// two identity risks the fix carries, and shows the change resolves a
// PRE-EXISTING identity defect rather than introducing one.
//
// [expr.RelationshipValue.Equal] and Hash are computed purely from ID. With the
// synthetic packing, two parallel CREATEs between the same ordered pair
// published the SAME id and therefore compared EQUAL — so `r1 = r2` was true
// for two distinct relationships and DISTINCT collapsed them into one row.
// openCypher relationship identity is per relationship, not per endpoint pair.
func TestParallelCreatedRelationships_HaveDistinctIdentity(t *testing.T) {
	t.Parallel()

	t.Run("distinct_keeps_both_parallel_edges", func(t *testing.T) {
		t.Parallel()
		eng, _ := wbrEngine()
		wbrWrite(t, eng, `CREATE (:P {n:'a'}), (:P {n:'b'})`, nil)
		wbrWrite(t, eng, `MATCH (a:P {n:'a'}), (b:P {n:'b'}) CREATE (a)-[:T {k: 1}]->(b)`, nil)
		wbrWrite(t, eng, `MATCH (a:P {n:'a'}), (b:P {n:'b'}) CREATE (a)-[:T {k: 2}]->(b)`, nil)

		ctx := context.Background()
		assertCount(ctx, t, eng,
			`MATCH (:P {n:'a'})-[r:T]->(:P {n:'b'}) RETURN count(r) AS n`, 2)
		assertCount(ctx, t, eng,
			`MATCH (:P {n:'a'})-[r:T]->(:P {n:'b'}) RETURN count(DISTINCT r) AS n`, 2)
		assertCount(ctx, t, eng,
			`MATCH (:P {n:'a'})-[r:T]->(:P {n:'b'}) WITH DISTINCT r RETURN count(r) AS n`, 2)
	})

	t.Run("two_creates_in_one_statement_get_distinct_ids", func(t *testing.T) {
		t.Parallel()
		eng, _ := wbrEngine()

		res, err := eng.RunInTxAny(context.Background(),
			`CREATE (a:P {n:'a'}), (b:P {n:'b'})
			 CREATE (a)-[r1:T]->(b)
			 CREATE (a)-[r2:T]->(b)
			 RETURN id(r1) AS i1, id(r2) AS i2, (r1 = r2) AS same`, nil)
		if err != nil {
			t.Fatalf("RunInTxAny: %v", err)
		}
		rows := collectRecords(t, res)
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		i1, i2 := fmtAny(rows[0]["i1"]), fmtAny(rows[0]["i2"])
		if i1 == i2 {
			t.Errorf("two parallel CREATEs published the same id(r) = %s; "+
				"two distinct relationships must not share an identity", i1)
		}
		if got := fmtAny(rows[0]["same"]); got != "false" {
			t.Errorf("(r1 = r2) = %s, want false: they are distinct relationships", got)
		}
	})

	t.Run("set_on_one_parallel_edge_does_not_touch_its_sibling", func(t *testing.T) {
		t.Parallel()
		eng, _ := wbrEngine()
		wbrWrite(t, eng, `CREATE (:P {n:'a'}), (:P {n:'b'})`, nil)
		// Two parallel edges, each SET through its own write-clause binding.
		wbrWrite(t, eng,
			`MATCH (a:P {n:'a'}), (b:P {n:'b'}) CREATE (a)-[r:T]->(b) SET r.since = 2020`, nil)
		wbrWrite(t, eng,
			`MATCH (a:P {n:'a'}), (b:P {n:'b'}) CREATE (a)-[r:T]->(b) SET r.since = 1999`, nil)

		ctx := context.Background()
		assertCount(ctx, t, eng,
			`MATCH (:P {n:'a'})-[r:T {since: 2020}]->(:P {n:'b'}) RETURN count(r) AS n`, 1)
		assertCount(ctx, t, eng,
			`MATCH (:P {n:'a'})-[r:T {since: 1999}]->(:P {n:'b'}) RETURN count(r) AS n`, 1)
	})
}

// TestMergeCreateMultiplicityRowsSurviveDedup covers the second identity risk.
//
// MERGE must emit one row per CREATE statement that targeted the pair, not one
// row per distinct storage entry (the Merge5 [3] contract, enforced by
// [GraphMutator.EdgeCreateCount]). Those multiplicity rows are re-emissions of
// ONE bound edge, so they legitimately share an identity — and they did under
// the synthetic packing too, which is why the change is identity-neutral here.
// The test pins it explicitly, since a dedup that keyed on the new id could
// otherwise collapse the rows and silently break the contract.
func TestMergeCreateMultiplicityRowsSurviveDedup(t *testing.T) {
	t.Parallel()
	eng, _ := wbrEngine()
	ctx := context.Background()

	wbrWrite(t, eng, `CREATE (:A {n:'a'}), (:B {n:'b'})`, nil)
	wbrWrite(t, eng, `MATCH (a:A), (b:B) CREATE (a)-[:T]->(b)`, nil)
	wbrWrite(t, eng, `MATCH (a:A), (b:B) CREATE (a)-[:T]->(b)`, nil)

	// Two CREATEs targeted the pair, so MERGE binds and re-emits two rows.
	wbrWrite(t, eng, `MATCH (a:A), (b:B) MERGE (a)-[r:T]->(b)`, nil)
	res, err := eng.RunInTxAny(ctx, `MATCH (a:A), (b:B) MERGE (a)-[r:T]->(b) RETURN count(r) AS n`, nil)
	if err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := fmtAny(rows[0]["n"]); got != "2" {
		t.Errorf("MERGE multiplicity count(r) = %s, want 2 (one row per CREATE, Merge5 [3])", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Durability: the value must survive commit AND a WAL close/reopen replay
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteBoundRelationship_SetSurvivesReopen drives the durable engine
// (recovery.Open + wal + txn.Store) so the SET travels the WAL rather than the
// in-memory mutator alone, then reads the value back through a freshly reopened
// engine.
//
// Two recovery routes, because the by-handle property store is persisted by two
// independent encoders and a write that lands in only one of them would still
// read back correctly through the other:
//
//   - snap=false — the WAL is fsynced and replayed on the next open, so the
//     value is reconstructed from the OpSetEdgeProperty{,ByHandle} frames.
//   - snap=true — a full snapshot is written and the WAL truncated, so the
//     value is reconstructed from edgehandles.bin, whose writer enumerates LIVE
//     adjacency slot handles ([lpg.Graph.WalkEdgeHandlesAsOf]). A write mirrored
//     under a handle no slot carries is therefore not merely unreadable, it is
//     not even persisted — which is why this arm is the stricter of the two.
func TestWriteBoundRelationship_SetSurvivesReopen(t *testing.T) {
	t.Parallel()

	for _, snap := range []bool{false, true} {
		name := "wal_replay"
		if snap {
			name = "snapshot_truncated_wal"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			// Write cycle 1: create the pair and SET through the write-clause
			// binding, then persist and close.
			bhWriteCycle(t, dir, snap,
				`MERGE (a:N {key:'x'})-[r:T]->(b:N {key:'y'}) SET r.since = 2020`)

			// Reopen from disk and read the value back through Cypher.
			res, err := recovery.Open[string, float64](dir, bhRecOpts())
			if err != nil {
				t.Fatalf("recovery.Open: %v", err)
			}
			eng := cypher.NewEngine(res.Graph)
			got := wbrScalars(t, eng,
				`MATCH (:N {key:'x'})-[r:T]->(:N {key:'y'}) RETURN r.since AS v`, "v")
			if len(got) != 1 || got[0] != "2020" {
				t.Fatalf("after reopen r.since = %v, want [2020]", got)
			}

			// And the value must sit under the handle the reopened adjacency
			// holds — proving it is readable by identity, not merely present in
			// the coalesced per-pair aggregate.
			perInstance := byHandleValuesForPair(t, dir, "x", "y")
			if len(perInstance) != 1 {
				t.Fatalf("pair x->y has %d handled instances after reopen, want 1", len(perInstance))
			}
			for h, props := range perInstance {
				pv, ok := props["since"]
				if !ok {
					t.Fatalf("handle %d carries no `since` after reopen; bag = %v", h, props)
				}
				if iv, iok := pv.Int64(); !iok || iv != 2020 {
					t.Errorf("handle %d carries since = %v after reopen, want 2020", h, pv)
				}
			}
		})
	}
}
