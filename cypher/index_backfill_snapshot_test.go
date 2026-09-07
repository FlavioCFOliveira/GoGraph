package cypher

// index_backfill_snapshot_test.go — rmp #2778 gate: a CREATE INDEX backfill
// indexes COMMITTED state, so no seek can return a row the graph does not hold.
//
// # The defect these tests pin
//
// A statement on an explicit transaction applies to the graph EAGERLY: the
// property bag, the label bag and the node-life records carry its writes from
// the moment the statement runs, and only [ExplicitTx.Rollback]'s undo replay
// takes them back. The CREATE INDEX backfill read that live state, so it indexed
// values, labels and nodes that nothing had committed. Rollback then discarded
// the transaction's [exec.IndexBuffer] WITHOUT inverting it — correctly, since
// the buffer describes changes that were never fanned out to any index — and the
// backfill's own entry stayed in the new index permanently.
//
// The result is a WRONG answer where rmp #2738's was an incomplete one:
//
//	GRAPH (label scan, no index): name STARTS WITH 'ghost'  = 0 rows
//	                              name STARTS WITH 'seed-7' = 111 rows
//	INDEX (seek):                 seek('ghost')             = 1 rows  (want 0)
//	                              seek('seed-7')            = 0 rows  (want 1)
//
// measured at HEAD before the fix. The graph holds no node named ghost and the
// index returns one, and the real value is missing on top of that.
//
// NO CONCURRENCY IS REQUIRED. One transaction, one CREATE INDEX, one rollback,
// on a single goroutine — which is why these tests are deterministic and carry
// no trial loop, unlike rmp #2738's gate next door.
//
// # Why a stale entry is not merely filtered out
//
// [exec.NodeByIndexSeek]'s only residual check is on the node's LABEL; it never
// rechecks the value the seek was keyed by. So a fabricated entry is returned as
// a row rather than dropped. That is the difference from PostgreSQL, where an
// index entry points at a heap tuple whose MVCC visibility is rechecked on
// fetch — the reason rmp #2738 could not borrow CREATE INDEX CONCURRENTLY's
// phases 2-3, recorded in graph/index/build.go.
//
// # What the oracle is
//
// A label scan over the SAME predicate, on a SECOND engine that carries no index
// and has run the SAME transaction and rollback. The assertion is set equality
// between the two answers, not a row count, so a fabricated row and a missing
// row are both caught and neither can mask the other.
//
// # Why the plan check is on the real value and not on the ghost
//
// Measured, not assumed: the range rewrite CONSULTS the index and declines when
// the requested value is absent from it. On a 20 000-node seed with ages 0,2,…,
// `n.age = 15` plans a NodeByLabelScan while `n.age = 14` plans a
// NodeByIndexRangeScan, and likewise `STARTS WITH 'seed-1-'` scans while
// `STARTS WITH 'seed-7'` seeks. So a FABRICATED entry has a second observable
// effect beyond the wrong row: it is what makes the planner choose the index for
// the ghost predicate at all. Requiring the ghost probe to use the index would
// therefore be requiring the defect. Non-vacuity is asserted on the REAL value
// instead, where the index is chosen in both arms, and the row comparison —
// which is the correctness assertion — is run on both.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// backfillSnapshotSeedSize is large enough that the planner prefers an index for
// every arm below. It is not a race window: at fifty nodes a label scan really is
// cheaper than a range seek and the planner correctly says so, which would leave
// the btree and numeric-companion arms measuring nothing.
const backfillSnapshotSeedSize = 20_000

// backfillSnapshotSeed builds the graph every arm starts from: Person nodes
// carrying a distinct "name", a distinct EVEN "age", and a "tag" that is the
// node's own key.
//
// tag is what every probe RETURNs. It exists so a row has an identity
// independent of the predicate: returning n.name under a predicate on n.name
// would make the comparison partly tautological.
//
// age is even so that an ODD value inside the index's key range is guaranteed
// absent from the committed graph. The numeric arm needs a ghost value that is
// in range yet absent — see this file's header for why an out-of-range ghost
// cannot be used.
func backfillSnapshotSeed(tb testing.TB) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < backfillSnapshotSeedSize; i++ {
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
	return g
}

