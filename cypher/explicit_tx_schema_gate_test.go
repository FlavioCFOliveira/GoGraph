package cypher

// explicit_tx_schema_gate_test.go — rmp #2738 gate: a CREATE INDEX catches up on
// every change committed while it was building, so a transaction's commit-time
// index fan-out cannot be lost inside the DDL's scan-and-register window.
//
// # The defect these tests pin
//
// [ExplicitTx.Commit] flushes the transaction's whole [exec.IndexBuffer] through
// index.Manager.ApplyBatch, which fans every buffered change out to the indexes
// registered AT THAT MOMENT. CREATE INDEX backfills its new index from a node
// list captured at the start of the statement and registers it only afterwards.
// A whole transaction — BEGIN, the CREATE, the buffer flush and the commit-record
// publication — fitted inside that window: the backfill's node list predated the
// node, and the fan-out found no index to write it to. The node was committed and
// visible to a scan while an index seek over the same predicate returned nothing,
// permanently. Measured at 20 losses in 20 trials before the fix, against 0 in 20
// for the identical autocommit workload, which the DDL's schema gate excludes.
//
// # Why the fix is a catch-up log and NOT an exclusion
//
// The obvious fix — make an explicit transaction's statements take the schema
// gate SHARED, exactly as an autocommit statement does — closes the window and
// DEADLOCKS. An explicit transaction is a registered store writer from BEGIN, so
// blocking it on the gate closes a three-way cycle with the store's quiesce
// ([txn.Store.RunUnderCommitLock], the checkpointer's and Close's seam):
//
//	T (open transaction, registered) waits for → the schema gate
//	D (DDL, holds the gate)          waits for → writer admission
//	Q (quiesce, admission closed)    waits for → T's registration
//
// Reproduced as a 20-second hang. Making it safe would mean inverting the
// module's documented lock order — schemaGate (outermost) → store writer
// admission → visMu — at every DDL site, which is not a trade this module makes.
//
// So the DDL does not exclude the transaction at all. It RECORDS what the
// transaction does and replays it: [index.Manager.BeginBuild] logs every change
// fanned out while the backfill runs, and [index.Manager.FinishBuild] replays
// the log into the new index under the manager's exclusive lock at the instant
// the index becomes reachable. The design is PostgreSQL's CREATE INDEX
// CONCURRENTLY insight — an index built concurrently with writes must reconcile
// with them before it is usable — with the reconciliation done from a change log
// rather than from a second table scan, and with the half-built index kept
// unreachable by ABSENCE from the manager's map rather than by an
// indisvalid-style flag every reader would have to remember to check. See
// graph/index/build.go for the full argument and the pinned citations.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// schemaGateSeed builds a graph of n labelled nodes each carrying a distinct
// "name", large enough that a serial CREATE INDEX backfill over it takes long
// enough to interleave a whole transaction against.
func schemaGateSeed(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("seed label %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(fmt.Sprintf("seed-%d", i))); err != nil {
			tb.Fatalf("seed prop %d: %v", i, err)
		}
	}
	return g
}

// schemaGateSeekRows runs the indexed predicate and returns the row count.
func schemaGateSeekRows(tb testing.TB, e *Engine, name string) int {
	tb.Helper()
	res, err := e.Run(context.Background(),
		`MATCH (n:Person) WHERE n.name = $n RETURN n.name`,
		map[string]expr.Value{"n": expr.StringValue(name)})
	if err != nil {
		tb.Fatalf("seek %q: %v", name, err)
	}
	defer res.Close() // test teardown
	rows := 0
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("seek %q drain: %v", name, err)
	}
	return rows
}

