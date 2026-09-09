package cypher

// index_build_replay_test.go — rmp #2793 gate: the catch-up replay of rmp #2738
// applies a recorded change from the state it was RECORDED with, so a second
// transaction's eager, uncommitted mutation cannot fabricate or suppress an
// index entry through the replay leg.
//
// # The defect these tests pin
//
// A [index.Change] does not describe its own effect on an index. A bound index
// asks the graph two further questions about the changed node: is it eligible
// for this index (live, and carrying the bound label), and — for a label add or
// remove, which carries no property payload — what is its current value of the
// bound property. On the live fan-out both are asked at the instant the change
// is fanned out. The build-log replay asked them at REPLAY time, which is a
// different instant and, crucially, a LIVE read: a statement on an explicit
// transaction applies eagerly, so the property bag, the label bitmap and the
// node-life records carry the writes of every open transaction.
//
// Measured at HEAD on the hash arm, one change in the log and one open
// transaction (rmp #2793, and reproduced with no concurrency at all — see "Why
// there is no race" below):
//
//	door                 index content after FinishBuild        want
//	currentValue         ghost=1, real-x=0                      ghost=0, real-x=1
//	tombstone            committed-7=0                          committed-7=1
//	labelBitmap          stray=1                                stray=0
//
// The interfering transaction then rolls back. Nothing inverts what the replay
// wrote, because its [exec.IndexBuffer] describes changes that were never fanned
// out — the same reason rmp #2778's fabricated backfill entry was permanent.
//
// # Why there is no race
//
// The task this closes described the defect as needing real concurrency. It does
// not. What it needs is a change IN THE LOG and an open transaction across the
// replay, and both are reachable in program order because a build is three
// separate calls: [Engine.beginIndexBuild] opens the window, the backfill fills
// the index, and [index.Manager.FinishBuild] replays and registers. These tests
// therefore make exactly the calls [Engine.createHashIndexLocked] makes, in
// exactly its order, with the committed write and the interfering transaction
// placed between them — the same white-box shape
// TestIndexUnderConstructionIsUnreachableByEveryPlannerRoute uses, and
// deterministic for the same reason.
//
// End-to-end reachability through the CREATE INDEX statement itself is covered
// by TestCreateIndexConcurrent_ReplayIgnoresUncommittedMutations below, which
// does race and says so.
//
// # What the oracle is
//
// A label scan over the same predicate on a SECOND engine that carries no index
// and has run the SAME committed write and the SAME rolled-back transaction. The
// index's cardinality for a value must equal the number of rows that scan
// returns for it — for every probed value, so a fabricated entry and a lost one
// are both caught and neither can mask the other.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// replaySeedSize is the number of :Person nodes the fixture seeds. It is small
// on purpose: nothing here depends on the backfill being slow, because nothing
// here is timed.
const replaySeedSize = 20

// replaySeed builds the graph every case starts from: replaySeedSize :Person
// nodes carrying a distinct "name", a distinct EVEN "age" and a "tag" that is
// the node's own key, plus ONE node that is NOT a :Person and carries committed
// values of both properties.
//
// The non-Person node is what the currentValue and labelBitmap doors need: a
// node whose committed label state and whose uncommitted label state differ.
func replaySeed(tb testing.TB) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < replaySeedSize; i++ {
		key := fmt.Sprintf("k%d", i)
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("seed label %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(fmt.Sprintf("seed-%d", i))); err != nil {
			tb.Fatalf("seed name %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "tag", lpg.StringValue(key)); err != nil {
			tb.Fatalf("seed tag %d: %v", i, err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i*2))); err != nil {
			tb.Fatalf("seed age %d: %v", i, err)
		}
	}
	if err := g.SetNodeLabel("kx", "Other"); err != nil {
		tb.Fatalf("seed kx label: %v", err)
	}
	for key, pv := range map[string]lpg.PropertyValue{
		"name": lpg.StringValue("real-x"),
		"tag":  lpg.StringValue("kx"),
		"age":  lpg.Int64Value(1001),
	} {
		if err := g.SetNodeProperty("kx", key, pv); err != nil {
			tb.Fatalf("seed kx %s: %v", key, err)
		}
	}
	return g
}

