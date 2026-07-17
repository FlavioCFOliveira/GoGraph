package cypher_test

// delete_parallel_edge_instance_test.go — regression gate for rmp #2018.
//
// In a multigraph, `DELETE r` of a specifically-bound parallel-edge instance
// must remove the EXACT bound instance (its adjacency slot AND its per-handle
// metadata), leaving siblings intact. The pre-fix DELETE path resolved the
// removal by endpoint pair only (mutator.RemoveEdge(src,dst)), which removes
// the FIRST adjacency slot regardless of which instance `r` is bound to — an
// ACID-Consistency violation. `DELETE r:T1` "worked" only by luck (T1 is the
// first slot); `DELETE r:T2` wrongly removed T1 and left T2 surviving.
//
// Pre-fix: TestDeleteParallelEdgeInstance_DeleteT2_LeavesT1 and the same-type
// case FAIL (the wrong instance survives). Post-fix: all pass.
//
// Layer: short. In-memory multigraph engine (lpgMutatorAdapter path) — the
// helpers inMemMultigraphEngine / mustRunWrite / keyNode live in
// byhandle_edge_prop_mutation_test.go.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// survivingRelTypes runs MATCH (:N{key:'x'})-[r]->(:N{key:'y'}) RETURN type(r)
// and returns the sorted surviving relationship types.
func survivingRelTypes(t *testing.T, eng *cypher.Engine) []string {
	t.Helper()
	res, err := eng.Run(context.Background(),
		`MATCH (:N {key:'x'})-[r]->(:N {key:'y'}) RETURN type(r) AS t`, nil)
	if err != nil {
		t.Fatalf("read surviving types: %v", err)
	}
	records := drainRecords(t, res)
	out := make([]string, 0, len(records))
	for _, row := range records {
		s, ok := row["t"].(expr.StringValue)
		if !ok {
			t.Fatalf("type(r) is %T, want StringValue", row["t"])
		}
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// survivingSeqs runs MATCH (:N{key:'x'})-[r]->(:N{key:'y'}) RETURN r.seq and
// returns the sorted surviving seq property values. Used for the same-type
// parallel-edge case, where instances are distinguished by a per-instance
// property rather than by type.
func survivingSeqs(t *testing.T, eng *cypher.Engine) []int64 {
	t.Helper()
	res, err := eng.Run(context.Background(),
		`MATCH (:N {key:'x'})-[r]->(:N {key:'y'}) RETURN r.seq AS s`, nil)
	if err != nil {
		t.Fatalf("read surviving seqs: %v", err)
	}
	records := drainRecords(t, res)
	out := make([]int64, 0, len(records))
	for _, row := range records {
		iv, ok := row["s"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("r.seq is %T, want IntegerValue", row["s"])
		}
		out = append(out, int64(iv))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// seedTwoDistinctlyTypedParallelEdges builds (:N{key:'x'})-[:T1]->(:N{key:'y'})
// and a parallel (:N{key:'x'})-[:T2]->(:N{key:'y'}), in that create order (so
// T1 occupies the FIRST adjacency slot — the slot the buggy first-match removal
// would drop).
func seedTwoDistinctlyTypedParallelEdges(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T1]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T2]->(b)`)
}

// TestDeleteParallelEdgeInstance_DeleteT2_LeavesT1 is the task's case-B repro:
// deleting the SECOND-created type must leave the first surviving. Pre-fix the
// first-match removal dropped T1 and left T2 — this test FAILS pre-fix.
func TestDeleteParallelEdgeInstance_DeleteT2_LeavesT1(t *testing.T) {
	t.Parallel()
	eng, _ := inMemMultigraphEngine(t)
	seedTwoDistinctlyTypedParallelEdges(t, eng)

	mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:T2]->(:N {key:'y'}) DELETE r`)

	got := survivingRelTypes(t, eng)
	if len(got) != 1 || got[0] != "T1" {
		t.Fatalf("after DELETE r:T2, surviving types = %v, want [T1] (wrong parallel instance removed)", got)
	}
}

// TestDeleteParallelEdgeInstance_DeleteT1_LeavesT2 is the symmetric case:
// deleting the FIRST-created type must leave the second surviving. This
// passed pre-fix only by luck (T1 is the first slot); it must stay green.
func TestDeleteParallelEdgeInstance_DeleteT1_LeavesT2(t *testing.T) {
	t.Parallel()
	eng, _ := inMemMultigraphEngine(t)
	seedTwoDistinctlyTypedParallelEdges(t, eng)

	mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:T1]->(:N {key:'y'}) DELETE r`)

	got := survivingRelTypes(t, eng)
	if len(got) != 1 || got[0] != "T2" {
		t.Fatalf("after DELETE r:T1, surviving types = %v, want [T2]", got)
	}
}

// TestDeleteParallelEdgeInstance_SameType_DeleteMiddle_LeavesTwo covers three
// SAME-type parallel edges distinguished only by a per-instance property. The
// bound instance (seq=2) must be the one removed; the survivors keep seq 1 and
// 3. Pre-fix the first-match removal always dropped the FIRST slot (seq=1),
// leaving {2,3} — this test FAILS pre-fix.
func TestDeleteParallelEdgeInstance_SameType_DeleteMiddle_LeavesTwo(t *testing.T) {
	t.Parallel()
	eng, _ := inMemMultigraphEngine(t)
	mustRunWrite(t, eng, `CREATE (a:N {key:'x'})`)
	mustRunWrite(t, eng, `CREATE (b:N {key:'y'})`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T {seq:1}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T {seq:2}]->(b)`)
	mustRunWrite(t, eng, `MATCH (a:N {key:'x'}),(b:N {key:'y'}) CREATE (a)-[:T {seq:3}]->(b)`)

	mustRunWrite(t, eng, `MATCH (:N {key:'x'})-[r:T]->(:N {key:'y'}) WHERE r.seq = 2 DELETE r`)

	got := survivingSeqs(t, eng)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("after DELETE of seq=2, surviving seqs = %v, want [1 3] (wrong parallel instance removed)", got)
	}
}

