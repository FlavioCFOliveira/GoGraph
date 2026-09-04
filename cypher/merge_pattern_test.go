package cypher_test

// merge_pattern_test.go — regression coverage for rmp #1866 (2026-07-02
// production-readiness audit round 2, finding "MERGE compound-pattern
// silently drops data").
//
// Background. `MERGE (a:L1{...})-[:R]->(b:L2{...})` — the single most common
// Cypher graph-building idiom, with at least one endpoint not already
// bound — used to create ONLY the first node. The relationship and the
// second node were silently discarded, with no error. Root cause:
// cypher/ir/writes.go's mergeClause only routed to the efficient
// MergeRelationship operator when BOTH endpoints were already bound; every
// other shape fell back to exec.Merge, which is structurally single-node
// (no field for a relationship or a second node at all). No TCK scenario
// covers this shape — every Merge TCK feature file pre-binds both
// endpoints — so this is a genuine coverage gap the TCK baseline never
// caught, and every existing merge_*_test.go in this package used the same
// pre-bind idiom.
//
// Fix: a new IR node (ir.MergePattern) and physical operator
// (exec.MergePattern) implement openCypher's whole-pattern match-or-create
// semantics for any chain shape MergeRelationship's narrow both-bound fast
// path does not already cover — one or more fresh endpoints, multi-hop
// chains, and ON CREATE/ON MATCH actions targeting a node variable.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func newMergePatternEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return cypher.NewEngine(g)
}

