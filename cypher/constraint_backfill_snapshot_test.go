package cypher

// constraint_backfill_snapshot_test.go — rmp #2792 gate: CREATE CONSTRAINT's two
// enforcement structures are both built from COMMITTED state, at one instant.
//
// # The defect these tests pin
//
// A UNIQUE constraint is enforced by TWO structures that have to agree:
//
//   - the VALUE-SET in [exec.ConstraintRegistry], which every write is checked
//     against, and
//   - the BACKING HASH INDEX "__uniq__<label>.<prop>", which a NodeByIndexSeek
//     reads.
//
// Before this fix each was populated by its own read of the LIVE graph — the
// value-set from [Engine.scanLabelProperty], the index from
// [Engine.backfillNodeHashIndex] — and the live graph carries an explicit
// transaction's EAGER, UNCOMMITTED mutations. [ExplicitTx.Rollback] then
// discarded that transaction's [exec.IndexBuffer] WITHOUT inverting it, correctly,
// since the buffer describes changes that were never fanned out to any index — so
// whatever the backfill had written stayed there permanently.
//
// This is rmp #2778's defect on the CREATE CONSTRAINT path, which that fix
// deliberately left on the live view. Measured on the pre-fix build, one
// goroutine, NO CONCURRENCY, after `SET n.name = 'seed-1-ghost'` on tag k7 was
// rolled back mid-backfill:
//
//	CONSTRAINT backing index: ['seed-1-ghost'] = 1 entries (want 0)
//	                          ['seed-7']       = 0 entries (want 1)
//	n.name = 'seed-1-ghost' -> rows=[k7]  plan=NodeByIndexSeek   <- FABRICATED ROW
//	n.name = 'seed-7'       -> rows=[]                            <- LOST ROW
//
// # And UNIQUE ENFORCEMENT was wrong in BOTH directions, which #2778's path is not
//
// The value-set was seeded from the same live read, so it was wrong at the same
// instant — and that is a Consistency breach rather than merely a wrong row.
// Measured on the pre-fix build, per mutation shape:
//
//	setProperty  write 'seed-1-ghost' (never committed) -> REFUSED   (want ACCEPTED)
//	createNode   write 'seed-1-ghost' (never committed) -> REFUSED   (want ACCEPTED)
//	setProperty  write 'seed-7' (a live duplicate)      -> ACCEPTED  (want REFUSED)
//	removeLabel  write 'seed-7' (a live duplicate)      -> ACCEPTED  (want REFUSED)
//	deleteNode   write 'seed-7' (a live duplicate)      -> ACCEPTED  (want REFUSED)
//
// After the duplicate was accepted, a label scan found TWO committed Person nodes
// named 'seed-7' under an ACTIVE UNIQUE constraint, while the backing index
// reported only the newer one. That is why this is not a one-line copy of #2778:
// the index content and the enforcement verdict are two separate observables and
// the fix has to move both, in the right direction.
//
// # How the two are made to answer at the same instant
//
// The backfill now runs through a read view bound to ONE snapshot, opened INSIDE
// the visibility barrier, and it RECORDS THE VALUES IT READS into a
// [uniqueValueSeed] that the value-set is then seeded from. So the two structures
// come not merely from the same instant but from the SAME READ of each node, and
// cannot disagree about a node even in principle. See [uniqueValueSeed] and
// [Engine.createConstraintLocked].
//
// The VALIDATION scan deliberately still reads live — see
// TestCreateConstraint_UncommittedDuplicateStillRefusesTheConstraint for the
// measurement that makes that load-bearing rather than an oversight.
//
// # What the oracle is
//
// A label scan over the SAME predicate on a SECOND engine that carries no
// constraint and has run the SAME transaction and rollback. The assertion is set
// equality, so a fabricated row and a lost row are both caught and neither can
// mask the other. Where an enforcement verdict is asserted, the committed
// duplicate count is taken from a predicate-free projection (`MATCH (n:Person)
// RETURN n.name`), which no index can serve, so the count cannot be reported by
// the very structure under test.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// constraintSnapshotDDL is the statement under test on every arm. The seed's
// names are distinct, so the constraint is valid over the committed data.
const constraintSnapshotDDL = `CREATE CONSTRAINT c_name FOR (n:Person) REQUIRE n.name IS UNIQUE`