// TestDeleteParallelEdgeInstance_DeletedRowView_IsBoundInstance asserts the
// deleted-row marker reflects the BOUND instance: DELETE r:T2 ... RETURN
// type(r) yields "T2", and the surviving edge is T1. This guards the row's
// post-delete RelationshipValue view against a regression while the removal is
// made instance-precise.
func TestDeleteParallelEdgeInstance_DeletedRowView_IsBoundInstance(t *testing.T) {
	t.Parallel()
	eng, _ := inMemMultigraphEngine(t)
	seedTwoDistinctlyTypedParallelEdges(t, eng)

	res, err := eng.RunInTx(context.Background(),
		`MATCH (:N {key:'x'})-[r:T2]->(:N {key:'y'}) DELETE r RETURN type(r) AS t`, nil)
	if err != nil {
		t.Fatalf("DELETE r:T2 RETURN type(r): %v", err)
	}
	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("DELETE ... RETURN yielded %d rows, want 1: %v", len(rows), rows)
	}
	s, ok := rows[0]["t"].(expr.StringValue)
	if !ok || string(s) != "T2" {
		t.Fatalf("RETURN type(r) after DELETE = %v (%T), want StringValue T2", rows[0]["t"], rows[0]["t"])
	}

	got := survivingRelTypes(t, eng)
	if len(got) != 1 || got[0] != "T1" {
		t.Fatalf("after DELETE r:T2 RETURN, surviving types = %v, want [T1]", got)
	}
}

// TestDeleteParallelEdgeInstance_RollbackRestoresExactInstance is the Atomicity
// gate: a DELETE of a bound parallel instance that is part of a statement which
// then fails on a later clause must restore EXACTLY that instance — its
// adjacency slot, its stable handle, and its own per-handle type — so neither
// sibling is lost and the removed one comes back with its own identity (rmp
// #2018, ACID-Atomicity). The failure is triggered by a SchemaValidator that
// rejects the trailing SET, so the whole statement rolls back inside the
// visibility barrier via the write-query undo log.
func TestDeleteParallelEdgeInstance_RollbackRestoresExactInstance(t *testing.T) {
	eng, g, w, _ := walMultigraphEngineWithGraph(t)
	defer w.Close()

	seedTwoDistinctlyTypedParallelEdges(t, eng)

	before := survivingRelTypes(t, eng)
	if len(before) != 2 || before[0] != "T1" || before[1] != "T2" {
		t.Fatalf("pre-rollback surviving types = %v, want [T1 T2]", before)
	}

	// DELETE the T2 instance, then SET a rejected node property so the statement
	// errors AFTER the DELETE has applied eagerly. nthSetRejector rejects the
	// first `boom` write.
	g.SetValidator(&nthSetRejector{key: "boom", rejN: 1})
	err := runWrite(t, eng,
		`MATCH (a:N {key:'x'})-[r:T2]->(b:N {key:'y'}) DELETE r SET a.boom = 1`)
	if err == nil {
		t.Fatal("expected the write to error on the rejected SET after DELETE, got nil")
	}
	g.SetValidator(nil)

	// The rolled-back statement must have restored the T2 instance exactly: both
	// parallel types are present again.
	after := survivingRelTypes(t, eng)
	if len(after) != 2 || after[0] != "T1" || after[1] != "T2" {
		t.Fatalf("after rolled-back DELETE, surviving types = %v, want [T1 T2] (removed instance not restored exactly)", after)
	}
}