// backfillSnapshotRows runs query and returns its single column, sorted, so two
// answers can be compared as sets.
func backfillSnapshotRows(tb testing.TB, e *Engine, query string) []string {
	tb.Helper()
	res, err := e.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run %q: %v", query, err)
	}
	defer res.Close() // test teardown
	if cols := res.Columns(); len(cols) != 1 {
		tb.Fatalf("Run %q: got %d columns %v, want 1", query, len(cols), cols)
	}
	var out []string
	for res.Next() {
		v := res.ValueAt(0)
		sv, ok := v.(expr.StringValue)
		if !ok {
			tb.Fatalf("Run %q: n.tag is %v (%T), not a string — every node this test "+
				"creates carries one, so a non-string here means the row is not the row "+
				"it claims to be", query, v, v)
		}
		out = append(out, string(sv))
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", query, err)
	}
	sort.Strings(out)
	return out
}

// backfillSnapshotArm is one CREATE INDEX access path under test, with the
// literals and the probe shapes that exercise it.
type backfillSnapshotArm struct {
	// kind names the arm in the subtest.
	kind string
	// ddl is the CREATE INDEX statement, and it is what selects the DDL path.
	ddl string
	// wantOp is the operator that must appear in the indexed engine's plan for
	// realProbe, and must never appear in the oracle engine's.
	wantOp string
	// prop is the indexed property, which is also the property the rolled-back
	// SET targets and the one the rolled-back CREATE carries.
	prop string
	// ghostLit is a Cypher literal for a value NO committed node holds but which
	// lies INSIDE the index's committed key range; the rolled-back transaction
	// writes it.
	ghostLit string
	// ghostProbe finds the fabricated value. Its correct answer is always no
	// rows, whatever the mutation.
	ghostProbe string
	// realProbe finds committed rows including the node the mutation targets
	// (tag 'k7'). It catches the other half of the defect — the entry the
	// backfill should have written and did not — and it is where non-vacuity is
	// asserted.
	realProbe string
	// ghostEntries and realEntries report how many entries the registered index
	// holds under the GHOST value and under the exact committed value of the
	// mutated node, read from the concrete subscriber. They are the
	// content-level oracle of
	// TestCreateIndexBackfill_IndexContentMatchesCommittedState; see that test
	// for why index content, not only query rows, has to be asserted.
	ghostEntries func(tb testing.TB, e *Engine) uint64
	realEntries  func(tb testing.TB, e *Engine) uint64
}

// backfillSnapshotArms covers every DDL path a CREATE INDEX can take, because a
// fix proven on one says nothing about the others:
//
//   - hash — [Engine.createHashIndexLocked] and [Engine.backfillNodeHashIndex],
//     reached by an equality predicate. It is the only arm whose plan carries NO
//     residual filter above the index operator at all, so it is where a
//     fabricated entry is most directly a fabricated row.
//   - btree — [Engine.createBTreeIndexLocked] and
//     [Engine.backfillNodeBTreeIndex], reached by a string range predicate and
//     registered through [exec.CreateIndexOp] inside the visibility barrier.
//   - numericCompanion — [Engine.backfillNodeBTreeIndexNumeric], the float64
//     companion the HASH DDL path builds alongside its hash index. A third
//     backfill function on a fourth code path, reached by numeric equality.
func backfillSnapshotArms() []backfillSnapshotArm {
	return []backfillSnapshotArm{
		{
			kind:       "hash",
			ddl:        `CREATE INDEX ix_name FOR (n:Person) ON (n.name)`,
			wantOp:     "NodeByIndexSeek",
			prop:       "name",
			ghostLit:   `'seed-1-ghost'`,
			ghostProbe: `MATCH (n:Person) WHERE n.name = 'seed-1-ghost' RETURN n.tag`,
			realProbe:  `MATCH (n:Person) WHERE n.name = 'seed-7' RETURN n.tag`,
			ghostEntries: func(tb testing.TB, e *Engine) uint64 {
				return hashIndexCardinality(tb, e, "ix_name", "seed-1-ghost")
			},
			realEntries: func(tb testing.TB, e *Engine) uint64 {
				return hashIndexCardinality(tb, e, "ix_name", "seed-7")
			},
		},
		{
			kind:       "btree",
			ddl:        `CREATE INDEX ix_name FOR (n:Person) ON (n.name) OPTIONS {indexType: 'btree'}`,
			wantOp:     "NodeByIndexRangeScan",
			prop:       "name",
			ghostLit:   `'seed-1-ghost'`,
			ghostProbe: `MATCH (n:Person) WHERE n.name STARTS WITH 'seed-1-' RETURN n.tag`,
			realProbe:  `MATCH (n:Person) WHERE n.name STARTS WITH 'seed-7' RETURN n.tag`,
			ghostEntries: func(tb testing.TB, e *Engine) uint64 {
				return btreeIndexCardinality(tb, e, "ix_name", "seed-1-ghost")
			},
			realEntries: func(tb testing.TB, e *Engine) uint64 {
				return btreeIndexCardinality(tb, e, "ix_name", "seed-7")
			},
		},
		{
			kind:       "numericCompanion",
			ddl:        `CREATE INDEX ix_age FOR (n:Person) ON (n.age)`,
			wantOp:     "NodeByIndexRangeScan",
			prop:       "age",
			ghostLit:   `15`,
			ghostProbe: `MATCH (n:Person) WHERE n.age = 15 RETURN n.tag`,
			realProbe:  `MATCH (n:Person) WHERE n.age = 14 RETURN n.tag`,
			ghostEntries: func(tb testing.TB, e *Engine) uint64 {
				return numericIndexCardinality(tb, e, numericBTreeName("Person", "age"), 15)
			},
			realEntries: func(tb testing.TB, e *Engine) uint64 {
				return numericIndexCardinality(tb, e, numericBTreeName("Person", "age"), 14)
			},
		},
	}
}