// constraintSnapshotGhost is a value NO committed node holds. The rolled-back
// transaction writes it; a seek for it must return nothing and a committed write
// of it must be accepted.
const constraintSnapshotGhost = "seed-1-ghost"

// constraintSnapshotReal is the committed value of the node every mutation below
// targets (tag k7). It is the other half of the defect: the entry the backfill
// should have written and did not, and the value whose duplicate must be refused.
const constraintSnapshotReal = "seed-7"

// constraintSnapshotMutation is one shape of eager, uncommitted write the
// backfill and the value-set seed must not observe. The four shapes cover each
// thing the scan resolves — the property VALUE, node EXISTENCE in both
// directions, and the LABEL — and they are the same four rmp #2778 uses, so the
// two gates are comparable arm for arm.
//
// setProperty and createNode FABRICATE an entry; removeLabel and deleteNode LOSE
// one. Both directions are needed, which is why the query-level oracle is set
// equality rather than "no unexpected rows".
type constraintSnapshotMutation struct {
	name string
	stmt string
}

func constraintSnapshotMutations() []constraintSnapshotMutation {
	return []constraintSnapshotMutation{
		{"setProperty", `MATCH (n:Person) WHERE n.tag = 'k7' SET n.name = '` + constraintSnapshotGhost + `'`},
		{"createNode", `CREATE (m:Person {tag: 'ghostnode', name: '` + constraintSnapshotGhost + `'})`},
		{"removeLabel", `MATCH (n:Person) WHERE n.tag = 'k7' REMOVE n:Person`},
		{"deleteNode", `MATCH (n:Person) WHERE n.tag = 'k7' DETACH DELETE n`},
	}
}

// newConstraintEngineAfterRollback builds a seeded engine, opens an explicit
// transaction, runs stmt inside it, runs ddl while that transaction is still
// OPEN, and only then rolls the transaction back. An empty ddl builds the oracle:
// the same seed and the same rolled-back transaction with no constraint, so its
// answers come from a label scan.
//
// The whole sequence is on ONE goroutine and needs no synchronisation: the DDL
// runs to completion between the eager mutation and the rollback by program
// order, so the window under test cannot fail to open. That is the point — this
// defect needs no concurrency at all.
//
// It deliberately mirrors [newEngineAfterRolledBackDDL] rather than calling it,
// for ONE reason: that helper forces the backfill SERIAL, and the value-set seed
// this fix introduces is merged from per-worker accumulators on the PARALLEL
// phase-2, which a permanently serial helper could never exercise. parallel
// selects the arm, and
// TestCreateConstraint_SeedIsIdenticalSerialAndParallel runs both.
func newConstraintEngineAfterRollback(tb testing.TB, ddl, stmt string, parallel bool) *Engine {
	tb.Helper()
	e := NewEngine(backfillSnapshotSeed(tb))
	e.parallelBackfillEnabled = parallel
	ctx := context.Background()

	tx, err := e.BeginTx(ctx)
	if err != nil {
		tb.Fatalf("BeginTx: %v", err)
	}
	sres, serr := tx.Exec(stmt, nil)
	drainConstraintStmt(tb, sres, serr)

	// The DDL runs to completion while the transaction is still open, so its
	// backfill and its value-set seed both meet the eager mutations.
	if ddl != "" {
		res, derr := e.Run(ctx, ddl, nil)
		if derr != nil {
			tb.Fatalf("%q: %v", ddl, derr)
		}
		drainConstraintStmt(tb, res, nil)
	}

	if err := tx.Rollback(); err != nil {
		tb.Fatalf("Rollback: %v", err)
	}
	return e
}