// replayArm is one index the CREATE INDEX paths build, with everything needed to
// construct it, populate it, read its content back, and write a Cypher literal
// for one of its keys.
type replayArm struct {
	// kind names the arm in the subtest, and prop is the property it indexes.
	kind, prop string
	// name is the manager key the index is registered under.
	name string
	// build constructs the index and backfills it from rv, exactly as the DDL
	// path for this kind does.
	build func(tb testing.TB, e *Engine, ctx context.Context, rv *lpg.ReadView[string, float64]) index.Subscriber
	// cardinality reports how many nodes the registered index holds under v.
	cardinality func(tb testing.TB, sub index.Subscriber, v any) uint64
	// lit renders v as the Cypher literal for this arm's property.
	lit func(v any) string
	// ghost, fresh and stray are the three values the doors write; probes is
	// every value whose index cardinality must match the label scan.
	ghost, fresh, stray any
	probes              []any
}

// replayArms covers every index a CREATE INDEX registers, because a fix proven
// on one says nothing about the others: the hash index and its numeric companion
// come from [Engine.createHashIndexLocked], the string btree and the same
// companion from [Engine.createBTreeIndexLocked], and all three are caught up by
// the same build log.
func replayArms() []replayArm {
	return []replayArm{
		{
			kind: "hash", prop: "name", name: "person_name",
			build: func(tb testing.TB, e *Engine, ctx context.Context, rv *lpg.ReadView[string, float64]) index.Subscriber {
				tb.Helper()
				idx, err := newBoundNodeHashIndex(e.g.ReadAt(nil), "Person", "name")
				if err != nil {
					tb.Fatalf("bind hash: %v", err)
				}
				if err := e.backfillNodeHashIndex(ctx, rv, idx, "Person", "name", nil); err != nil {
					tb.Fatalf("backfill hash: %v", err)
				}
				return idx
			},
			cardinality: func(tb testing.TB, sub index.Subscriber, v any) uint64 {
				tb.Helper()
				idx, ok := sub.(*indexhash.Index[string])
				if !ok {
					tb.Fatalf("index is %T, want *hash.Index[string]", sub)
				}
				return idx.Cardinality(v.(string))
			},
			lit:   func(v any) string { return "'" + v.(string) + "'" },
			ghost: "ghost", fresh: "committed-7", stray: "stray",
			probes: []any{"ghost", "committed-7", "stray", "real-x", "seed-7", "seed-3"},
		},
		{
			kind: "btree", prop: "name", name: "person_name_btree",
			build: func(tb testing.TB, e *Engine, ctx context.Context, rv *lpg.ReadView[string, float64]) index.Subscriber {
				tb.Helper()
				idx, err := newBoundNodeBTreeIndex(e.g.ReadAt(nil), "Person", "name")
				if err != nil {
					tb.Fatalf("bind btree: %v", err)
				}
				if err := e.backfillNodeBTreeIndex(ctx, rv, idx, "Person", "name"); err != nil {
					tb.Fatalf("backfill btree: %v", err)
				}
				return idx
			},
			cardinality: func(tb testing.TB, sub index.Subscriber, v any) uint64 {
				tb.Helper()
				idx, ok := sub.(*indexbtree.Index[string])
				if !ok {
					tb.Fatalf("index is %T, want *btree.Index[string]", sub)
				}
				return idx.Cardinality(v.(string))
			},
			lit:   func(v any) string { return "'" + v.(string) + "'" },
			ghost: "ghost", fresh: "committed-7", stray: "stray",
			probes: []any{"ghost", "committed-7", "stray", "real-x", "seed-7", "seed-3"},
		},
		{
			kind: "numericCompanion", prop: "age", name: numericBTreeName("Person", "age"),
			build: func(tb testing.TB, e *Engine, ctx context.Context, rv *lpg.ReadView[string, float64]) index.Subscriber {
				tb.Helper()
				idx, err := newBoundNodeBTreeIndexNumeric(e.g.ReadAt(nil), "Person", "age")
				if err != nil {
					tb.Fatalf("bind numeric: %v", err)
				}
				if err := e.backfillNodeBTreeIndexNumeric(ctx, rv, idx, "Person", "age"); err != nil {
					tb.Fatalf("backfill numeric: %v", err)
				}
				return idx
			},
			cardinality: func(tb testing.TB, sub index.Subscriber, v any) uint64 {
				tb.Helper()
				idx, ok := sub.(*indexbtree.Index[float64])
				if !ok {
					tb.Fatalf("index is %T, want *btree.Index[float64]", sub)
				}
				return idx.Cardinality(v.(float64))
			},
			lit:   func(v any) string { return fmt.Sprintf("%d", int64(v.(float64))) },
			ghost: float64(999), fresh: float64(777), stray: float64(888),
			probes: []any{float64(999), float64(777), float64(888), float64(1001), float64(14), float64(6)},
		},
	}
}