// TestMergePattern_CreatesBothNodesAndRelationship is the audit's exact
// repro: a single-hop pattern with BOTH endpoints fresh must create both
// nodes and the relationship connecting them, not just the first node.
func TestMergePattern_CreatesBothNodesAndRelationship(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[:R]->(b:L2 {k: 'b'})`)

	assertCount(ctx, t, eng, `MATCH (n:L1 {k: 'a'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:L2 {k: 'b'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (:L1 {k: 'a'})-[:R]->(:L2 {k: 'b'}) RETURN count(*) AS n`, 1)
}

// TestMergePattern_SecondIdenticalMergeMatchesWholePattern verifies that a
// second, identical compound MERGE matches the whole pattern already
// created by the first and does not create a duplicate node or edge.
func TestMergePattern_SecondIdenticalMergeMatchesWholePattern(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[:R]->(b:L2 {k: 'b'})`)
	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[:R]->(b:L2 {k: 'b'})`)

	assertCount(ctx, t, eng, `MATCH (n:L1 {k: 'a'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:L2 {k: 'b'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (:L1 {k: 'a'})-[:R]->(:L2 {k: 'b'}) RETURN count(*) AS n`, 1)
}

// TestMergePattern_OnCreateSet verifies ON CREATE SET applies to a freshly
// created node in the compound pattern, and does NOT re-apply on a
// subsequent match of the same pattern.
func TestMergePattern_OnCreateSet(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[:R]->(b:L2 {k: 'b'}) ON CREATE SET a.created = true, b.created = true`)
	assertCount(ctx, t, eng, `MATCH (n:L1 {k: 'a', created: true}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:L2 {k: 'b', created: true}) RETURN count(n) AS n`, 1)

	// A second identical MERGE matches (does not re-create), so ON CREATE
	// SET must not fire again — assert via ON MATCH SET below instead of a
	// negative property check, since the property is already true either way.
	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[:R]->(b:L2 {k: 'b'}) ON CREATE SET a.stamp = 'create' ON MATCH SET a.stamp = 'match'`)
	assertCount(ctx, t, eng, `MATCH (n:L1 {k: 'a', stamp: 'match'}) RETURN count(n) AS n`, 1)
}

// TestMergePattern_OnMatchSet verifies ON MATCH SET applies to the nodes and
// relationship of a compound pattern when the whole pattern already exists.
func TestMergePattern_OnMatchSet(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[:R]->(b:L2 {k: 'b'})`)
	drainRunInTx(t, eng, `MERGE (a:L1 {k: 'a'})-[r:R]->(b:L2 {k: 'b'}) ON MATCH SET a.seen = true, b.seen = true, r.seen = true`)

	assertCount(ctx, t, eng, `MATCH (n:L1 {k: 'a', seen: true}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:L2 {k: 'b', seen: true}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH ()-[r:R {seen: true}]->() RETURN count(r) AS n`, 1)
}

// TestMergePattern_AttachToExistingParent_DoesNotDecompose covers the
// asymmetric "attach a fresh child to an already-bound parent" form, and the
// central MERGE trap: an existing node that satisfies the fresh endpoint's
// label/property predicate but is NOT connected to the bound parent must
// NEVER be reused. openCypher MERGE treats the whole pattern as one atomic
// unit — a partial match is not a match — so the correct outcome is a BRAND
// NEW child node connected to the parent, leaving the pre-existing
// disconnected node untouched.
func TestMergePattern_AttachToExistingParent_DoesNotDecompose(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	// Seed a Parent and a Child that satisfies the pattern predicate but is
	// deliberately left unconnected to the parent.
	drainRunInTx(t, eng, `CREATE (:Parent {id: 1}), (:Child {id: 99})`)
	assertCount(ctx, t, eng, `MATCH (n:Child {id: 99}) RETURN count(n) AS n`, 1)

	drainRunInTx(t, eng, `MATCH (p:Parent {id: 1}) MERGE (p)-[:HAS_CHILD]->(:Child {id: 99})`)

	// The whole pattern did not match (no edge from p to the existing
	// Child), so MERGE must create a SECOND Child{id:99} rather than
	// decomposing into "reuse any Child{id:99}, then add an edge".
	assertCount(ctx, t, eng, `MATCH (n:Child {id: 99}) RETURN count(n) AS n`, 2)
	assertCount(ctx, t, eng, `MATCH (:Parent {id: 1})-[:HAS_CHILD]->(:Child {id: 99}) RETURN count(*) AS n`, 1)

	// Running the identical MERGE again must now match the just-created
	// connected pattern and stop growing — proves the operator is not
	// merely "always create".
	drainRunInTx(t, eng, `MATCH (p:Parent {id: 1}) MERGE (p)-[:HAS_CHILD]->(:Child {id: 99})`)
	assertCount(ctx, t, eng, `MATCH (n:Child {id: 99}) RETURN count(n) AS n`, 2)
}

// TestMergePattern_TwoHopChain verifies a 2-hop chain with every position
// fresh creates all three nodes and both relationships in one MERGE, and
// that a second identical MERGE matches the whole chain without duplicating
// any part of it.
func TestMergePattern_TwoHopChain(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:X {k: 1})-[:R1]->(b:Y {k: 2})-[:R2]->(c:Z {k: 3})`)

	assertCount(ctx, t, eng, `MATCH (n:X {k: 1}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:Y {k: 2}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:Z {k: 3}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (:X {k: 1})-[:R1]->(:Y {k: 2})-[:R2]->(:Z {k: 3}) RETURN count(*) AS n`, 1)

	drainRunInTx(t, eng, `MERGE (a:X {k: 1})-[:R1]->(b:Y {k: 2})-[:R2]->(c:Z {k: 3})`)
	assertCount(ctx, t, eng, `MATCH (n:X {k: 1}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:Y {k: 2}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:Z {k: 3}) RETURN count(n) AS n`, 1)
}

// TestMergePattern_MultipleJointMatchesFanOut verifies that when the whole
// pattern has more than one satisfying joint binding, MERGE emits one row
// per binding rather than collapsing to a single match — mirroring how a
// MATCH of the same pattern would fan out.
func TestMergePattern_MultipleJointMatchesFanOut(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:Hub {id: 1}) CREATE (a)-[:LINK]->(:Leaf {tag: 'x'}) CREATE (a)-[:LINK]->(:Leaf {tag: 'x'})`)
	assertCount(ctx, t, eng, `MATCH (:Hub {id: 1})-[:LINK]->(:Leaf {tag: 'x'}) RETURN count(*) AS n`, 2)

	res, err := eng.RunInTx(ctx, `MATCH (a:Hub {id: 1}) MERGE (a)-[:LINK]->(l:Leaf {tag: 'x'}) RETURN l`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("compound MERGE with 2 joint matches produced %d rows, want 2 (must fan out, not collapse)", len(rows))
	}
	// No new Leaf was created: still exactly 2.
	assertCount(ctx, t, eng, `MATCH (:Hub {id: 1})-[:LINK]->(:Leaf {tag: 'x'}) RETURN count(*) AS n`, 2)
}

// TestMergePattern_RollbackLeavesNoPartialState verifies ACID atomicity: a
// transaction whose first statement is a compound MERGE (creating fresh
// nodes and a relationship) and whose second statement fails must roll back
// to a state with NONE of the MERGE's writes visible — never a partial
// pattern (e.g. the first node created but not the second, or vice versa).
func TestMergePattern_RollbackLeavesNoPartialState(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Exec(`MERGE (a:RB1 {k: 'a'})-[:R]->(b:RB2 {k: 'b'})`, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec compound MERGE: %v", err)
	}
	// Force a failure later in the same transaction (undefined variable is
	// a compile-time SemanticError, guaranteed to fail every time).
	if _, err := tx.Exec(`RETURN undefinedVar`, nil); err == nil {
		_ = tx.Rollback()
		t.Fatal("expected the deliberately-broken second statement to fail")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	assertCount(ctx, t, eng, `MATCH (n:RB1 {k: 'a'}) RETURN count(n) AS n`, 0)
	assertCount(ctx, t, eng, `MATCH (n:RB2 {k: 'b'}) RETURN count(n) AS n`, 0)
	assertCount(ctx, t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS n`, 0)
}

// TestMergePattern_UndirectedMatchesEitherDirection verifies an undirected
// compound-pattern hop (`-[:R]-`) creates the edge in a fixed direction but
// matches it back from either direction on a subsequent MERGE.
func TestMergePattern_UndirectedMatchesEitherDirection(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:U1 {k: 'a'})-[:R]-(b:U2 {k: 'b'})`)
	assertCount(ctx, t, eng, `MATCH (:U1 {k: 'a'})-[:R]-(:U2 {k: 'b'}) RETURN count(*) AS n`, 1)

	// Re-running with the pattern's two endpoints swapped must still match
	// the single existing edge (undirected), not create a second one.
	drainRunInTx(t, eng, `MERGE (b:U2 {k: 'b'})-[:R]-(a:U1 {k: 'a'})`)
	assertCount(ctx, t, eng, `MATCH ()-[r:R]-() RETURN count(r) AS n`, 2) // count(r) sees each undirected edge from both ends
	assertCount(ctx, t, eng, `MATCH (:U1 {k: 'a'})-[:R]->(:U2 {k: 'b'}) RETURN count(*) AS n`, 1)
}

// TestMergePattern_IncomingDirection verifies `(a)<-[:R]-(b)` stores the
// edge from b to a (not a to b), matching CREATE's own direction handling.
func TestMergePattern_IncomingDirection(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (a:In1 {k: 'a'})<-[:R]-(b:In2 {k: 'b'})`)

	assertCount(ctx, t, eng, `MATCH (:In2 {k: 'b'})-[:R]->(:In1 {k: 'a'}) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (:In1 {k: 'a'})-[:R]->(:In2 {k: 'b'}) RETURN count(*) AS n`, 0)

	// Re-running the identical pattern must match, not duplicate.
	drainRunInTx(t, eng, `MERGE (a:In1 {k: 'a'})<-[:R]-(b:In2 {k: 'b'})`)
	assertCount(ctx, t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS n`, 1)
}

// TestMergePattern_ParameterizedProperties verifies a compound pattern's
// node properties bound via query parameters are neither silently dropped
// (the defect class this fix eliminates) nor mishandled: the created nodes
// and the search predicate on a subsequent MERGE both honour the parameter
// value.
func TestMergePattern_ParameterizedProperties(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	params := map[string]expr.Value{"pid": expr.StringValue("p1"), "cid": expr.StringValue("c1")}
	res, err := eng.RunInTx(ctx, `MERGE (a:PX {id: $pid})-[:R]->(b:PY {id: $cid})`, params)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	res.Close()

	assertCount(ctx, t, eng, `MATCH (n:PX {id: 'p1'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:PY {id: 'c1'}) RETURN count(n) AS n`, 1)

	// A second MERGE with the SAME parameter values must match, not
	// duplicate — proves the parameter value drives the search predicate,
	// not just the create-time write.
	res2, err := eng.RunInTx(ctx, `MERGE (a:PX {id: $pid})-[:R]->(b:PY {id: $cid})`, params)
	if err != nil {
		t.Fatalf("RunInTx (2nd): %v", err)
	}
	for res2.Next() {
	}
	if err := res2.Err(); err != nil {
		t.Fatalf("result error (2nd): %v", err)
	}
	res2.Close()

	assertCount(ctx, t, eng, `MATCH (n:PX {id: 'p1'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (:PX {id: 'p1'})-[:R]->(:PY {id: 'c1'}) RETURN count(*) AS n`, 1)
}

// TestMergePattern_NonLiteralRelationshipProperty verifies that a relationship
// property whose value is a non-literal expression (a variable reference from a
// bound node, `s.v`) is evaluated per driving row, written on the created edge,
// AND used as the whole-pattern search predicate. This shape was once rejected
// at build time (neither MERGE path evaluated non-literal relationship
// properties); it is now supported on par with the both-endpoints-bound
// MergeRelationship fast path. Previously the value was at risk of being
// silently dropped to null — a fail-silent Consistency defect.
func TestMergePattern_NonLiteralRelationshipProperty(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (:Src {v: 7})`)
	drainRunInTx(t, eng, `MATCH (s:Src) MERGE (a:RX {k: 'a'})-[:R {x: s.v}]->(b:RY {k: 'b'})`)

	// The created edge carries the evaluated value (integer 7): matching on the
	// property in the read pattern proves it was written, not dropped to null.
	assertCount(ctx, t, eng, `MATCH (:RX {k:'a'})-[:R {x: 7}]->(:RY {k:'b'}) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:RX) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:RY) RETURN count(n) AS n`, 1)

	// A second MERGE with the same evaluated value matches the first pattern
	// (no duplicate) — the value drives the search predicate, not just the
	// create-time write.
	drainRunInTx(t, eng, `MATCH (s:Src) MERGE (a:RX {k: 'a'})-[:R {x: s.v}]->(b:RY {k: 'b'})`)
	assertCount(ctx, t, eng, `MATCH (:RX {k:'a'})-[:R]->(:RY {k:'b'}) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n:RX) RETURN count(n) AS n`, 1)
}

// TestMergePattern_WALDurability verifies a compound MERGE's created nodes
// and relationship survive a close+reopen (pure WAL replay, no checkpoint)
// cycle, and that a second identical MERGE issued against the REPLAYED graph
// still matches the whole pattern rather than duplicating it — the latter
// exercises the same __cx_merge_<hex> global-counter reseed
// ([seedGlobalNodeCounter]) that [CreateNode]/[Merge] already rely on across
// a process boundary (rmp #1460), now also exercised by MergePattern's own
// fresh-node minting.
func TestMergePattern_WALDurability(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	ctx := context.Background()

	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	deleteWALEngineRun(t, g1, w1, `MERGE (a:WD1 {k: 'a'})-[:R]->(b:WD2 {k: 'b'})`)

	res2, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open2 recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	// Re-run the identical MERGE against the replayed graph. If the fresh
	// nodes did not survive replay this would create a duplicate pair; if
	// the global node counter were not correctly reseeded post-replay, the
	// duplicate would collide with the replayed keys instead of matching.
	deleteWALEngineRun(t, res2.Graph, w2, `MERGE (a:WD1 {k: 'a'})-[:R]->(b:WD2 {k: 'b'})`)

	res3, err := recovery.Open[string, float64](dir, deleteWALRecOpts())
	if err != nil {
		t.Fatalf("open3 recovery.Open: %v", err)
	}
	eng3 := cypher.NewEngine(res3.Graph)

	assertCount(ctx, t, eng3, `MATCH (n:WD1 {k: 'a'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng3, `MATCH (n:WD2 {k: 'b'}) RETURN count(n) AS n`, 1)
	assertCount(ctx, t, eng3, `MATCH (:WD1 {k: 'a'})-[:R]->(:WD2 {k: 'b'}) RETURN count(*) AS n`, 1)
}