// drainConstraintStmt drains a statement result that must have succeeded.
func drainConstraintStmt(tb testing.TB, res *Result, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("statement: %v", err)
	}
	for res.Next() {
	}
	if derr := res.Err(); derr != nil {
		tb.Fatalf("drain: %v", derr)
	}
	_ = res.Close()
}

// runConstraintWrite runs a committed (autocommit) write and returns the error
// the caller saw, which is the UNIQUE enforcement verdict: nil means the write
// was ACCEPTED, non-nil means it was REFUSED.
func runConstraintWrite(tb testing.TB, e *Engine, stmt string) error {
	tb.Helper()
	res, err := e.RunAny(context.Background(), stmt, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	verdict := res.Err()
	_ = res.Close()
	return verdict
}

// countCommittedName counts, over a PREDICATE-FREE projection that no index can
// serve, how many committed Person nodes hold value under name. It is the oracle
// for an enforcement verdict: the structure under test must not be the thing that
// reports whether it kept its promise.
func countCommittedName(tb testing.TB, e *Engine, value string) int {
	tb.Helper()
	const query = `MATCH (n:Person) RETURN n.name`
	if plan := planOf(tb, e, query, nil); strings.Contains(plan, "NodeByIndexSeek") {
		tb.Fatalf("the duplicate count is not independent of the index — %q plans a seek:\n%s", query, plan)
	}
	res, err := e.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", query, err)
	}
	n := 0
	for res.Next() {
		if sv, ok := res.ValueAt(0).(expr.StringValue); ok && string(sv) == value {
			n++
		}
	}
	if derr := res.Err(); derr != nil {
		tb.Fatalf("drain %q: %v", query, derr)
	}
	_ = res.Close()
	return n
}

// uniqBackingCardinality reports how many nodes the UNIQUE backing index holds
// under value.
func uniqBackingCardinality(tb testing.TB, e *Engine, value string) uint64 {
	tb.Helper()
	return hashIndexCardinality(tb, e, exec.UniqueIndexName("Person", "name"), value)
}

// TestCreateConstraint_BackingIndexMatchesCommittedState is the CONTENT gate: the
// UNIQUE backing index must hold entries for EXACTLY the committed state — none
// for a value only an uncommitted transaction wrote, and one for the value the
// graph actually holds.
//
// It is asserted at content level as well as at query level because the two catch
// different instances of the same defect: on the createNode shape the rolled-back
// node loses its label, so [exec.NodeByIndexSeek]'s label residual filters the
// fabricated entry and no wrong ROW reaches the caller — yet an index that
// permanently retains an entry for a value no transaction ever committed is a
// defect whether or not today's residual masks it.
//
// The ghost half carries a sensitivity control, because a zero cardinality proves
// nothing from an accessor that can never see anything: a COMMITTED write of the
// same value must make the same accessor report a non-zero count.
func TestCreateConstraint_BackingIndexMatchesCommittedState(t *testing.T) {
	t.Parallel()
	for _, mut := range constraintSnapshotMutations() {
		t.Run(mut.name, func(t *testing.T) {
			t.Parallel()
			e := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, false)

			if got := uniqBackingCardinality(t, e, constraintSnapshotGhost); got != 0 {
				t.Errorf("after %q was rolled back mid-backfill, the UNIQUE backing index holds "+
					"%d entr(ies) for the GHOST value %q — the backfill indexed a value no "+
					"transaction ever committed, and the rollback does not invert it because the "+
					"change was never fanned out", mut.stmt, got, constraintSnapshotGhost)
			}
			if got := uniqBackingCardinality(t, e, constraintSnapshotReal); got != 1 {
				t.Errorf("after %q was rolled back mid-backfill, the UNIQUE backing index holds "+
					"%d entr(ies) for the COMMITTED value %q of the mutated node, want exactly 1 "+
					"— the backfill read the transaction's uncommitted state instead of the "+
					"committed one, so the real entry was never written",
					mut.stmt, got, constraintSnapshotReal)
			}

			// Sensitivity control for the ghost assertion above.
			commitStmt := `MATCH (n:Person) WHERE n.tag = 'k9' SET n.name = '` + constraintSnapshotGhost + `'`
			if err := runConstraintWrite(t, e, commitStmt); err != nil {
				t.Fatalf("the sensitivity control could not run: a COMMITTED write of %q was "+
					"refused (%v). Under a correct value-set that value is absent, so this write "+
					"must be accepted; the ghost assertion above cannot be trusted while it is not.",
					constraintSnapshotGhost, err)
			}
			if got := uniqBackingCardinality(t, e, constraintSnapshotGhost); got == 0 {
				t.Fatalf("the ghost assertion above is not sensitive: after a COMMITTED write of "+
					"%q the backing index still holds no entry for it, so this accessor could "+
					"never have observed a stale one either", constraintSnapshotGhost)
			}
		})
	}
}