// replayDoor is one live read the replay used to make, and the pair of
// statements that poisons it: a write that COMMITS during the build, so it lands
// in the catch-up log, and an eager write on an explicit transaction that is
// still OPEN when the log is replayed.
type replayDoor struct {
	name string
	// committed lands in the build log.
	committed func(a replayArm) string
	// eager is applied to the graph and never committed.
	eager func(a replayArm) string
	// why names the failure the door produces before the fix.
	why string
}

// replayDoors enumerates the three reads the replay resolved from the graph, one
// per structure the binding consults. Each is poisoned on its own, and each
// fails on its own, so a fix that closed only one would still fail here:
//
//   - currentValue — the versioned property bag, reached on a LABEL change,
//     which carries no property payload. An uncommitted SET makes the replay
//     index the uncommitted value AND lose the committed one.
//   - tombstone — the node-life records, reached through Eligible. An
//     uncommitted DETACH DELETE makes the replay drop a committed property
//     write.
//   - labelBitmap — the label bitmap index, also reached through Eligible and
//     read as an unversioned candidate structure. An uncommitted label add makes
//     the replay index a node the committed graph gives no such label.
//
// The three are not interchangeable, and which one a given interference opens
// was measured rather than assumed: an uncommitted REMOVE of the label does NOT
// open the eligibility door, because the label bitmap is a union that additions
// set and removals never clear, so Eligible still answers true. That is why the
// suppression door here is a DELETE and not a label removal.
func replayDoors() []replayDoor {
	return []replayDoor{
		{
			name:      "currentValue",
			committed: func(a replayArm) string { return `MATCH (n) WHERE n.tag = 'kx' SET n:Person` },
			eager: func(a replayArm) string {
				return `MATCH (n) WHERE n.tag = 'kx' SET n.` + a.prop + ` = ` + a.lit(a.ghost)
			},
			why: "the replay of the label add read the property bag at replay time, where an " +
				"open transaction's uncommitted value was sitting",
		},
		{
			name: "tombstone",
			committed: func(a replayArm) string {
				return `MATCH (n:Person) WHERE n.tag = 'k7' SET n.` + a.prop + ` = ` + a.lit(a.fresh)
			},
			eager: func(a replayArm) string { return `MATCH (n:Person) WHERE n.tag = 'k7' DETACH DELETE n` },
			why: "the replay of the property write asked whether the node was still live, and " +
				"an open transaction had eagerly deleted it",
		},
		{
			name: "labelBitmap",
			committed: func(a replayArm) string {
				return `MATCH (n) WHERE n.tag = 'kx' SET n.` + a.prop + ` = ` + a.lit(a.stray)
			},
			eager: func(a replayArm) string { return `MATCH (n) WHERE n.tag = 'kx' SET n:Person` },
			why: "the replay of the property write asked whether the node carried the label, and " +
				"an open transaction had eagerly added it",
		},
	}
}