// TestExplicitTx_CommitFanOutCannotEscapeADDLBarrier is the CONSISTENCY gate
// (ACID Mandate 2), and the reproduction this whole change exists to close. A
// whole explicit transaction runs while a CREATE INDEX backfill is in flight; the
// node it commits must be findable through the indexed predicate afterwards.
//
// Both index kinds are exercised, because both DDL paths were changed: the hash
// path registers its pair directly under the manager's lock, and the btree path
// registers through exec.CreateIndexOp inside the graph's visibility barrier.
// A fix proven on one says nothing about the other.
//
// The assertion is guarded against two ways of passing vacuously:
//
//   - the plan is checked to name the index operator, so the row count exercises
//     the index rather than a label scan that would find the node either way;
//   - the trial is only counted when the transaction actually STARTED while the
//     DDL was still in flight, and the test fails if no trial ever did. It
//     cannot instead require the transaction to FINISH first: making it wait for
//     the DDL is precisely the rejected approach, which deadlocks.
//
// The backfill is forced SERIAL (the same white-box toggle
// TestBackfillNodeHashIndex_SerialVsParallelIdentical uses) so the window is
// wide and the test does not depend on the scheduler.
func TestExplicitTx_CommitFanOutCannotEscapeADDLBarrier(t *testing.T) {
	t.Parallel()
	const seed = 20_000
	const trials = 5

	for _, arm := range []struct {
		kind   string
		ddl    string
		probe  string
		params map[string]expr.Value
		wantOp string
	}{
		{
			kind:   "hash",
			ddl:    `CREATE INDEX person_name FOR (n:Person) ON (n.name)`,
			probe:  `MATCH (n:Person) WHERE n.name = $n RETURN n.name`,
			params: map[string]expr.Value{"n": expr.StringValue("target")},
			wantOp: "NodeByIndexSeek",
		},
		{
			// A btree serves the RANGE access path, not the equality seek — the
			// seek rewrite admits only a hash index — so the probe must be a range
			// predicate for the assertion to exercise the index at all.
			kind:   "btree",
			ddl:    `CREATE INDEX person_name FOR (n:Person) ON (n.name) OPTIONS {indexType: 'btree'}`,
			probe:  `MATCH (n:Person) WHERE n.name STARTS WITH 'target' RETURN n.name`,
			params: nil,
			wantOp: "NodeByIndexRangeScan",
		},
	} {
		t.Run(arm.kind, func(t *testing.T) {
			t.Parallel()
			raced := 0
			for trial := 0; trial < trials; trial++ {
				e := NewEngine(schemaGateSeed(t, seed))
				e.parallelBackfillEnabled = false
				ctx := context.Background()

				// Pay the parse and plan-cache cost outside the raced window.
				if _, err := e.Explain(`CREATE (n:Person {name:'warm'})`, nil); err != nil {
					t.Fatalf("warm: %v", err)
				}

				var (
					wg     sync.WaitGroup
					ddlEnd time.Time
				)
				wg.Add(1)
				go func() {
					defer wg.Done()
					res, err := e.Run(ctx, arm.ddl, nil)
					if err != nil {
						t.Errorf("CREATE INDEX: %v", err)
						return
					}
					for res.Next() {
					}
					_ = res.Close()
					ddlEnd = time.Now()
				}()

				// Let the DDL get past the node-list capture at the head of its
				// backfill, so the node this transaction creates cannot be in it.
				time.Sleep(500 * time.Microsecond)

				txStart := time.Now()
				tx, err := e.BeginTx(ctx)
				if err != nil {
					t.Fatalf("BeginTx: %v", err)
				}
				res, err := tx.ExecAny(`CREATE (n:Person {name:'target'})`, nil)
				if err != nil {
					t.Fatalf("Exec: %v", err)
				}
				for res.Next() {
				}
				_ = res.Close()
				if err := tx.Commit(); err != nil {
					t.Fatalf("Commit: %v", err)
				}
				wg.Wait()

				if plan := planOf(t, e, arm.probe, arm.params); !strings.Contains(plan, arm.wantOp) {
					t.Fatalf("trial %d: the assertion would be vacuous — the plan does not use the index (no %q):\n%s",
						trial, arm.wantOp, plan)
				}
				if got := rowsOf(t, e, arm.probe, arm.params); got != 1 {
					t.Fatalf("trial %d: a COMMITTED node is missing from the index: "+
						"the indexed predicate returned %d rows, want 1", trial, got)
				}
				if txStart.Before(ddlEnd) {
					raced++
				}
			}
			if raced == 0 {
				t.Fatalf("no trial opened its transaction while the DDL was still in flight: "+
					"the window under test never opened, so the %d passes prove nothing", trials)
			}
		})
	}
}