// TestCreateConstraint_BackingIndexAnswersWhatTheGraphHolds is the QUERY-level
// gate: a seek served by the UNIQUE backing index must return exactly what a
// label scan over the same predicate returns.
//
// The oracle is a second engine with no constraint, carrying the same seed and
// the same rolled-back transaction. Non-vacuity is asserted on the REAL value,
// where the index is chosen in both builds — asserting it on the ghost value
// would be asserting the defect, since a fabricated entry is part of what makes
// the planner choose the index for an absent value.
func TestCreateConstraint_BackingIndexAnswersWhatTheGraphHolds(t *testing.T) {
	t.Parallel()
	for _, mut := range constraintSnapshotMutations() {
		t.Run(mut.name, func(t *testing.T) {
			t.Parallel()
			indexed := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, false)
			oracle := newConstraintEngineAfterRollback(t, "", mut.stmt, false)

			for _, value := range []string{constraintSnapshotGhost, constraintSnapshotReal} {
				query := `MATCH (n:Person) WHERE n.name = '` + value + `' RETURN n.tag`
				if plan := planOf(t, oracle, query, nil); strings.Contains(plan, "NodeByIndexSeek") {
					t.Fatalf("%s: the oracle is not an oracle — no constraint was created on it, "+
						"yet its plan uses a seek:\n%s", query, plan)
				}
				want := backfillSnapshotRows(t, oracle, query)
				got := backfillSnapshotRows(t, indexed, query)
				if !sameStringSet(want, got) {
					t.Fatalf("%s\nafter %q was rolled back mid-backfill:\n"+
						"  constrained engine = %d rows %v\n"+
						"  label scan         = %d rows %v\n"+
						"  constrained plan   = %s\n"+
						"the two must be identical: extra rows are FABRICATED (the graph does "+
						"not hold them) and missing rows are LOST",
						query, mut.stmt, len(got), summariseRows(got), len(want), summariseRows(want),
						planOf(t, indexed, query, nil))
				}
			}

			realProbe := `MATCH (n:Person) WHERE n.name = '` + constraintSnapshotReal + `' RETURN n.tag`
			if plan := planOf(t, indexed, realProbe, nil); !strings.Contains(plan, "NodeByIndexSeek") {
				t.Fatalf("%s: the comparison above was vacuous — the constrained engine's plan "+
					"does not use the backing index after %q was rolled back mid-backfill:\n%s",
					realProbe, mut.stmt, plan)
			}
		})
	}
}