// replayLabelScanCount returns how many :Person nodes an engine WITH NO INDEX
// reports for prop = lit. It is the oracle: what the graph actually holds.
func replayLabelScanCount(tb testing.TB, e *Engine, prop, lit string) uint64 {
	tb.Helper()
	query := `MATCH (n:Person) WHERE n.` + prop + ` = ` + lit + ` RETURN n.tag`
	if plan := planOf(tb, e, query, nil); strings.Contains(plan, "NodeByIndex") {
		tb.Fatalf("the oracle is not an oracle — no index was created on it, yet its plan "+
			"uses an index operator:\n%s", plan)
	}
	return uint64(rowsOf(tb, e, query, nil))
}

// runReplayCase performs the build sequence [Engine.createHashIndexLocked]
// performs, with the door's committed write and its still-open transaction
// placed between the backfill and the replay, and returns the engine with the
// index registered.
//
// withIndex == false builds the ORACLE: the same seed, the same committed write
// and the same rolled-back transaction, with no build and no index at all.
func runReplayCase(tb testing.TB, arm *replayArm, door *replayDoor, withIndex bool) (*Engine, index.Subscriber) {
	tb.Helper()
	e := NewEngine(replaySeed(tb))
	e.parallelBackfillEnabled = false
	ctx := context.Background()
	mgr := e.g.IndexManager()

	var (
		buildLog *index.BuildLog
		scanView *lpg.ReadView[string, float64]
		release  = func() {}
		sub      index.Subscriber
	)
	if withIndex {
		// EXACTLY the DDL's order: open the recording, snapshot, then backfill.
		buildLog, scanView, release = e.beginIndexBuild(mgr, "Person", arm.prop)
		defer mgr.AbandonBuild(buildLog)
		defer release()
		sub = arm.build(tb, e, ctx, scanView)
	}

	// A transaction COMMITS inside the build window, so its change reaches no
	// index and is recorded in the log instead. This is rmp #2738's window.
	execStatement(tb, e, door.committed(*arm))
	if withIndex && buildLog.Len() == 0 {
		tb.Fatalf("nothing was recorded for %q: the replay leg is not being exercised at all, "+
			"so every assertion below would be vacuous", door.committed(*arm))
	}

	// A SECOND transaction applies an eager, uncommitted mutation and stays OPEN
	// across the replay. This is what the replay must not observe.
	tx, err := e.BeginTx(ctx)
	if err != nil {
		tb.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.ExecAny(door.eager(*arm), nil)
	if err != nil {
		tb.Fatalf("Exec %q: %v", door.eager(*arm), err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", door.eager(*arm), err)
	}
	_ = res.Close()

	if withIndex {
		release()
		if err := mgr.FinishBuild(buildLog, func(reg index.RegisterFunc) error {
			return reg(arm.name, sub)
		}); err != nil {
			tb.Fatalf("FinishBuild: %v", err)
		}
		e.ClearPlanCache()
	}

	if err := tx.Rollback(); err != nil {
		tb.Fatalf("Rollback: %v", err)
	}
	return e, sub
}

// execStatement runs one autocommit statement to completion.
func execStatement(tb testing.TB, e *Engine, stmt string) {
	tb.Helper()
	res, err := e.RunAny(context.Background(), stmt, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", stmt, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", stmt, err)
	}
	_ = res.Close()
}

// TestFinishBuildReplay_IgnoresUncommittedMutations is the CORRECTNESS gate of
// rmp #2793: after the catch-up replay, the index holds entries for EXACTLY the
// committed state — for every probed value, on every index a CREATE INDEX
// builds, through every door the replay used to read the graph through.
func TestFinishBuildReplay_IgnoresUncommittedMutations(t *testing.T) {
	t.Parallel()
	for _, arm := range replayArms() {
		for _, door := range replayDoors() {
			t.Run(arm.kind+"/"+door.name, func(t *testing.T) {
				t.Parallel()
				indexed, sub := runReplayCase(t, &arm, &door, true)
				oracle, _ := runReplayCase(t, &arm, &door, false)

				for _, v := range arm.probes {
					lit := arm.lit(v)
					want := replayLabelScanCount(t, oracle, arm.prop, lit)
					got := arm.cardinality(t, sub, v)
					if got != want {
						t.Errorf("after %q was committed into the build window and %q was rolled "+
							"back across the replay, the index holds %d entr(ies) for %s and a "+
							"label scan over the same predicate returns %d:\n  %s",
							door.committed(arm), door.eager(arm), got, lit, want, door.why)
					}
				}

				// Sensitivity: the accessor must be able to observe an entry at
				// all. A committed write of the door's ghost value onto a node
				// that IS a :Person must show up through the very same accessor,
				// otherwise a zero above would prove nothing.
				execStatement(t, indexed,
					`MATCH (n:Person) WHERE n.tag = 'k9' SET n.`+arm.prop+` = `+arm.lit(arm.ghost))
				if got := arm.cardinality(t, sub, arm.ghost); got == 0 {
					t.Fatalf("the assertions above are not sensitive: after a COMMITTED write of "+
						"%s the index still holds no entry for it, so this accessor could never "+
						"have observed a fabricated one either", arm.lit(arm.ghost))
				}
			})
		}
	}
}

// TestFinishBuildReplay_FabricatedAndLostRowsOnTheSeek states the currentValue
// door's measurement as rows rather than as index content, on the one arm where
// the difference is directly a wrong ANSWER: the equality seek plans no residual
// filter on the value, so a fabricated entry is returned as a row.
//
// It is deliberately redundant with the hash/currentValue case above. That case
// proves the general property; this one names the two halves separately, so a
// future change that broke only one of them would say which.
func TestFinishBuildReplay_FabricatedAndLostRowsOnTheSeek(t *testing.T) {
	t.Parallel()
	arm := replayArms()[0] // hash
	door := replayDoors()[0]
	indexed, _ := runReplayCase(t, &arm, &door, true)
	oracle, _ := runReplayCase(t, &arm, &door, false)

	for _, tc := range []struct {
		value string
		why   string
	}{
		{"ghost", "the graph holds no :Person with this name — the transaction that wrote it " +
			"rolled back, so an entry for it was FABRICATED by the replay and never inverted"},
		{"real-x", "the graph holds exactly this node — the replay read the transaction's " +
			"uncommitted overwrite instead, so the committed value was never indexed"},
	} {
		query := `MATCH (n:Person) WHERE n.name = '` + tc.value + `' RETURN n.tag`
		if plan := planOf(t, indexed, query, nil); !strings.Contains(plan, "NodeByIndexSeek") {
			t.Fatalf("seek(%q): the assertion would be vacuous — not an index seek:\n%s",
				tc.value, plan)
		}
		want := replaySortedRows(t, oracle, query)
		got := replaySortedRows(t, indexed, query)
		if !sameStringSet(want, got) {
			t.Errorf("seek(%q) = %v, label scan = %v — %s", tc.value, got, want, tc.why)
		}
	}
}

// replaySortedRows runs a single-column query and returns its column sorted, so two
// answers can be compared as sets.
func replaySortedRows(tb testing.TB, e *Engine, query string) []string {
	tb.Helper()
	res, err := e.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", query, err)
	}
	defer res.Close() // test teardown
	var out []string
	for res.Next() {
		out = append(out, res.ValueAt(0).String())
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", query, err)
	}
	sort.Strings(out)
	return out
}