// backfillSnapshotMutation is one shape of eager, uncommitted write that the
// backfill must not observe.
type backfillSnapshotMutation struct {
	name string
	stmt func(arm backfillSnapshotArm) string
}

// backfillSnapshotMutations enumerates the four shapes, one for each thing the
// backfill resolves through its read view — and each can be wrong on its own:
//
//   - setProperty — the property VALUE. Pre-fix this both fabricated the ghost
//     entry and lost the real one; it is the measurement quoted in the file
//     header.
//   - createNode — node EXISTENCE. A node an open transaction created and rolled
//     back must not be in the index; pre-fix the mapper walk found its id and the
//     live label and property bags admitted it.
//   - removeLabel — the LABEL. A label an open transaction removed and rolled
//     back must not exclude the node from the index.
//   - deleteNode — existence from the other direction: a node an open transaction
//     deleted and rolled back must still be in the index.
//
// setProperty and createNode produce a FABRICATED entry; removeLabel and
// deleteNode produce a MISSING one. Both directions are needed, which is why the
// query-level oracle is set equality and not merely "no unexpected rows".
//
// Each statement is rendered against the arm, so setProperty and createNode
// carry whichever property that arm indexes.
func backfillSnapshotMutations() []backfillSnapshotMutation {
	return []backfillSnapshotMutation{
		{"setProperty", func(a backfillSnapshotArm) string {
			return `MATCH (n:Person) WHERE n.tag = 'k7' SET n.` + a.prop + ` = ` + a.ghostLit
		}},
		{"createNode", func(a backfillSnapshotArm) string {
			return `CREATE (m:Person {tag: 'ghostnode', ` + a.prop + `: ` + a.ghostLit + `})`
		}},
		{"removeLabel", func(a backfillSnapshotArm) string {
			return `MATCH (n:Person) WHERE n.tag = 'k7' REMOVE n:Person`
		}},
		{"deleteNode", func(a backfillSnapshotArm) string {
			return `MATCH (n:Person) WHERE n.tag = 'k7' DETACH DELETE n`
		}},
	}
}