// TestExplicitTx_SchemaGateExcludesAutocommitButNotATransaction pins the
// ASYMMETRY the whole design rests on, in both directions, so neither half can be
// changed silently.
//
// With the schema gate held STRONGLY — precisely what a DDL holds across its
// scan-and-register sequence — an AUTOCOMMIT statement must block, and a
// statement on an EXPLICIT TRANSACTION must NOT.
//
// The autocommit half is a real exclusion and must stay one: it is why the only
// writer that can commit into a DDL's window is an explicit transaction, which is
// what bounds the catch-up log ([index.MaxBuildLogChanges]).
//
// The explicit-transaction half looks like the defect and is in fact the fix's
// precondition. Making that arm block is the rejected approach, and it deadlocks:
// the transaction has been a registered store writer since BEGIN, so waiting on
// the gate closes the three-way cycle with the store quiesce described in this
// file's header and gated by
// TestSchemaGate_DDLDoesNotHoldTheGateAcrossWriterAdmission. Its writes are made
// safe by the build log instead, which is what
// TestExplicitTx_CommitFanOutCannotEscapeADDLBarrier proves.
func TestExplicitTx_SchemaGateExcludesAutocommitButNotATransaction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		arm         string
		wantBlocked bool
		why         string
	}{
		{"autocommit", true,
			"an autocommit statement must be excluded by a DDL's schema gate; without that " +
				"exclusion every writer could commit into a CREATE INDEX window and the catch-up " +
				"log would be unbounded"},
		{"explicitTx", false,
			"a statement on an explicit transaction must NOT block on the schema gate: the " +
				"transaction is a registered store writer from BEGIN, so waiting there deadlocks " +
				"against a store quiesce. Its writes are caught by the build log, not excluded"},
	} {
		t.Run(tc.arm, func(t *testing.T) {
			t.Parallel()
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			e := NewEngine(g)
			ctx := context.Background()

			e.schemaGate.StrongLock()

			done := make(chan error, 1)
			go func() {
				done <- runOneWrite(ctx, e, tc.arm)
			}()

			completed := false
			select {
			case err := <-done:
				completed = true
				if err != nil {
					t.Errorf("%s: %v", tc.arm, err)
				}
			case <-time.After(250 * time.Millisecond):
			}
			e.schemaGate.StrongUnlock()
			if !completed {
				if err := <-done; err != nil {
					t.Fatalf("%s after the gate was released: %v", tc.arm, err)
				}
			}
			if blocked := !completed; blocked != tc.wantBlocked {
				t.Fatalf("%s: blocked on the schema gate = %v, want %v — %s",
					tc.arm, blocked, tc.wantBlocked, tc.why)
			}
		})
	}
}