// TestCreateConstraint_UniqueEnforcementAfterRollbackMidBackfill is the
// ENFORCEMENT gate, and it is the assertion that makes this fix not a copy of rmp
// #2778: the value-set is the structure a write is actually checked against, so a
// fix that repaired the index and moved an enforcement verdict would have traded
// a wrong row for a wrong write.
//
// Both directions are asserted, on every mutation shape:
//
//	A. A COMMITTED write of the ROLLED-BACK value must be ACCEPTED — the graph
//	   never held it — and a SECOND write of it must then be REFUSED, which is
//	   what proves the acceptance was enforcement working rather than enforcement
//	   absent.
//	B. A write that would DUPLICATE the value the graph actually holds must be
//	   REFUSED, and the committed graph must still hold exactly ONE node with it.
//
// Pre-fix, A was refused on setProperty and createNode (a legitimate write
// rejected against a value nothing committed) and B was accepted on setProperty,
// removeLabel and deleteNode, leaving two committed nodes sharing one value under
// an active UNIQUE constraint.
func TestCreateConstraint_UniqueEnforcementAfterRollbackMidBackfill(t *testing.T) {
	t.Parallel()
	for _, mut := range constraintSnapshotMutations() {
		t.Run(mut.name+"/rolledBackValueIsWritable", func(t *testing.T) {
			t.Parallel()
			e := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, false)

			first := `MATCH (n:Person) WHERE n.tag = 'k11' SET n.name = '` + constraintSnapshotGhost + `'`
			if err := runConstraintWrite(t, e, first); err != nil {
				t.Fatalf("after %q was rolled back mid-backfill, a COMMITTED write of %q was "+
					"REFUSED (%v) — the graph holds no node with that value, so the value-set "+
					"was seeded from the rolled-back transaction's uncommitted state and is "+
					"refusing a legitimate write", mut.stmt, constraintSnapshotGhost, err)
			}
			second := `MATCH (n:Person) WHERE n.tag = 'k12' SET n.name = '` + constraintSnapshotGhost + `'`
			if err := runConstraintWrite(t, e, second); err == nil {
				t.Fatalf("after %q was rolled back mid-backfill, a SECOND committed write of %q "+
					"was ACCEPTED — enforcement is absent, not merely correct, so the acceptance "+
					"of the first write proves nothing", mut.stmt, constraintSnapshotGhost)
			}
			if got := countCommittedName(t, e, constraintSnapshotGhost); got != 1 {
				t.Errorf("after %q was rolled back mid-backfill and one accepted write of %q, "+
					"%d committed nodes hold it, want exactly 1",
					mut.stmt, constraintSnapshotGhost, got)
			}
		})

		t.Run(mut.name+"/committedValueCannotBeDuplicated", func(t *testing.T) {
			t.Parallel()
			e := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, false)

			dup := `MATCH (n:Person) WHERE n.tag = 'k11' SET n.name = '` + constraintSnapshotReal + `'`
			err := runConstraintWrite(t, e, dup)
			got := countCommittedName(t, e, constraintSnapshotReal)
			if err == nil {
				t.Fatalf("after %q was rolled back mid-backfill, a committed write DUPLICATING "+
					"%q was ACCEPTED, and %d committed nodes now hold it under an ACTIVE UNIQUE "+
					"constraint — the value-set lost the committed value because it was seeded "+
					"from the transaction's uncommitted state. This is a Consistency breach, not "+
					"a wrong row.", mut.stmt, constraintSnapshotReal, got)
			}
			if got != 1 {
				t.Errorf("after %q was rolled back mid-backfill, %d committed nodes hold %q, "+
					"want exactly 1", mut.stmt, got, constraintSnapshotReal)
			}
		})
	}
}