// concurrentReplayTrials is how many times
// TestCreateIndexConcurrent_ReplayIgnoresUncommittedMutations opens the window.
// The trial only counts when the transactions really did run inside the DDL, and
// the test fails if none of them did, so the number is a budget for the racing —
// never a threshold the assertion is graded against.
const concurrentReplayTrials = 5

// TestCreateIndexConcurrent_ReplayIgnoresUncommittedMutations is the
// REACHABILITY gate: everything the white-box tests above assert must also hold
// when the build window is opened by the real CREATE INDEX statement rather than
// by its parts.
//
// It exists because the white-box tests make the DDL's calls without being the
// DDL, and a reader is entitled to ask whether the window they open is the window
// a statement opens. Here it is the statement, and the two transactions are real
// concurrent explicit transactions — the only writers the DDL's schema gate does
// not exclude, which is why rmp #2738's catch-up log exists at all.
//
// The shape is the currentValue door: transaction one CREATEs a :Person inside
// the build window, so its label and property changes reach no index and are
// recorded; transaction two then overwrites that node's name EAGERLY and is
// still open when the replay runs, so a replay that re-read the property bag
// would index the uncommitted name. The rollback leaves the graph holding
// 'target' and never 'ghost', and the index must say the same.
//
// The backfill is forced SERIAL so the window is wide, exactly as
// TestExplicitTx_CommitFanOutCannotEscapeADDLBarrier does; the host this ran on
// was not idle, and the trial accounting below is what makes the result readable
// anyway.
func TestCreateIndexConcurrent_ReplayIgnoresUncommittedMutations(t *testing.T) {
	t.Parallel()
	const seed = 20_000
	raced := 0

	for trial := 0; trial < concurrentReplayTrials; trial++ {
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
			res, err := e.Run(ctx, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`, nil)
			if err != nil {
				t.Errorf("CREATE INDEX: %v", err)
				return
			}
			for res.Next() {
			}
			_ = res.Close()
			ddlEnd = time.Now()
		}()

		// Let the DDL get past the node-list capture at the head of its backfill,
		// so the node the first transaction creates cannot be in it.
		time.Sleep(500 * time.Microsecond)

		// Transaction one: commits INSIDE the window, so its changes are recorded
		// rather than delivered. An autocommit statement could not do this — the
		// DDL's schema gate excludes it for the whole build.
		committer, err := e.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx (committer): %v", err)
		}
		drainTx(t, committer, `CREATE (n:Person {name:'target'})`)
		if err := committer.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		// Transaction two: eager, uncommitted, and still open across the replay.
		interferer, err := e.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx (interferer): %v", err)
		}
		drainTx(t, interferer, `MATCH (n:Person) WHERE n.name = 'target' SET n.name = 'ghost'`)
		openedAt := time.Now()

		wg.Wait()
		if err := interferer.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if openedAt.Before(ddlEnd) {
			raced++
		}

		// The graph holds 'target' and has never held a committed 'ghost'.
		plan := planOf(t, e, `MATCH (n:Person) WHERE n.name = $n RETURN n.name`,
			map[string]expr.Value{"n": expr.StringValue("target")})
		if !strings.Contains(plan, "NodeByIndexSeek") {
			t.Fatalf("trial %d: the assertions would be vacuous — not an index seek:\n%s", trial, plan)
		}
		if got := schemaGateSeekRows(t, e, "ghost"); got != 0 {
			t.Errorf("trial %d: the indexed predicate returns %d rows for a name NO transaction "+
				"ever committed, want 0 — the catch-up replay resolved the recorded change "+
				"against the live property bag, where an open transaction's uncommitted write "+
				"was sitting, and the rollback does not invert what the replay wrote", trial, got)
		}
		if got := schemaGateSeekRows(t, e, "target"); got != 1 {
			t.Errorf("trial %d: the indexed predicate returns %d rows for the committed name, "+
				"want 1 — the replay indexed the uncommitted value in its place", trial, got)
		}
	}

	if raced == 0 {
		t.Fatalf("no trial opened the window: in every one of %d trials the CREATE INDEX had "+
			"already finished before the interfering transaction opened its eager write, so "+
			"nothing above exercised the replay leg", concurrentReplayTrials)
	}
	t.Logf("%d of %d trials had the interfering transaction open inside the DDL", raced, concurrentReplayTrials)
}

// drainTx runs one statement on an explicit transaction to completion.
func drainTx(tb testing.TB, tx *ExplicitTx, stmt string) {
	tb.Helper()
	res, err := tx.ExecAny(stmt, nil)
	if err != nil {
		tb.Fatalf("Exec %q: %v", stmt, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", stmt, err)
	}
	_ = res.Close()
}