// TestMergePattern_RejectsCyclicSameVariableReuse verifies a pattern that
// reuses the SAME freshly-introduced variable at a later chain position
// (a cycle back to the anchor) is rejected with a clear, specific error at
// plan-build time — not silently mishandled, and not left to fail deep
// inside the physical operator with a confusing "bound variable is null"
// message. Genuinely bound-by-an-earlier-clause re-use (the ordinary
// asymmetric attach-to-existing-parent form, see
// TestMergePattern_AttachToExistingParent_DoesNotDecompose) is unaffected.
func TestMergePattern_RejectsCyclicSameVariableReuse(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	_, err := eng.RunInTx(ctx, `MERGE (a:CY {k: 1})-[:R1]->(b:CY2 {k: 2})-[:R2]->(a)`, nil)
	if err == nil {
		t.Fatal("expected an error for a cyclic same-pattern variable reuse, got nil")
	}
	const wantSubstr = "re-references fresh variable"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), wantSubstr)
	}
	assertCount(ctx, t, eng, `MATCH (n:CY) RETURN count(n) AS n`, 0)
	assertCount(ctx, t, eng, `MATCH (n:CY2) RETURN count(n) AS n`, 0)
}

// execExpectConstraintViolation runs query, DRAINING the result to
// completion — write operators are Volcano-style (pull-based), so the
// actual mutation and its constraint check only happen once the caller
// pulls rows via Next, not at RunInTx's return (mirrors
// TestEngine_UniqueConstraint_Violation's pattern in write_engine_test.go).
// Accepts the violation surfacing either as a build-time RunInTx error or as
// a drain-time iteration error, since both are valid places for a Volcano
// pipeline to report it.
func execExpectConstraintViolation(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	ctx := context.Background()
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		if !errors.Is(err, exec.ErrConstraintViolation) {
			t.Fatalf("RunInTx(%q) build error = %v, want errors.Is(err, exec.ErrConstraintViolation)", query, err)
		}
		return
	}
	for res.Next() { // drain to trigger the write + constraint check
	}
	iterErr := res.Err()
	_ = res.Close()
	if iterErr == nil {
		t.Fatalf("RunInTx(%q): expected a constraint violation, got nil", query)
	}
	if !errors.Is(iterErr, exec.ErrConstraintViolation) {
		t.Fatalf("RunInTx(%q) iteration error = %v, want errors.Is(err, exec.ErrConstraintViolation)", query, iterErr)
	}
}