// TestCreateIndexBackfill_IgnoresUncommittedMutations is the CORRECTNESS gate of
// rmp #2778: an index must answer EXACTLY what a label scan over the same
// predicate answers, after a transaction whose eager mutations were rolled back
// while the backfill was reading them.
//
// Every shape in [backfillSnapshotMutations] is exercised against every DDL
// path, because a fix proven on one of either says nothing about the others.
//
// Three of the twelve cases — createNode on each arm — pass in BOTH builds, and
// that is recorded rather than hidden: a rolled-back CREATE loses its label, so
// the seek's label residual filters the stale entry and no wrong row reaches the
// caller. Those cases are gated at the content level instead, by
// TestCreateIndexBackfill_IndexContentMatchesCommittedState, which fails on the
// pre-fix build for all three.
func TestCreateIndexBackfill_IgnoresUncommittedMutations(t *testing.T) {
	t.Parallel()

	for _, arm := range backfillSnapshotArms() {
		for _, mut := range backfillSnapshotMutations() {
			t.Run(arm.kind+"/"+mut.name, func(t *testing.T) {
				t.Parallel()
				stmt := mut.stmt(arm)

				// The engine under test: the DDL runs while the transaction's
				// eager mutations are in the live graph, and the transaction is
				// rolled back afterwards.
				indexed := newEngineAfterRolledBackDDL(t, arm.ddl, stmt)
				// The ORACLE: the same seed and the same rolled-back
				// transaction, with NO index, so its answers are what the graph
				// actually holds.
				oracle := newEngineAfterRolledBackDDL(t, "", stmt)

				// The correctness assertion, on BOTH probes and before any
				// non-vacuity check, so a pre-fix run is diagnosed by the rows
				// it got wrong rather than by the plan the wrong rows produced.
				for _, query := range []string{arm.ghostProbe, arm.realProbe} {
					if plan := planOf(t, oracle, query, nil); strings.Contains(plan, arm.wantOp) {
						t.Fatalf("%s: the oracle is not an oracle — no index was created on it, "+
							"yet its plan uses %q:\n%s", query, arm.wantOp, plan)
					}
					want := backfillSnapshotRows(t, oracle, query)
					got := backfillSnapshotRows(t, indexed, query)
					if !sameStringSet(want, got) {
						t.Fatalf("%s\nafter %q was rolled back mid-backfill:\n"+
							"  indexed engine = %d rows %v\n"+
							"  label scan     = %d rows %v\n"+
							"  indexed plan   = %s\n"+
							"the two must be identical: extra rows are FABRICATED (the graph "+
							"does not hold them) and missing rows are LOST",
							query, stmt, len(got), summariseRows(got), len(want), summariseRows(want),
							planOf(t, indexed, query, nil))
					}
				}

				// Non-vacuity: the comparison above must have exercised the
				// index, not two label scans. Asserted on the real value, where
				// the index is chosen both before and after the fix — see this
				// file's header for the measurement that rules the ghost value
				// out of this check.
				if plan := planOf(t, indexed, arm.realProbe, nil); !strings.Contains(plan, arm.wantOp) {
					t.Fatalf("%s: the indexed engine's plan does not use the index (no %q) after %q "+
						"was rolled back mid-backfill:\n%s\n"+
						"Either the comparison above was vacuous (two label scans), or — since the "+
						"range rewrite CONSULTS the index and declines when the value is absent from "+
						"it — the index lost the entry for a value the graph holds. "+
						"TestCreateIndexBackfill_IndexContentMatchesCommittedState distinguishes the two.",
						arm.realProbe, arm.wantOp, stmt, plan)
				}
			})
		}
	}
}

// TestCreateIndexBackfill_RolledBackValueIsNeitherFabricatedNorLost states the
// header's measurement as a direct, self-describing assertion on the hash path,
// so the two halves of the defect are named separately in the failure output
// rather than only as a set difference.
//
// It is deliberately redundant with the hash/setProperty case above: that case
// proves the general property (index answer ≡ label-scan answer) and this one
// proves the two specific numbers the defect was reported with, so a future
// change that broke only one half would say which. The hash arm is the one that
// can carry it, because the equality seek plans no residual filter and is chosen
// for an absent value as readily as for a present one.
func TestCreateIndexBackfill_RolledBackValueIsNeitherFabricatedNorLost(t *testing.T) {
	t.Parallel()
	e := newEngineAfterRolledBackDDL(t,
		`CREATE INDEX ix_name FOR (n:Person) ON (n.name)`,
		`MATCH (n:Person) WHERE n.tag = 'k7' SET n.name = 'ghost'`)

	for _, tc := range []struct {
		value string
		want  int
		why   string
	}{
		{"ghost", 0, "the graph holds no node with this name — the transaction that wrote it " +
			"rolled back, so an entry for it was FABRICATED by the backfill and never inverted"},
		{"seed-7", 1, "the graph holds exactly this node — the backfill read the transaction's " +
			"uncommitted overwrite instead, so the committed value was never indexed"},
	} {
		query := `MATCH (n:Person) WHERE n.name = '` + tc.value + `' RETURN n.tag`
		if plan := planOf(t, e, query, nil); !strings.Contains(plan, "NodeByIndexSeek") {
			t.Fatalf("seek(%q): the assertion would be vacuous — not an index seek:\n%s", tc.value, plan)
		}
		if got := len(backfillSnapshotRows(t, e, query)); got != tc.want {
			t.Errorf("seek(%q) = %d rows, want %d — %s", tc.value, got, tc.want, tc.why)
		}
	}
}

