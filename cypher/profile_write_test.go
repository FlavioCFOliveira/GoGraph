package cypher_test

// profile_write_test.go — PROFILE of a WRITING statement (rmp #2790).
//
// The profiling wrapper is installed in the WRITE builder, so a profiled write
// executes on the ordinary transactional write path: its writes apply exactly
// once, its write counters are those of the unprefixed statement, and the
// measured tree covers the write operators. Engine.Run, which never writes,
// refuses a writing PROFILE exactly as it refuses the unprefixed statement.
//
// Every comparison below runs the SAME statement against engines seeded
// identically: one unprefixed, one with the PROFILE prefix, one through
// Engine.Profile. Equality of the counters and of the resulting graph is the
// assertion, so a profiled run that wrote twice, wrote nothing, or wrote
// something else fails it.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
)

// profileWriteFixture seeds eng with four :Person nodes (age 20..23) on a
// :KNOWS chain 20→21→22→23. It goes through RunInTx so the store-backed
// variant seeds through the WAL.
func profileWriteFixture(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, q := range []string{
		"CREATE (:Person {age: 20})-[:KNOWS]->(:Person {age: 21})-[:KNOWS]->(:Person {age: 22})-[:KNOWS]->(:Person {age: 23})",
	} {
		r, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		_ = r.Close()
	}
}

func newProfileWriteEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
	t.Cleanup(func() { _ = eng.Close() })
	profileWriteFixture(t, eng)
	return eng
}