// TestMergePattern_EnforcesUniqueConstraintOnCreate verifies a UNIQUE
// constraint on a fresh chain position's label is enforced when MergePattern
// creates it, both via the direct create path and via ON CREATE SET —
// mirroring the enforcement [Merge]/[CreateNode] already apply. Without
// this wiring a compound MERGE could silently create a node that violates a
// declared uniqueness constraint (an ACID Consistency breach).
func TestMergePattern_EnforcesUniqueConstraintOnCreate(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	if _, err := eng.RunAny(ctx, `CREATE CONSTRAINT uniq_person_email ON (n:UCPerson) ASSERT n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	drainRunInTx(t, eng, `CREATE (:UCPerson {email: 'dup@example.com'}), (:Hub2 {id: 1})`)

	// Direct create-path violation: the fresh endpoint's own property map
	// collides with the existing UCPerson.
	execExpectConstraintViolation(t, eng, `MATCH (h:Hub2 {id: 1}) MERGE (h)-[:OWNS]->(:UCPerson {email: 'dup@example.com'})`)
	// No partial state: the violating node must not have been created.
	assertCount(ctx, t, eng, `MATCH (n:UCPerson) RETURN count(n) AS n`, 1)

	// ON CREATE SET violation: the fresh endpoint's OWN pattern properties
	// are fine, but an ON CREATE SET action assigns the colliding value.
	execExpectConstraintViolation(t, eng, `MATCH (h:Hub2 {id: 1}) MERGE (h)-[:OWNS2]->(p:UCPerson {id: 999}) ON CREATE SET p.email = 'dup@example.com'`)
	assertCount(ctx, t, eng, `MATCH (n:UCPerson) RETURN count(n) AS n`, 1)

	// A non-colliding value must still succeed.
	drainRunInTx(t, eng, `MATCH (h:Hub2 {id: 1}) MERGE (h)-[:OWNS]->(:UCPerson {email: 'unique@example.com'})`)
	assertCount(ctx, t, eng, `MATCH (n:UCPerson) RETURN count(n) AS n`, 2)
}

// TestMergePattern_ParallelRelationshipMultiplicity verifies MergePattern fans
// out one binding per pre-existing parallel relationship for a both-bound hop
// (rmp #1875): when the endpoints already have TWO parallel qualifying
// relationships, a both-bound MERGE reports the same multiplicity as the
// equivalent MATCH (two rows), rather than the single row the pre-fix
// under-counting produced. The MERGE still matches (does not create a
// duplicate) and does not create a duplicate node.
func TestMergePattern_ParallelRelationshipMultiplicity(t *testing.T) {
	t.Parallel()
	eng := newMergePatternEngine(t)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:Hub3 {id: 1}), (b:Leaf3 {tag: 'x'})`)
	drainRunInTx(t, eng, `MATCH (a:Hub3 {id: 1}), (b:Leaf3 {tag: 'x'}) CREATE (a)-[:LINK]->(b)`)
	drainRunInTx(t, eng, `MATCH (a:Hub3 {id: 1}), (b:Leaf3 {tag: 'x'}) CREATE (a)-[:LINK]->(b)`)

	// Baseline: a plain MATCH correctly sees both parallel edges.
	assertCount(ctx, t, eng, `MATCH (:Hub3 {id: 1})-[:LINK]->(:Leaf3 {tag: 'x'}) RETURN count(*) AS n`, 2)

	// A both-bound MERGE with a node-targeted ON CREATE action routes
	// through MergePattern (not the MergeRelationship fast path, which
	// requires actions confined to the relationship variable). Post-#1875 it
	// reports the SAME multiplicity as the MATCH above.
	res, err := eng.RunInTx(ctx, `MATCH (a:Hub3 {id: 1}), (b:Leaf3 {tag: 'x'}) MERGE (a)-[r:LINK]->(b) ON CREATE SET a.touched = true RETURN count(r) AS n`, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	mustInt(t, "count(r)", rows[0]["n"], 2) // #1875: MERGE multiplicity == MATCH multiplicity

	// No duplicate relationship or node was created by the MERGE itself.
	assertCount(ctx, t, eng, `MATCH (:Hub3 {id: 1})-[:LINK]->(:Leaf3 {tag: 'x'}) RETURN count(*) AS n`, 2)
	assertCount(ctx, t, eng, `MATCH (n:Leaf3) RETURN count(n) AS n`, 1)
}