// runOneWrite performs a single CREATE through the named write entry point.
func runOneWrite(ctx context.Context, e *Engine, arm string) error {
	drain := func(r *Result) error {
		for r.Next() {
		}
		err := r.Err()
		_ = r.Close()
		return err
	}
	if arm == "autocommit" {
		res, err := e.RunInTxAny(ctx, `CREATE (n:Person {name:'x'})`, nil)
		if err != nil {
			return err
		}
		return drain(res)
	}
	tx, err := e.BeginTx(ctx)
	if err != nil {
		return err
	}
	res, err := tx.ExecAny(`CREATE (n:Person {name:'x'})`, nil)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := drain(res); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TestSchemaGate_DDLDoesNotHoldTheGateAcrossWriterAdmission is the LIVENESS gate
// that forbids the REJECTED fix from being reintroduced.
//
// It builds the exact three-party state in which making an explicit
// transaction's statements take the schema gate deadlocks, and requires the
// transaction to finish:
//
//	T (open transaction, registered) → would wait for the schema gate
//	D (DDL, holds the gate)          → waits for writer admission
//	Q (quiesce, gate closed)         → waits for T's registration
//
// T is a registered store writer from BEGIN ([txn.Store.BeginCtx] registers it
// for the quiesce accounting), Q is [txn.Store.RunUnderCommitLock] — the
// checkpointer's and Close's seam — and D is a CREATE INDEX. Under the chosen
// design the cycle cannot close, because T waits for NOTHING: its statements take
// no schema gate, and the writes they make are reconciled by the DDL's catch-up
// log instead of being excluded. A build carrying the exclusion hangs here for
// the full 20 seconds.
//
// The test therefore guards the design decision itself, not an implementation
// detail. It stays green if the lock order is ever inverted to make the exclusion
// safe — that is a different, deliberate change — but it goes red the moment the
// exclusion is added WITHOUT that inversion, which is the mistake it exists to
// catch.
func TestSchemaGate_DDLDoesNotHoldTheGateAcrossWriterAdmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	wr, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = wr.Close() }() // test teardown
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	e := NewEngineWithStore(st)
	ctx := context.Background()

	seed, err := e.RunInTxAny(ctx, `CREATE (n:Person {name:'seed'})`, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for seed.Next() {
	}
	_ = seed.Close()

	// T: an open explicit transaction — a registered store writer since BEGIN.
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	first, err := tx.ExecAny(`CREATE (n:Person {name:'a'})`, nil)
	if err != nil {
		t.Fatalf("first statement: %v", err)
	}
	for first.Next() {
	}
	_ = first.Close()

	// Q: closes the admission gate and drains — it now waits for T.
	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		if qerr := st.RunUnderCommitLock(func() error { return nil }); qerr != nil {
			t.Errorf("quiesce: %v", qerr)
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// D: a DDL, which must not reach the schema gate before writer admission.
	ddl := make(chan struct{})
	go func() {
		defer close(ddl)
		res, derr := e.Run(ctx, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`, nil)
		if derr != nil {
			t.Errorf("CREATE INDEX: %v", derr)
			return
		}
		for res.Next() {
		}
		_ = res.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	// T's next statement and its commit must still be able to finish.
	finished := make(chan error, 1)
	go func() {
		res, eerr := tx.ExecAny(`CREATE (n:Person {name:'b'})`, nil)
		if eerr != nil {
			finished <- eerr
			return
		}
		for res.Next() {
		}
		_ = res.Close()
		finished <- tx.Commit()
	}()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("second statement: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: the transaction could not finish while a quiesce drained " +
			"and a DDL held the schema gate; the DDL must take writer admission first")
	}
	<-quiesced
	<-ddl
}

// TestExplicitTx_StraddlingTransactionIsCaughtByTheBackfill covers the OTHER
// shape of the window: a transaction whose statement ran BEFORE the DDL started
// but whose COMMIT — and therefore whose index fan-out — lands inside it.
//
// Two independent mechanisms make that write survive, and this test passes if
// EITHER holds, which is deliberate:
//
//   - the catch-up log records the fan-out and replays it at registration
//     ([index.Manager.BeginBuild]); and
//   - the backfill scan reads the PHYSICAL LATEST state
//     ([lpg.Graph.HasNodeLabel] and [lpg.Graph.GetNodeProperty] read the live bag,
//     not a snapshot) and this transaction's mutations are applied EAGERLY at
//     Exec, so the still-uncommitted node is already there for the backfill to
//     find.
//
// The second mechanism is the one that made this case survive BEFORE rmp #2738,
// and it is not a property to rely on: reading uncommitted state is also how the
// backfill can index a value a transaction later ROLLS BACK. The first mechanism
// is the one the fix adds, and it is exact.
func TestExplicitTx_StraddlingTransactionIsCaughtByTheBackfill(t *testing.T) {
	t.Parallel()
	const seed = 20_000

	e := NewEngine(schemaGateSeed(t, seed))
	e.parallelBackfillEnabled = false
	ctx := context.Background()

	// The statement runs BEFORE the DDL starts; only the commit straddles.
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.ExecAny(`CREATE (n:Person {name:'straddle'})`, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for res.Next() {
	}
	_ = res.Close()

	var (
		wg     sync.WaitGroup
		ddlEnd time.Time
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		dres, derr := e.Run(ctx, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`, nil)
		if derr != nil {
			t.Errorf("CREATE INDEX: %v", derr)
			return
		}
		for dres.Next() {
		}
		_ = dres.Close()
		ddlEnd = time.Now()
	}()

	time.Sleep(500 * time.Microsecond) // the DDL is now inside its backfill
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	commitEnd := time.Now()
	wg.Wait()

	// The commit — and with it the whole index fan-out — must have COMPLETED before
	// the DDL did, or the fan-out could have reached an already-registered index and
	// the backfill would not be the thing under test.
	if !commitEnd.Before(ddlEnd) {
		t.Fatalf("the commit did not complete inside the DDL's window, so this test proves nothing")
	}
	plan, err := e.Explain(`MATCH (n:Person) WHERE n.name = $n RETURN n.name`,
		map[string]expr.Value{"n": expr.StringValue("straddle")})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("the assertion would be vacuous — the plan is not an index seek:\n%s", plan)
	}
	if got := schemaGateSeekRows(t, e, "straddle"); got != 1 {
		t.Fatalf("a transaction that committed inside the DDL window is missing from "+
			"the index: the indexed predicate returned %d rows, want 1", got)
	}
}