// TestCreateIndexBackfill_IndexContentMatchesCommittedState asserts the property
// one level below the planner: the registered index must hold entries for
// EXACTLY the committed state — no entry for a value only an uncommitted
// transaction wrote, and the entry for the value the graph actually holds.
//
// It exists because the query-level oracle cannot see every instance of the
// defect, and because when it can, it does not always name it. Both measured on
// the neutralised build:
//
//   - createNode leaves a stale entry that is NOT observable as a wrong row.
//     The rolled-back node loses its label, so [exec.NodeByIndexSeek]'s label
//     residual filters it, and re-creating the same key afterwards mints a NEW
//     node id rather than recycling the stale one (verified: the seek still
//     returned no rows). The entry is latent, not wrong — but an index that
//     permanently retains entries for values no transaction ever committed is a
//     defect whether or not today's residual happens to mask it.
//   - on the btree and numeric arms a LOST entry makes the range rewrite decline
//     the index, because the rewrite consults it. The query then answers
//     correctly from a label scan, so the row comparison passes and only the
//     plan changes. Asserting content says "the entry is missing" directly
//     instead of leaving it to be inferred from a plan.
//
// The ghost half also carries a sensitivity control, because a zero
// cardinality proves nothing from an accessor that can never see anything: a
// COMMITTED write of the same value must make the same accessor report a
// non-zero count.
func TestCreateIndexBackfill_IndexContentMatchesCommittedState(t *testing.T) {
	t.Parallel()
	for _, arm := range backfillSnapshotArms() {
		for _, mut := range backfillSnapshotMutations() {
			t.Run(arm.kind+"/"+mut.name, func(t *testing.T) {
				t.Parallel()
				stmt := mut.stmt(arm)
				e := newEngineAfterRolledBackDDL(t, arm.ddl, stmt)

				if got := arm.ghostEntries(t, e); got != 0 {
					t.Errorf("after %q was rolled back mid-backfill, the index holds %d entr(ies) "+
						"for the GHOST value %s — the backfill indexed a value no transaction ever "+
						"committed, and the rollback does not invert it because the change was never "+
						"fanned out", stmt, got, arm.ghostLit)
				}
				if got := arm.realEntries(t, e); got != 1 {
					t.Errorf("after %q was rolled back mid-backfill, the index holds %d entr(ies) "+
						"for the COMMITTED value of the mutated node, want exactly 1 — the backfill "+
						"read the transaction's uncommitted state instead of the committed one, so "+
						"the real entry was never written", stmt, got)
				}

				commitGhostValue(t, e, &arm)
				if got := arm.ghostEntries(t, e); got == 0 {
					t.Fatalf("the ghost assertion above is not sensitive: after a COMMITTED write of "+
						"%s the index still holds no entry for it, so this accessor could never have "+
						"observed a stale one either", arm.ghostLit)
				}
			})
		}
	}
}