// TestCreateConstraint_SeedIsIdenticalSerialAndParallel gates the ONE piece of
// this fix that the tests above cannot reach: the value-set seed is accumulated
// per worker on the parallel phase-2 of [Engine.backfillNodeHashIndex] and merged
// afterwards, and every test above forces the backfill serial.
//
// # The first version of this test could not fail, and that was measured
//
// It compared only the two probed values and the two enforcement verdicts. With
// the merge deliberately altered to DROP THE LAST WORKER'S VALUES it still
// passed on all four shapes, because the dropped values belong to nodes no probe
// names. A gate that cannot fail is worse than no gate, so the coverage
// assertion below was added and the same neutralisation now fails it.
//
// # What the coverage assertion does
//
// [chunkRepresentativeNames] replicates the worker/chunk arithmetic of
// [Engine.backfillNodeHashIndex] over the same mapper walk order and returns one
// committed name from EVERY chunk. A duplicate of each must be REFUSED, so
// dropping any chunk's values from the merge is caught wherever that chunk is.
// It is deliberately white-box: the property under test is a property of that
// partitioning, and a black-box sample cannot promise to hit every chunk at an
// arbitrary GOMAXPROCS.
//
// What it does NOT catch is the loss of a single value from INSIDE one chunk;
// that would be a defect in processRange, which the serial path shares and the
// tests above already exercise. Dropping the merge entirely, or merging a part
// twice, are both caught more loudly: an empty seed flips an enforcement verdict,
// and a duplicated part makes [exec.ConstraintRegistry.SeedUniqueValues] report a
// duplicate and the whole DDL fail.
//
// The seed is 20 000 nodes, above [backfillParallelMinNodes], so the parallel arm
// really does fan out; a change to that floor that silently made this arm serial
// would make the comparison vacuous, which is why the size is asserted first.
func TestCreateConstraint_SeedIsIdenticalSerialAndParallel(t *testing.T) {
	t.Parallel()
	if backfillSnapshotSeedSize < backfillParallelMinNodes {
		t.Fatalf("this comparison is vacuous: the seed is %d nodes, below the parallel floor of %d",
			backfillSnapshotSeedSize, backfillParallelMinNodes)
	}
	for _, mut := range constraintSnapshotMutations() {
		t.Run(mut.name, func(t *testing.T) {
			t.Parallel()
			serial := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, false)
			par := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, true)

			for _, value := range []string{constraintSnapshotGhost, constraintSnapshotReal} {
				s := uniqBackingCardinality(t, serial, value)
				p := uniqBackingCardinality(t, par, value)
				if s != p {
					t.Errorf("backing index cardinality for %q differs: serial %d, parallel %d",
						value, s, p)
				}
			}
			for _, tc := range []struct {
				what  string
				value string
			}{
				{"rolled-back value", constraintSnapshotGhost},
				{"committed value", constraintSnapshotReal},
			} {
				stmt := `MATCH (n:Person) WHERE n.tag = 'k11' SET n.name = '` + tc.value + `'`
				se := runConstraintWrite(t, serial, stmt)
				pe := runConstraintWrite(t, par, stmt)
				if (se == nil) != (pe == nil) {
					t.Errorf("the enforcement verdict for a write of the %s (%q) differs between "+
						"the serial and the parallel backfill: serial=%v parallel=%v — the "+
						"per-worker seed merge lost or invented a value",
						tc.what, tc.value, se, pe)
				}
			}

			// COVERAGE: one committed value from every worker chunk, so a merge
			// that drops any chunk is caught wherever that chunk falls. Run on a
			// FRESH parallel engine, because the verdict probes above have already
			// written to par.
			cov := newConstraintEngineAfterRollback(t, constraintSnapshotDDL, mut.stmt, true)
			names := chunkRepresentativeNames(t, cov)
			if len(names) < 2 {
				t.Fatalf("the coverage assertion is vacuous: %d chunk representatives, want at "+
					"least 2 — the parallel phase-2 did not fan out on this host", len(names))
			}
			for _, name := range names {
				stmt := `MATCH (n:Person) WHERE n.tag = 'k1' SET n.name = '` + name + `'`
				if err := runConstraintWrite(t, cov, stmt); err == nil {
					t.Fatalf("a committed write DUPLICATING %q was ACCEPTED after a PARALLEL "+
						"backfill: that value is committed in the graph, so the per-worker seed "+
						"merge lost the chunk it belongs to (%d chunk representatives probed)",
						name, len(names))
				}
			}
		})
	}
}