// schemaGateSeedTyped builds n :Person nodes carrying a string "name" and an
// integer "age", so one graph can exercise the equality-seek, key-set-seek and
// range-scan access paths.
func schemaGateSeedTyped(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("seed label %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(fmt.Sprintf("seed-%d", i))); err != nil {
			tb.Fatalf("seed name %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("seed age %d: %v", i, err)
		}
	}
	return g
}

// planOf returns the rendered plan for query, failing the test on a plan error.
func planOf(tb testing.TB, e *Engine, query string, params map[string]expr.Value) string {
	tb.Helper()
	plan, err := e.Explain(query, params)
	if err != nil {
		tb.Fatalf("Explain %q: %v", query, err)
	}
	return plan
}

// rowsOf runs query and returns the number of rows it produced.
func rowsOf(tb testing.TB, e *Engine, query string, params map[string]expr.Value) int {
	tb.Helper()
	res, err := e.Run(context.Background(), query, params)
	if err != nil {
		tb.Fatalf("Run %q: %v", query, err)
	}
	defer res.Close() // test teardown
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", query, err)
	}
	return n
}

// TestIndexUnderConstructionIsUnreachableByEveryPlannerRoute is the CORRECTNESS
// gate of the whole rmp #2738 design, and it is the one that must not be
// weakened: an index that is still being built must never answer a query.
//
// A half-built index is WORSE than the missing-entry defect this change fixes.
// A missing entry loses a row; an index consulted while it is still filling
// FABRICATES an answer — it reports "no such node" for a node the graph holds.
// The design's whole claim to correctness is that this cannot happen.
//
// Here that claim is not a flag the planner is trusted to honour. The index under
// construction is simply NOT IN [index.Manager]'s map, and [index.Manager.GetIndex],
// [index.Manager.ListIndexes] and [index.Manager.Count] are the only ways any
// caller can reach a registered subscriber — the map itself is unexported and is
// touched nowhere outside graph/index/manager.go. So the property is structural.
// This test confirms it holds where it is observable: through the real planner,
// on every access path that consults the manager, with the index FULLY POPULATED
// so a leak would be visible rather than merely empty.
//
// Each route is checked twice — the plan must not name the index operator, and
// the answer must still be RIGHT — because a route that declined the index but
// returned the wrong rows would be just as much a defect.
func TestIndexUnderConstructionIsUnreachableByEveryPlannerRoute(t *testing.T) {
	t.Parallel()
	const seed = 500
	e := NewEngine(schemaGateSeedTyped(t, seed))
	e.parallelBackfillEnabled = false
	ctx := context.Background()
	mgr := e.g.IndexManager()

	// Open a build and populate, under it, exactly the index set a CREATE INDEX
	// statement builds: the user hash index on (Person, name), the auto-named
	// string btree on (Person, name), and the internal numeric companion on
	// (Person, age). None is registered while the build is open.
	buildLog := mgr.BeginBuild()
	defer mgr.AbandonBuild(buildLog)

	hashIdx, err := newBoundNodeHashIndex(e.g.ReadAt(nil), "Person", "name")
	if err != nil {
		t.Fatalf("bind hash: %v", err)
	}
	if err := e.backfillNodeHashIndex(ctx, hashIdx, "Person", "name"); err != nil {
		t.Fatalf("backfill hash: %v", err)
	}
	btreeIdx, err := newBoundNodeBTreeIndex(e.g.ReadAt(nil), "Person", "name")
	if err != nil {
		t.Fatalf("bind btree: %v", err)
	}
	if err := e.backfillNodeBTreeIndex(ctx, btreeIdx, "Person", "name"); err != nil {
		t.Fatalf("backfill btree: %v", err)
	}
	numIdx, err := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), "Person", "age")
	if err != nil {
		t.Fatalf("bind numeric: %v", err)
	}
	if err := e.backfillNodeBTreeIndexNumeric(ctx, numIdx, "Person", "age"); err != nil {
		t.Fatalf("backfill numeric: %v", err)
	}
	// The indexes really are populated: a leak below would surface a WRONG plan,
	// not merely an empty one. Asserted so this test cannot pass vacuously by
	// having built nothing.
	if got := hashIdx.Cardinality("seed-7"); got != 1 {
		t.Fatalf("the hash index under construction is not populated (cardinality %d): "+
			"the rest of this test would prove nothing", got)
	}

	// Every accessor that can reach a registered subscriber.
	if n := mgr.Count(); n != 0 {
		t.Fatalf("Manager.Count reports %d indexes while all of them are still building, want 0", n)
	}
	if names := mgr.ListIndexes(); len(names) != 0 {
		t.Fatalf("Manager.ListIndexes reports %v while all of them are still building, want none", names)
	}
	for _, name := range []string{"person_name", "person_name_btree", numericBTreeName("Person", "age")} {
		if _, gerr := mgr.GetIndex(name); gerr == nil {
			t.Fatalf("Manager.GetIndex(%q) resolved an index that is still building", name)
		}
	}

	// Every planner route that consults the manager, with the answer each must
	// still produce. wantOp is the operator that MUST NOT appear while the build
	// is open, and MAY appear once it is finished.
	seekParams := map[string]expr.Value{"n": expr.StringValue("seed-7")}
	routes := []struct {
		name     string
		query    string
		params   map[string]expr.Value
		wantOp   string
		wantRows int
	}{
		{"equality seek", `MATCH (n:Person) WHERE n.name = $n RETURN n.name`,
			seekParams, "NodeByIndexSeek", 1},
		{"key-set seek", `UNWIND ['seed-7','seed-8'] AS k MATCH (n:Person) WHERE n.name = k RETURN n.name`,
			nil, "NodeByIndexSeekSet", 2},
		{"string range scan", `MATCH (n:Person) WHERE n.name STARTS WITH 'seed-49' RETURN n.name`,
			nil, "NodeByIndexRangeScan", 11},
		{"numeric range scan", `MATCH (n:Person) WHERE n.age > 496 RETURN n.age`,
			nil, "NodeByIndexRangeScan", 3},
	}
	for _, r := range routes {
		if plan := planOf(t, e, r.query, r.params); strings.Contains(plan, r.wantOp) {
			t.Errorf("%s: the planner reached an index that is still BUILDING — plan contains %q:\n%s",
				r.name, r.wantOp, plan)
		}
		if got := rowsOf(t, e, r.query, r.params); got != r.wantRows {
			t.Errorf("%s: returned %d rows while the index was building, want %d",
				r.name, got, r.wantRows)
		}
	}

	// The introspection routes, which read the same map.
	if got := rowsOf(t, e, `SHOW INDEXES`, nil); got != 0 {
		t.Errorf("SHOW INDEXES reported %d rows while every index was still building, want 0", got)
	}
	if got := rowsOf(t, e, `CALL db.indexes()`, nil); got != 0 {
		t.Errorf("db.indexes() reported %d rows while every index was still building, want 0", got)
	}

	// Finish the build. The same routes must now light up and give the SAME
	// answers — otherwise "unreachable" would be indistinguishable from "the
	// indexes were never usable at all", and the assertions above would prove
	// nothing about the building state specifically.
	if err := mgr.FinishBuild(buildLog, func(reg index.RegisterFunc) error {
		if rerr := reg("person_name", hashIdx); rerr != nil {
			return rerr
		}
		if rerr := reg("person_name_btree", btreeIdx); rerr != nil {
			return rerr
		}
		return reg(numericBTreeName("Person", "age"), numIdx)
	}); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	e.ClearPlanCache()

	for _, r := range routes {
		if plan := planOf(t, e, r.query, r.params); !strings.Contains(plan, r.wantOp) {
			t.Errorf("%s: after FinishBuild the planner still does not use the index "+
				"(no %q), so the building-state assertion above proves nothing:\n%s",
				r.name, r.wantOp, plan)
		}
		if got := rowsOf(t, e, r.query, r.params); got != r.wantRows {
			t.Errorf("%s: returned %d rows after FinishBuild, want %d", r.name, got, r.wantRows)
		}
	}
}