// commitGhostValue writes the arm's ghost value onto a committed node, so
// TestCreateIndexBackfill_IndexContentMatchesCommittedState can prove its accessor
// would have seen an entry if one had been there.
func commitGhostValue(tb testing.TB, e *Engine, arm *backfillSnapshotArm) {
	tb.Helper()
	stmt := `MATCH (n:Person) WHERE n.tag = 'k9' SET n.` + arm.prop + ` = ` + arm.ghostLit
	res, err := e.RunAny(context.Background(), stmt, nil)
	if err != nil {
		tb.Fatalf("%q: %v", stmt, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", stmt, err)
	}
	_ = res.Close()
}

// hashIndexCardinality reports how many nodes the registered hash index holds
// under value.
func hashIndexCardinality(tb testing.TB, e *Engine, name, value string) uint64 {
	tb.Helper()
	sub, err := e.g.IndexManager().GetIndex(name)
	if err != nil {
		tb.Fatalf("GetIndex %q: %v", name, err)
	}
	idx, ok := sub.(*indexhash.Index[string])
	if !ok {
		tb.Fatalf("index %q is %T, want *hash.Index[string]", name, sub)
	}
	return idx.Cardinality(value)
}

// btreeIndexCardinality reports how many nodes the registered string btree holds
// under value.
func btreeIndexCardinality(tb testing.TB, e *Engine, name, value string) uint64 {
	tb.Helper()
	sub, err := e.g.IndexManager().GetIndex(name)
	if err != nil {
		tb.Fatalf("GetIndex %q: %v", name, err)
	}
	idx, ok := sub.(*indexbtree.Index[string])
	if !ok {
		tb.Fatalf("index %q is %T, want *btree.Index[string]", name, sub)
	}
	return idx.Cardinality(value)
}

// numericIndexCardinality reports how many nodes the registered numeric
// companion btree holds under value.
func numericIndexCardinality(tb testing.TB, e *Engine, name string, value float64) uint64 {
	tb.Helper()
	sub, err := e.g.IndexManager().GetIndex(name)
	if err != nil {
		tb.Fatalf("GetIndex %q: %v", name, err)
	}
	idx, ok := sub.(*indexbtree.Index[float64])
	if !ok {
		tb.Fatalf("index %q is %T, want *btree.Index[float64]", name, sub)
	}
	return idx.Cardinality(value)
}

// newEngineAfterRolledBackDDL builds a seeded engine, opens an explicit
// transaction, runs stmt inside it, runs ddl while that transaction is still
// OPEN, and only then rolls the transaction back.
//
// The whole sequence is on ONE goroutine and needs no synchronisation: the DDL
// runs to completion between the eager mutation and the rollback by program
// order, so the window under test cannot fail to open. That is the point — this
// defect needs no concurrency at all.
//
// An empty ddl builds the oracle: the same seed and the same rolled-back
// transaction with no index created, so its answers come from a label scan.
//
// The backfill is forced SERIAL so no arm depends on GOMAXPROCS. It is the same
// white-box toggle TestBackfillNodeHashIndex_SerialVsParallelIdentical uses, and
// the read view under test is shared by the serial and parallel phase-2 paths,
// so nothing about the property being asserted is specific to either.
func newEngineAfterRolledBackDDL(tb testing.TB, ddl, stmt string) *Engine {
	tb.Helper()
	e := NewEngine(backfillSnapshotSeed(tb))
	e.parallelBackfillEnabled = false
	ctx := context.Background()

	tx, err := e.BeginTx(ctx)
	if err != nil {
		tb.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.Exec(stmt, nil)
	if err != nil {
		tb.Fatalf("Exec %q: %v", stmt, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("drain %q: %v", stmt, err)
	}
	_ = res.Close()

	// The DDL runs to completion while the transaction is still open, so its
	// backfill observes the eager mutations in the live graph.
	if ddl != "" {
		dres, derr := e.Run(ctx, ddl, nil)
		if derr != nil {
			tb.Fatalf("%q: %v", ddl, derr)
		}
		for dres.Next() {
		}
		if err := dres.Err(); err != nil {
			tb.Fatalf("drain %q: %v", ddl, err)
		}
		_ = dres.Close()
	}

	if err := tx.Rollback(); err != nil {
		tb.Fatalf("Rollback: %v", err)
	}
	return e
}

// sameStringSet reports whether two sorted row columns are identical.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// summariseRows renders a row column for a failure message, bounded so a
// thousand-row arm does not bury the numbers that matter.
func summariseRows(rows []string) string {
	const maxShown = 8
	if len(rows) <= maxShown {
		return fmt.Sprint(rows)
	}
	return fmt.Sprintf("%v …(+%d more)", rows[:maxShown], len(rows)-maxShown)
}