// chunkRepresentativeNames returns the committed "name" of one node from every
// range the parallel phase-2 of [Engine.backfillNodeHashIndex] partitions the
// mapper walk into, replicating that function's worker and chunk arithmetic over
// the same walk order.
//
// It skips the node the caller writes onto (tag k1), because setting a node's own
// current value is not a duplicate, and it skips the rolled-back ghost value,
// which by construction is committed nowhere.
func chunkRepresentativeNames(tb testing.TB, e *Engine) []string {
	tb.Helper()
	mapper := e.g.AdjList().Mapper()
	var keys []string
	mapper.Walk(func(_ graph.NodeID, key string) bool {
		keys = append(keys, key)
		return true
	})
	workers := runtime.GOMAXPROCS(0)
	if workers > len(keys) {
		workers = len(keys)
	}
	if workers < 1 {
		workers = 1
	}
	chunk := (len(keys) + workers - 1) / workers
	var out []string
	for lo := 0; lo < len(keys); lo += chunk {
		hi := lo + chunk
		if hi > len(keys) {
			hi = len(keys)
		}
		for i := lo; i < hi; i++ {
			if keys[i] == "k1" {
				continue
			}
			pv, ok := e.g.GetNodeProperty(keys[i], "name")
			if !ok {
				continue
			}
			sv, ok := pv.String()
			if !ok || sv == constraintSnapshotGhost {
				continue
			}
			out = append(out, sv)
			break
		}
	}
	return out
}

// TestCreateConstraint_CommittedDuplicateHiddenByOpenTxRefusesTheConstraint pins
// the OTHER defect the live seed carried, in the opposite direction: a duplicate
// that the COMMITTED graph holds and an open transaction has eagerly removed.
//
// Pre-fix the live scan could not see it, so CREATE CONSTRAINT succeeded and the
// rollback restored the duplicate: measured on the pre-fix build, the constraint
// was REGISTERED (`constraint registered = true`) over committed data holding
// "dup" twice. Post-fix the seed reads committed state,
// [exec.ConstraintRegistry.SeedUniqueValues] finds the duplicate, and the
// pre-existing unwind refuses the constraint with nothing registered.
//
// This is the reason the "unreachable in practice" note on that seed error was
// removed rather than kept: it is now the path that closes this hole.
func TestCreateConstraint_CommittedDuplicateHiddenByOpenTxRefusesTheConstraint(t *testing.T) {
	t.Parallel()
	e, tx := constraintDupEngine(t, map[string]string{"k1": "dup", "k2": "dup", "k3": "c"},
		`MATCH (n:Person) WHERE n.tag = 'k1' SET n.name = 'other'`)

	err := runConstraintWrite(t, e, constraintSnapshotDDL)
	if err == nil {
		t.Errorf("CREATE CONSTRAINT succeeded while the COMMITTED graph held \"dup\" twice and " +
			"an open transaction had eagerly removed one of them: the constraint is now active " +
			"over violating committed data, which is a Consistency breach. The value-set must be " +
			"seeded from committed state so the duplicate is seen.")
	}
	if err != nil && !strings.Contains(err.Error(), "duplicate value") {
		t.Errorf("CREATE CONSTRAINT was refused, but not for the duplicate: %v", err)
	}
	if e.constraintReg.HasUnique("Person", "name") {
		t.Errorf("the refused CREATE CONSTRAINT left a UNIQUE constraint registered on " +
			"(Person).name — the unwind did not run")
	}
	if rerr := tx.Rollback(); rerr != nil {
		t.Fatalf("Rollback: %v", rerr)
	}
	if got := countCommittedName(t, e, "dup"); got != 2 {
		t.Fatalf("this test is not testing what it claims: after the rollback the committed graph "+
			"holds %d nodes named \"dup\", want 2", got)
	}
}