// writeFingerprint renders every node (labels and properties) and every
// relationship (type, endpoint ages, properties) as a sorted list, so two
// graphs compare equal exactly when they hold the same content.
func writeFingerprint(t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	var lines []string
	for _, q := range []string{
		"MATCH (n) RETURN labels(n) AS l, properties(n) AS p",
		"MATCH (a)-[r]->(b) RETURN type(r) AS t, a.age AS s, b.age AS e, properties(r) AS p",
	} {
		r, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("fingerprint %q: %v", q, err)
		}
		for r.Next() {
			parts := make([]string, 0, 4)
			for i := range r.Columns() {
				parts = append(parts, canonicalValue(r.ValueAt(i)))
			}
			lines = append(lines, strings.Join(parts, "|"))
		}
		if err := r.Err(); err != nil {
			t.Fatalf("fingerprint %q: %v", q, err)
		}
		_ = r.Close()
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// canonicalValue renders v with a map's entries in key order, because a map's
// own rendering follows insertion order and two equal graphs need not have
// written their properties in the same order.
func canonicalValue(v expr.Value) string {
	m, ok := v.(expr.MapValue)
	if !ok {
		return v.String()
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + m[k].String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// pwDrainRows returns the rendered rows of r and closes it.
func pwDrainRows(t *testing.T, r *cypher.Result) []string {
	t.Helper()
	var rows []string
	for r.Next() {
		parts := make([]string, 0, 2)
		for i := range r.Columns() {
			parts = append(parts, r.ValueAt(i).String())
		}
		rows = append(rows, strings.Join(parts, "|"))
	}
	if err := r.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return rows
}

// planNames returns every operator name in the tree, root first.
func planNames(n *exec.PlanNode) []string {
	out := make([]string, 0, 1+len(n.Children))
	out = append(out, n.Name)
	for i := range n.Children {
		out = append(out, planNames(&n.Children[i])...)
	}
	return out
}

// findPlanNode returns the first node named name, or nil.
func findPlanNode(n *exec.PlanNode, name string) *exec.PlanNode {
	if n.Name == name {
		return n
	}
	for i := range n.Children {
		if f := findPlanNode(&n.Children[i], name); f != nil {
			return f
		}
	}
	return nil
}

// unmeasured returns the names of the nodes the profiler did not measure.
func unmeasured(n *exec.PlanNode) []string {
	var out []string
	if !n.Profiled {
		out = append(out, n.Name)
	}
	for i := range n.Children {
		out = append(out, unmeasured(&n.Children[i])...)
	}
	return out
}

var profileWriteCases = []struct {
	name  string
	query string
	// op is the write operator the measured tree must contain, with rows > 0.
	op string
}{
	{"CREATE node", "CREATE (n:Q {name: 'new', age: 99}) RETURN n.name AS name", "CreateNode"},
	{"CREATE relationship", "MATCH (a:Person {age: 20}), (b:Person {age: 23}) CREATE (a)-[:LIKES {w: 1}]->(b)", "CreateRelationship"},
	{"SET property and label", "MATCH (n:Person) WHERE n.age >= 21 SET n.w = n.age * 2, n:Tagged RETURN count(*) AS c", "SetProperty"},
	{"REMOVE property", "MATCH (n:Person) WHERE n.age < 22 REMOVE n.age", "RemoveProperty"},
	{"REMOVE label", "MATCH (n:Person {age: 23}) REMOVE n:Person", "RemoveLabels"},
	// DELETE of a relationship and of a node are both built as the DeleteNode
	// operator, which deletes whatever entity its expression evaluates to.
	{"DELETE relationship", "MATCH (:Person {age: 20})-[r:KNOWS]->() DELETE r", "DeleteNode"},
	{"DELETE node", "MATCH (n:Person {age: 23})<-[r:KNOWS]-() DELETE r, n", "DeleteNode"},
	{"DETACH DELETE", "MATCH (n:Person {age: 21}) DETACH DELETE n", "DetachDelete"},
	{"MERGE creating", "MERGE (n:Person {age: 50}) ON CREATE SET n.created = true RETURN n.age AS age", "Merge"},
	{"MERGE matching", "MERGE (n:Person {age: 20}) ON MATCH SET n.matched = true", "Merge"},
	{"MERGE relationship", "MATCH (a:Person {age: 20}), (b:Person {age: 22}) MERGE (a)-[r:KNOWS2]->(b) RETURN type(r) AS t", "MergeRelationship"},
}

// TestProfileWrite_PrefixAppliesWritesOnceWithEqualCounters is the AC: PROFILE
// of CREATE, SET, REMOVE, DELETE, DETACH DELETE and MERGE executes, applies its
// writes exactly once, returns a measured tree, and reports the write counters
// of the unprefixed statement.
func TestProfileWrite_PrefixAppliesWritesOnceWithEqualCounters(t *testing.T) {
	ctx := context.Background()
	for _, tc := range profileWriteCases {
		t.Run(tc.name, func(t *testing.T) {
			plain := newProfileWriteEngine(t)
			profiled := newProfileWriteEngine(t)
			viaAPI := newProfileWriteEngine(t)
			before := writeFingerprint(t, plain)

			pr, err := plain.RunInTx(ctx, tc.query, nil)
			if err != nil {
				t.Fatalf("unprefixed: %v", err)
			}
			plainCounters := *pr.Counters()
			if pr.Profile() != nil {
				t.Errorf("an UNPROFILED write carries a measured plan")
			}
			plainRows := pwDrainRows(t, pr)

			fr, err := profiled.RunInTx(ctx, "PROFILE "+tc.query, nil)
			if err != nil {
				t.Fatalf("PROFILE-prefixed write was not executed: %v", err)
			}
			profCounters := *fr.Counters()
			tree := fr.Profile()
			profRows := pwDrainRows(t, fr)

			if plainCounters != profCounters {
				t.Errorf("write counters differ:\n  unprefixed %+v\n  profiled   %+v", plainCounters, profCounters)
			}
			if !plainCounters.ContainsUpdates() {
				t.Fatalf("the statement changed nothing, so this case proves nothing: %+v", plainCounters)
			}
			if strings.Join(plainRows, ",") != strings.Join(profRows, ",") {
				t.Errorf("rows differ: unprefixed %v, profiled %v", plainRows, profRows)
			}
			after := writeFingerprint(t, plain)
			if after == before {
				t.Fatalf("the unprefixed statement left the graph unchanged; the case proves nothing")
			}
			if got := writeFingerprint(t, profiled); got != after {
				t.Errorf("the profiled write left a different graph from the unprefixed one:\n--- unprefixed ---\n%s\n--- profiled ---\n%s", after, got)
			}

			if tree == nil {
				t.Fatal("a PROFILE-prefixed write returned no measured plan")
			}
			if u := unmeasured(tree); len(u) > 0 {
				t.Errorf("unmeasured operators %v in:\n%s", u, exec.RenderPlanNode(tree))
			}
			// Only a RETURN-bearing statement has result rows to compare the root
			// against; a write-only statement returns none whatever its root emits.
			if strings.Contains(tc.query, " RETURN ") && tree.Rows != int64(len(profRows)) {
				t.Errorf("the plan root reports %d rows, the statement returned %d:\n%s",
					tree.Rows, len(profRows), exec.RenderPlanNode(tree))
			}
			w := findPlanNode(tree, tc.op)
			if w == nil {
				t.Fatalf("no %s operator in the measured tree %v:\n%s", tc.op, planNames(tree), exec.RenderPlanNode(tree))
			}
			if w.Rows == 0 {
				t.Errorf("the %s operator was measured at zero rows:\n%s", tc.op, exec.RenderPlanNode(tree))
			}

			// The Go surface: Engine.Profile executes the write the same way.
			out, err := viaAPI.Profile(ctx, tc.query, nil)
			if err != nil {
				t.Fatalf("Engine.Profile of a writing statement: %v", err)
			}
			if !strings.Contains(out, tc.op) {
				t.Errorf("Engine.Profile's tree has no %s:\n%s", tc.op, out)
			}
			if got := writeFingerprint(t, viaAPI); got != after {
				t.Errorf("Engine.Profile left a different graph from the unprefixed statement:\n--- unprefixed ---\n%s\n--- Profile ---\n%s", after, got)
			}
		})
	}
}

// TestProfileWrite_RunRefusesLikeTheUnprefixedStatement pins that Engine.Run —
// a surface that never writes — refuses a writing PROFILE with exactly the error
// it gives the unprefixed statement, and applies nothing.
func TestProfileWrite_RunRefusesLikeTheUnprefixedStatement(t *testing.T) {
	ctx := context.Background()
	eng := newProfileWriteEngine(t)
	before := writeFingerprint(t, eng)

	_, plainErr := eng.Run(ctx, "CREATE (:Q)", nil)
	_, profErr := eng.Run(ctx, "PROFILE CREATE (:Q)", nil)
	if plainErr == nil || profErr == nil {
		t.Fatalf("Run executed a write: unprefixed err=%v, profiled err=%v", plainErr, profErr)
	}
	if !errors.Is(profErr, cypher.ErrWriteInReadOnlyTx) {
		t.Errorf("the profiled refusal is not ErrWriteInReadOnlyTx: %v", profErr)
	}
	if plainErr.Error() != profErr.Error() {
		t.Errorf("Run refuses the two spellings differently:\n  unprefixed %v\n  profiled   %v", plainErr, profErr)
	}
	if after := writeFingerprint(t, eng); after != before {
		t.Errorf("a refused write changed the graph")
	}

	// A read-only explicit transaction refuses it too, as it refuses the write.
	tx, err := eng.BeginReadTx(ctx)
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	if _, err := tx.Exec("PROFILE CREATE (:Q)", nil); !errors.Is(err, cypher.ErrWriteInReadOnlyTx) {
		t.Errorf("a read-only transaction did not refuse a writing PROFILE: %v", err)
	}
	_ = tx.Rollback()
	if after := writeFingerprint(t, eng); after != before {
		t.Errorf("a refused write changed the graph")
	}
}

// TestProfileWrite_InsideAnExplicitTransaction pins that a writing PROFILE joins
// the transaction it is executed in: rolled back, it leaves nothing; committed,
// it leaves its writes exactly once.
func TestProfileWrite_InsideAnExplicitTransaction(t *testing.T) {
	ctx := context.Background()
	eng := newProfileWriteEngine(t)
	countQ := func() string {
		rows := pwDrainRows(t, pwMustRun(t, eng, "MATCH (n:Q) RETURN count(n) AS c"))
		return rows[0]
	}

	for _, commit := range []bool{false, true} {
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		r, err := tx.Exec("PROFILE CREATE (:Q {name: 'tx'})", nil)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("PROFILE inside a transaction: %v", err)
		}
		if r.Profile() == nil || findPlanNode(r.Profile(), "CreateNode") == nil {
			t.Errorf("no measured CreateNode in the transaction's PROFILE")
		}
		_ = pwDrainRows(t, r)
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		} else if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
	}
	if got := countQ(); got != "1" {
		t.Errorf("after one rolled-back and one committed PROFILE CREATE there are %s :Q nodes, want 1", got)
	}
}

func pwMustRun(t *testing.T, eng *cypher.Engine, q string) *cypher.Result {
	t.Helper()
	r, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	return r
}

// TestProfileWrite_IsDurableAndAtomicOnTheWAL runs profiled writes on a
// WAL-backed engine and recovers the log: a committed profiled write is in it
// exactly once, and a profiled write that fails mid-pipeline — after applying
// some rows — is rolled back in memory and absent from the log.
func TestProfileWrite_IsDurableAndAtomicOnTheWAL(t *testing.T) {
	ctx := context.Background()
	eng, g, w, dir := walEngineWithGraph(t)
	profileWriteFixture(t, eng)

	r, err := eng.RunInTx(ctx, "PROFILE CREATE (:Q {name: 'durable'})", nil)
	if err != nil {
		t.Fatalf("PROFILE CREATE: %v", err)
	}
	_ = pwDrainRows(t, r)

	// `1 / (n.age - 22)` divides by zero on the age-22 node only, so the SET
	// applies to the rows before it and then fails: the statement must roll back
	// as a whole.
	if r, err := eng.RunInTx(ctx, "PROFILE MATCH (n:Person) WITH n ORDER BY n.age SET n.x = 1 / (n.age - 22)", nil); err == nil {
		rows, derr := func() ([]string, error) {
			var out []string
			for r.Next() {
				out = append(out, "row")
			}
			e := r.Err()
			_ = r.Close()
			return out, e
		}()
		if derr == nil {
			t.Fatalf("the failing profiled SET succeeded (%d rows)", len(rows))
		}
	}
	if got := pwDrainRows(t, pwMustRun(t, eng, "MATCH (n) WHERE n.x IS NOT NULL RETURN count(n) AS c")); got[0] != "0" {
		t.Errorf("a failed profiled SET left %s nodes with x in memory, want 0", got[0])
	}

	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	res, err := recovery.Open[string, float64](dir, recOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	rg := res.Graph
	var qNodes, withX int
	rg.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if v, ok := rg.GetNodeProperty(key, "name"); ok && func() bool { s, _ := v.String(); return s == "durable" }() {
			qNodes++
		}
		if _, ok := rg.GetNodeProperty(key, "x"); ok {
			withX++
		}
		return true
	})
	if qNodes != 1 {
		t.Errorf("the recovered log holds %d copies of the committed profiled write, want 1", qNodes)
	}
	if withX != 0 {
		t.Errorf("the recovered log holds %d nodes written by the FAILED profiled SET, want 0", withX)
	}
	_ = g
}