// TestCreateConstraint_UncommittedDuplicateStillRefusesTheConstraint is the
// counterweight, and it is why the VALIDATION scan was deliberately left reading
// the live graph while the seed moved to a snapshot.
//
// An explicit transaction that has EAGERLY written a duplicate has not committed
// it. A snapshot-reading validation would therefore accept the constraint — and
// nothing would refuse that transaction's commit afterwards, because UNIQUE is
// reserved at WRITE time and the statement ran before the constraint existed.
// Measured while designing this fix: with the validation moved to the snapshot,
// CREATE CONSTRAINT succeeded, the COMMIT returned nil, and two committed nodes
// held "b" under an active UNIQUE constraint.
//
// So this test fails on the tempting one-line version of the fix — moving
// scanLabelProperty to the snapshot as well — and that is exactly its purpose.
func TestCreateConstraint_UncommittedDuplicateStillRefusesTheConstraint(t *testing.T) {
	t.Parallel()
	e, tx := constraintDupEngine(t, map[string]string{"k1": "a", "k2": "b", "k3": "c"},
		`MATCH (n:Person) WHERE n.tag = 'k1' SET n.name = 'b'`)
	defer func() { _ = tx.Rollback() }()

	if err := runConstraintWrite(t, e, constraintSnapshotDDL); err == nil {
		t.Fatalf("CREATE CONSTRAINT succeeded while an open transaction held an eager, "+
			"uncommitted duplicate of \"b\". Nothing checks UNIQUE at commit for a statement "+
			"that ran before the constraint existed, so that transaction can now commit the "+
			"duplicate: %d committed nodes would hold it under an active constraint. The "+
			"validation scan must keep reading the live graph.", 2)
	}
}

// constraintDupEngine builds a small Person graph from tag -> name, opens an
// explicit transaction and runs stmt eagerly inside it, returning the engine and
// the still-OPEN transaction. The caller decides whether to commit or roll back,
// which is the variable the two tests above differ on.
func constraintDupEngine(tb testing.TB, names map[string]string, stmt string) (*Engine, *ExplicitTx) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for key, name := range names {
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("seed label %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(name)); err != nil {
			tb.Fatalf("seed name %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "tag", lpg.StringValue(key)); err != nil {
			tb.Fatalf("seed tag %s: %v", key, err)
		}
	}
	e := NewEngine(g)
	e.parallelBackfillEnabled = false
	tx, err := e.BeginTx(context.Background())
	if err != nil {
		tb.Fatalf("BeginTx: %v", err)
	}
	sres, serr := tx.Exec(stmt, nil)
	drainConstraintStmt(tb, sres, serr)
	return e, tx
}

// TestUniqueValueSeed_CoversNonStringValues pins the one detail that makes the
// seed correct where the index alone would not be: the two structures cover
// DIFFERENT value sets by design.
//
// The backing hash index takes only [projectStringPropValue]-projectable strings,
// while [exec.ConstraintRegistry.SeedUniqueValues] canonicalises every non-null
// value, numbers included. The seed is therefore recorded BEFORE the projection.
// Recording it after would silently drop numeric values from the value-set and
// stop UNIQUE being enforced over a numeric property at all — a defect no
// string-valued test could observe.
func TestUniqueValueSeed_CoversNonStringValues(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.SetNodeLabel(key, "Acct"); err != nil {
			t.Fatalf("label %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "num", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("num %d: %v", i, err)
		}
	}
	e := NewEngine(g)
	e.parallelBackfillEnabled = false

	if err := runConstraintWrite(t, e,
		`CREATE CONSTRAINT c_num FOR (n:Acct) REQUIRE n.num IS UNIQUE`); err != nil {
		t.Fatalf("CREATE CONSTRAINT on a numeric property: %v", err)
	}
	// n0 holds 0; writing 0 onto n3 must be refused by the value-set, which is
	// the only structure that can refuse it — the hash index never indexed a
	// number.
	if err := runConstraintWrite(t, e, `MATCH (n:Acct) WHERE n.num = 3 SET n.num = 0`); err == nil {
		t.Fatalf("a committed write duplicating the numeric value 0 was ACCEPTED: the UNIQUE " +
			"value-set was seeded from values that had already been filtered through the hash " +
			"index's string projection, so no numeric value ever reached it")
	}
}
