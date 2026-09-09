package cypher

// Regression battery for rmp #2814: a property-index access path must never be
// chosen when the enclosing write transaction has already mutated the graph on
// the coordinate that index is bound to.
//
// # The defect, as measured at 5b7e930 (the shipped v0.14.0 behaviour)
//
// The graph is mutated EAGERLY — every statement reads through
// lpg.Graph.WriterViewOf, which sees its own writes — while the property indexes
// are written ONLY at the transaction boundary (exec.IndexBuffer.Commit →
// index.Manager.ApplyBatch). The label half of the planner is immune, because
// label reads go through the MVCC overlay in graph/lpg/mvcc_index.go; the
// property half has no overlay at all. And the equality rewrite SUBSUMES the
// Selection it replaces, keeping only a label residual, so nothing downstream
// re-checks the value. With a hash index on (:L, s) over 512 nodes:
//
//	case                                                    seek   scan
//	uncommitted SET moves s from "v7" to "zzz", seek "v7"      1      0
//	  → a row NO NODE CARRIES ANY MORE (a fabricated row)
//	same, seek the new key "zzz"                               0      1
//	  → the row is LOST
//	one autocommit statement, no BEGIN:
//	  CREATE (:L {s:'qqq'}) WITH count(*) AS n
//	  MATCH (m:L {s:'qqq'}) RETURN count(m)                    0      1
//	the same question either side of a commit         before 0 / after 1   1, 1
//
// The last line is what classifies it: post-commit the index is right, so this is
// a VISIBILITY defect, not index corruption.
//
// # What every test here is built to do
//
// Each case carries a CONTROL ARM that reaches the same answer through a plain
// scan, written as `m.s + '' = …` so the equality rewrite cannot claim it. The
// control is not decoration: without it a failing seek arm is indistinguishable
// from a broken fixture, and a passing one from a query shape that never wrote
// anything. Every control failure is a t.Fatalf, because it invalidates the
// measurement rather than reporting a defect.
//
// Plan choice is asserted from [buildOpts]-level builds, never from Explain
// inside a write transaction: cypher/plan_prefix.go plans against a pinned view
// and cypher/exectx.go passes nil for a statement inside an open write
// transaction, so Explain there plans against a fresh COMMITTED snapshot and can
// name an access path other than the one that ran.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// idxUncommittedEngine builds a 512-node (:L) population carrying a string
// property s = "v<i>", a second string property other = "o<i>", and a numeric
// property p = i, with a hash index on (:L, s) and on (:L, p).
//
// 512 nodes rather than a handful because every index access path in this engine
// is population-gated: the key-set seek and the range seek both refuse a label
// smaller than rangeSeekMinLabelPopulation, so a toy fixture would decline the
// seek for the wrong reason and every test here would pass vacuously.
func idxUncommittedEngine(t *testing.T, ddl ...string) (*Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 512; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(id, "L"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(id, "s", lpg.StringValue(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("SetNodeProperty s: %v", err)
		}
		if err := g.SetNodeProperty(id, "other", lpg.StringValue(fmt.Sprintf("o%d", i))); err != nil {
			t.Fatalf("SetNodeProperty other: %v", err)
		}
		if err := g.SetNodeProperty(id, "p", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty p: %v", err)
		}
	}
	eng := NewEngine(g)
	if len(ddl) == 0 {
		ddl = []string{
			"CREATE INDEX l_s_hash FOR (n:L) ON (n.s)",
			"CREATE INDEX l_p_hash FOR (n:L) ON (n.p)",
		}
	}
	for _, d := range ddl {
		if _, err := eng.Run(context.Background(), d, nil); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
	return eng, g
}

// idxUncommittedScalar drains a one-row, one-column result into an int64.
func idxUncommittedScalar(t *testing.T, q string, next func() bool, rec func() exec.Record, errf func() error) int64 {
	t.Helper()
	var got int64 = -1
	rows := 0
	for next() {
		rows++
		for k, v := range rec() {
			switch iv := v.(type) {
			case int64:
				got = iv
			case int:
				got = int64(iv)
			case float64:
				got = int64(iv)
			case expr.IntegerValue:
				got = int64(iv)
			default:
				t.Fatalf("query %q: column %q has unhandled type %T (%v)", q, k, v, v)
			}
		}
	}
	if err := errf(); err != nil {
		t.Fatalf("iterate %q: %v", q, err)
	}
	if rows != 1 {
		t.Fatalf("query %q: got %d rows, want exactly 1", q, rows)
	}
	if got < 0 {
		t.Fatalf("query %q: extracted no count", q)
	}
	return got
}

// idxUncommittedTxCount runs q inside the open transaction tx and returns its
// single scalar. ExecAny, not Exec: the statement may write.
func idxUncommittedTxCount(t *testing.T, tx *ExplicitTx, q string) int64 {
	t.Helper()
	res, err := tx.ExecAny(q, nil)
	if err != nil {
		t.Fatalf("ExecAny %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	return idxUncommittedScalar(t, q, res.Next, res.Record, res.Err)
}

// idxUncommittedExec runs a write statement inside tx and drains it.
func idxUncommittedExec(t *testing.T, tx *ExplicitTx, q string) {
	t.Helper()
	res, err := tx.ExecAny(q, nil)
	if err != nil {
		t.Fatalf("ExecAny %q: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate %q: %v", q, err)
	}
	_ = res.Close()
}

// idxUncommittedAutocommit runs q on the autocommit WRITE path. Engine.Run is
// read-only; RunAny is the write path — getting that wrong costs a run.
func idxUncommittedAutocommit(t *testing.T, eng *Engine, q string) int64 {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunAny %q: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	return idxUncommittedScalar(t, q, res.Next, res.Record, res.Err)
}

// TestIndexSeekUncommittedWrite_LostRow pins the LOST-ROW direction: a node
// created in an uncommitted transaction must be reachable through a seek on the
// key it carries.
//
// It asks the same question on both sides of the commit, which is what classifies
// the defect rather than merely detecting it: at 5b7e930 the answer was 0 before
// the commit and 1 after it, so the index was being maintained correctly and only
// its VISIBILITY to the writing transaction was wrong. An index-maintenance
// defect would have shown 0 on both sides, and the fix for that would be a
// different fix.
func TestIndexSeekUncommittedWrite_LostRow(t *testing.T) {
	ctx := context.Background()
	eng, _ := idxUncommittedEngine(t)

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	idxUncommittedExec(t, tx, `CREATE (:L {s: 'zzz', other: 'ozzz', p: 999999})`)

	preSeek := idxUncommittedTxCount(t, tx, `MATCH (m:L {s: 'zzz'}) RETURN count(m) AS c`)
	preScan := idxUncommittedTxCount(t, tx, `MATCH (m:L) WHERE m.s + '' = 'zzz' RETURN count(m) AS c`)

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx2, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx 2: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	postSeek := idxUncommittedTxCount(t, tx2, `MATCH (m:L {s: 'zzz'}) RETURN count(m) AS c`)
	postScan := idxUncommittedTxCount(t, tx2, `MATCH (m:L) WHERE m.s + '' = 'zzz' RETURN count(m) AS c`)

	// The control. A scan that cannot see the node on both sides means the fixture
	// or the query shape is broken, not the index.
	if preScan != 1 || postScan != 1 {
		t.Fatalf("CONTROL FAILED: the scan arm must see the node on both sides of the commit, got pre=%d post=%d", preScan, postScan)
	}
	if postSeek != 1 {
		t.Fatalf("CONTROL FAILED: the seek misses the node even AFTER the commit (post=%d) — that is an index-MAINTENANCE defect, which this test is not measuring", postSeek)
	}
	if preSeek != 1 {
		t.Errorf("LOST ROW (rmp #2814): a node created in this uncommitted transaction is invisible to the seek — seek=%d, scan=%d, and after the commit both say %d",
			preSeek, preScan, postSeek)
	}
}

// TestIndexSeekUncommittedWrite_FabricatedRow pins the harder direction: after an
// uncommitted SET moves an indexed value, a seek for the OLD key must return
// nothing, because no node carries it any more.
//
// This is the direction the range and intersection seeks cannot exhibit — they
// retain the original predicate as a residual Filter — and the direction the
// equality seek can, because it SUBSUMES the Selection and keeps only the label
// residual (api.go tryBuildIndexSeekFromSelection). At 5b7e930 it returned 1.
//
// The write is located through a SCAN (`m.s + ” = 'v7'`), so the seek under test
// cannot be blamed for the write not happening.
func TestIndexSeekUncommittedWrite_FabricatedRow(t *testing.T) {
	ctx := context.Background()
	eng, _ := idxUncommittedEngine(t)

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	idxUncommittedExec(t, tx, `MATCH (m:L) WHERE m.s + '' = 'v7' SET m.s = 'zzz'`)

	oldSeek := idxUncommittedTxCount(t, tx, `MATCH (m:L {s: 'v7'}) RETURN count(m) AS c`)
	oldScan := idxUncommittedTxCount(t, tx, `MATCH (m:L) WHERE m.s + '' = 'v7' RETURN count(m) AS c`)
	newSeek := idxUncommittedTxCount(t, tx, `MATCH (m:L {s: 'zzz'}) RETURN count(m) AS c`)
	newScan := idxUncommittedTxCount(t, tx, `MATCH (m:L) WHERE m.s + '' = 'zzz' RETURN count(m) AS c`)

	// The control: the scan arm must show the value actually moved in this tx.
	if oldScan != 0 || newScan != 1 {
		t.Fatalf("CONTROL FAILED: the scan arm must show the value moved from 'v7' to 'zzz', got oldScan=%d newScan=%d", oldScan, newScan)
	}
	if oldSeek != 0 {
		t.Errorf("FABRICATED ROW (rmp #2814): the seek returns %d row(s) for the OLD key 'v7' that no node carries any more (scan=%d)", oldSeek, oldScan)
	}
	if newSeek != 1 {
		t.Errorf("LOST ROW (rmp #2814): the seek returns %d for the NEW key 'zzz' the scan finds (scan=%d)", newSeek, newScan)
	}
}

// TestIndexSeekUncommittedWrite_SingleAutocommitStatement pins the case that
// needs no BEGIN at all: ONE statement writes and then reads its own write back
// through a seek.
//
// It is the case a buffer-only guard cannot reach, and the reason
// [pendingIndexDelta.addPlanWrites] exists: the plan is chosen before the
// statement's first row flows, so the change buffer is empty at that moment, and
// the autocommit index drain happens later still (Result.commitUnderBarrier runs
// after materialize). The WITH is a pipeline breaker, so the CREATE has fully
// drained by the time the MATCH's access path is exercised.
func TestIndexSeekUncommittedWrite_SingleAutocommitStatement(t *testing.T) {
	eng, _ := idxUncommittedEngine(t)

	const seekQ = `CREATE (:L {s: 'qqq'}) WITH count(*) AS n MATCH (m:L {s: 'qqq'}) RETURN count(m) AS c`
	const scanQ = `CREATE (:L {s: 'www'}) WITH count(*) AS n MATCH (m:L) WHERE m.s + '' = 'www' RETURN count(m) AS c`

	seekSaw := idxUncommittedAutocommit(t, eng, seekQ)
	scanSaw := idxUncommittedAutocommit(t, eng, scanQ)

	// The control: the scan arm must read its own write back, or the query shape —
	// not the index — is what is wrong.
	if scanSaw != 1 {
		t.Fatalf("CONTROL FAILED: the scan arm did not read its own write back (%d); the query shape, not the index, is at fault", scanSaw)
	}
	if seekSaw != 1 {
		t.Errorf("LOST ROW in ONE AUTOCOMMIT STATEMENT (rmp #2814): the seek arm reads %d where the scan arm reads %d, with no BEGIN anywhere", seekSaw, scanSaw)
	}
}

// TestIndexSeekUncommittedWrite_OtherAccessPaths covers the three remaining
// property-index access paths on the same defect, so the guard is pinned on all
// of them and not only on the single-key equality seek.
//
// # Why each arm is shaped the way it is
//
// Every one of these paths declines when the STALE index answers with zero
// postings — "an empty result is correct but pointless to seek", the rule stated
// in [buildSeekSetOperator] and applied by the range seek's selectivity gate — so
// the obvious shape (write a brand-new key and then seek it) makes them fall back
// to a scan for a reason that has nothing to do with rmp #2814, and the arm
// becomes one that CANNOT FAIL. Measured: with the guard fully reverted,
// `SET m.p = 999999` followed by `MATCH (m:L {p: 999999})` PASSED, because the
// range seek declined the empty range on its own.
//
// So each arm below writes into a key space the stale index ALREADY populates.
// The seek then returns a non-empty but WRONG candidate set — everything the
// index knew, minus what this transaction has since added — and the residual
// Filter each of these three paths retains cannot repair that, because a residual
// only ever removes candidates and can never supply a missing one. They therefore
// exhibit the LOST-ROW direction only; being immune to the FABRICATED-ROW
// direction is the residual's whole purpose, and that direction is asserted where
// it belongs, on the equality seek
// ([TestIndexSeekUncommittedWrite_FabricatedRow]).
//
// The access path each arm reaches is named beside it, read from EXPLAIN on the
// READ path outside any transaction — which is where EXPLAIN is trustworthy — and
// re-checked at run time, so an arm that stops reaching its path fails instead of
// going quietly green.
func TestIndexSeekUncommittedWrite_OtherAccessPaths(t *testing.T) {
	ctx := context.Background()
	arms := []struct {
		name string
		// ddl overrides the fixture's default indexes.
		ddl []string
		// write is the uncommitted statement run first, inside the transaction.
		write string
		// seekQ takes the guarded access path; scanQ is the control, written so no
		// rewrite can claim it.
		seekQ, scanQ string
		// want is the answer BOTH arms must give.
		want int64
		// wantLeaf is the access-path operator seekQ must reach.
		wantLeaf string
		path     string
	}{
		{
			name: "numeric equality via the btree companion",
			ddl:  []string{"CREATE INDEX l_p_hash FOR (n:L) ON (n.p)"},
			// n7 joins n8 on the key 8, which the stale index already holds.
			write:    `MATCH (m:L) WHERE m.p + 0 = 7 SET m.p = 8`,
			seekQ:    `MATCH (m:L {p: 8}) RETURN count(m) AS c`,
			scanQ:    `MATCH (m:L) WHERE m.p + 0 = 8 RETURN count(m) AS c`,
			want:     2,
			wantLeaf: "NodeByIndexRangeScan",
			path:     "Filter over NodeByIndexRangeScan [range=8..8]",
		},
		{
			name:     "key-set seek over a disjunction of equalities",
			ddl:      []string{"CREATE INDEX l_s_hash FOR (n:L) ON (n.s)"},
			write:    `CREATE (:L {s: 'v1'})`,
			seekQ:    `MATCH (m:L) WHERE m.s = 'v1' OR m.s = 'v2' RETURN count(m) AS c`,
			scanQ:    `MATCH (m:L) WHERE m.s + '' = 'v1' OR m.s + '' = 'v2' RETURN count(m) AS c`,
			want:     3,
			wantLeaf: "NodeByIndexSeekSet",
			path:     "NodeByIndexSeekSet",
		},
		{
			name: "string btree range seek",
			ddl: []string{
				"CREATE INDEX l_s_hash FOR (n:L) ON (n.s)",
				"CREATE INDEX l_s_btree FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}",
			},
			// [v10, v11) already holds v10 and v100..v109 — eleven nodes, inside the
			// 10% selectivity ceiling on a 512-node label — and this adds a twelfth.
			write:    `CREATE (:L {s: 'v105'})`,
			seekQ:    `MATCH (m:L) WHERE m.s >= 'v10' AND m.s < 'v11' RETURN count(m) AS c`,
			scanQ:    `MATCH (m:L) WHERE m.s + '' >= 'v10' AND m.s + '' < 'v11' RETURN count(m) AS c`,
			want:     12,
			wantLeaf: "NodeByIndexRangeScan",
			path:     `Filter over NodeByIndexRangeScan [range="v10".."v11"(excl)]`,
		},
		{
			name: "STARTS WITH prefix rewrite",
			ddl: []string{
				"CREATE INDEX l_s_hash FOR (n:L) ON (n.s)",
				"CREATE INDEX l_s_btree FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}",
			},
			write:    `CREATE (:L {s: 'v205'})`,
			seekQ:    `MATCH (m:L) WHERE m.s STARTS WITH 'v20' RETURN count(m) AS c`,
			scanQ:    `MATCH (m:L) WHERE (m.s + '') STARTS WITH 'v20' RETURN count(m) AS c`,
			want:     12,
			wantLeaf: "NodeByIndexRangeScan",
			path:     `Filter over NodeByIndexRangeScan [range="v20".."v21"(excl)]`,
		},
	}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			eng, _ := idxUncommittedEngine(t, arm.ddl...)

			// The access-path control, on the READ path outside any transaction: if the
			// shape no longer reaches the path this arm exists to guard, the arm proves
			// nothing about it and must be reshaped rather than left green.
			plan, err := eng.Explain(arm.seekQ, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if !strings.Contains(plan, arm.wantLeaf) {
				t.Fatalf("CONTROL FAILED: %q no longer reaches %s on the read path — want %s, got:\n%s",
					arm.seekQ, arm.wantLeaf, arm.path, plan)
			}

			tx, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			idxUncommittedExec(t, tx, arm.write)

			seek := idxUncommittedTxCount(t, tx, arm.seekQ)
			scan := idxUncommittedTxCount(t, tx, arm.scanQ)

			// The control: the scan arm must observe the uncommitted write, or the
			// fixture, and not the index, is what is wrong.
			if scan != arm.want {
				t.Fatalf("CONTROL FAILED: the scan arm reads %d, want %d — the fixture or the query shape is wrong, not the index", scan, arm.want)
			}
			if seek != scan {
				t.Errorf("LOST ROW via %s (rmp #2814): the index access path reads %d where the scan reads %d",
					arm.path, seek, scan)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The decline's PRECISION, asserted at the build, not from a query answer
// ─────────────────────────────────────────────────────────────────────────────
//
// The behavioural tests above cannot tell a per-coordinate decline from a blanket
// one: both give the right answer. Without the tests below, "fix the defect by
// never seeking on a write path" would pass everything and silently retire the
// access paths rmp #2225 measured at 178× on the bulk-load idiom.

// idxUncommittedSelection builds the exact IR shape the equality rewrite claims:
// Selection[n.<prop> = '<value>'] over NodeByLabelScan[label]. Built by hand
// rather than translated, so the test pins the REWRITE and not the translator's
// current lowering of a property-map pattern.
func idxUncommittedSelection(label, nodeVar, prop, value string) *ir.Selection {
	return &ir.Selection{
		Child: &ir.NodeByLabelScan{NodeVar: nodeVar, Label: label},
		PredicateExpr: &ast.BinaryOp{
			Operator: "=",
			Left:     &ast.Property{Receiver: &ast.Variable{Name: nodeVar}, Key: prop},
			Right:    &ast.StringLiteral{Value: value},
		},
	}
}

// TestPendingIndexDelta_DeclineIsKeyedPerCoordinate proves the decline is keyed
// on the (label, property) actually dirtied: a pending write on (:L, other) must
// leave the equality seek on (:L, s) in place, and a pending write on (:L, s)
// must take it away.
func TestPendingIndexDelta_DeclineIsKeyedPerCoordinate(t *testing.T) {
	eng, _ := idxUncommittedEngine(t)
	idxMgr := eng.g.IndexManager()
	labelSrc := &lpgLabelResolver{g: eng.g.ReadAt(nil)}
	var params map[string]expr.Value
	sel := idxUncommittedSelection("L", "n", "s", "v7")

	fires := func(d *pendingIndexDelta) bool {
		t.Helper()
		op, ok, err := tryBuildIndexSeekFromSelection(sel, params, make(map[string]int), idxMgr,
			labelSrc, &buildOpts{indexSeekEnabled: true, pendingIdx: d})
		if err != nil {
			t.Fatalf("tryBuildIndexSeekFromSelection: %v", err)
		}
		if ok && op != nil {
			_ = op.Close()
		}
		return ok
	}

	// The control: with nothing pending the seek must fire, or every "declined"
	// result below proves nothing.
	if !fires(nil) {
		t.Fatalf("CONTROL FAILED: the equality seek does not fire on (:L, s) with nothing pending, so this test cannot observe a decline")
	}

	cases := []struct {
		name    string
		delta   *pendingIndexDelta
		wantOff bool
	}{
		{"a write on the SAME property", &pendingIndexDelta{props: map[string]struct{}{"s": {}}}, true},
		{"a write on a DIFFERENT property", &pendingIndexDelta{props: map[string]struct{}{"other": {}}}, false},
		{"a write on the SAME label", &pendingIndexDelta{labels: map[string]struct{}{"L": {}}}, true},
		{"a write on a DIFFERENT label", &pendingIndexDelta{labels: map[string]struct{}{"Other": {}}}, false},
		{"an undecodable write (all)", &pendingIndexDelta{all: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fires(tc.delta); got == tc.wantOff {
				verb := "DECLINED"
				if got {
					verb = "still FIRED"
				}
				t.Errorf("with %s pending, the equality seek on (:L, s) %s (fires=%v, wantFires=%v)", tc.name, verb, got, !tc.wantOff)
			}
		})
	}
}

// TestPendingIndexDelta_FromBuffer pins what the buffer half derives, including
// the two things it must NOT derive: an edge change dirties no node coordinate,
// and an empty buffer yields the nil "nothing pending" state that keeps the read
// path free.
func TestPendingIndexDelta_FromBuffer(t *testing.T) {
	_, g := idxUncommittedEngine(t)
	sID := uint32(g.PropertyKeys().Intern("s"))
	lID := uint32(g.Registry().Intern("L"))

	if d := newPendingIndexDelta(nil, g); d != nil {
		t.Errorf("a nil buffer must yield nil, got %+v", d)
	}
	var emptyBuf exec.IndexBuffer
	if d := newPendingIndexDelta(&emptyBuf, g); d != nil {
		t.Errorf("an empty buffer must yield nil, got %+v", d)
	}

	tests := []struct {
		name             string
		change           index.Change
		blocksLS, blocks bool // (:L, s) and (:Other, other)
	}{
		{"a node-property set", index.Change{Op: index.OpSetNodeProperty, Property: sID}, true, false},
		{"a node-property delete", index.Change{Op: index.OpDelNodeProperty, Property: sID}, true, false},
		{"a node-label add", index.Change{Op: index.OpAddNodeLabel, Label: lID}, true, false},
		{"a node-label remove", index.Change{Op: index.OpRemoveNodeLabel, Label: lID}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf exec.IndexBuffer
			buf.Enqueue(tc.change)
			d := newPendingIndexDelta(&buf, g)
			if d == nil {
				t.Fatalf("%s must produce a delta, got nil", tc.name)
			}
			if got := d.blocksNodeIndex("L", "s"); got != tc.blocksLS {
				t.Errorf("%s: blocksNodeIndex(L, s) = %v, want %v", tc.name, got, tc.blocksLS)
			}
			if got := d.blocksNodeIndex("Other", "other"); got != tc.blocks {
				t.Errorf("%s: blocksNodeIndex(Other, other) = %v, want %v", tc.name, got, tc.blocks)
			}
		})
	}

	// An edge change must dirty nothing: every access path guarded by this type
	// seeks NODES through a node-bound index, which ignores edge changes. This is
	// what keeps the #2225 bulk-load idiom on its seeks.
	var edgeBuf exec.IndexBuffer
	edgeBuf.Enqueue(index.Change{Op: index.OpSetEdgeProperty, Property: sID})
	edgeBuf.Enqueue(index.Change{Op: index.OpAddEdgeLabel, Label: lID})
	if d := newPendingIndexDelta(&edgeBuf, g); d != nil {
		t.Errorf("edge-only changes must yield nil (nothing pending), got %+v", d)
	}
}

// TestPendingIndexDelta_EveryWriteIRNodeIsClassified is the drift gate on
// [pendingIndexDelta.addPlanWrites].
//
// That walker treats any IR node type it does not name as non-writing and
// recurses through it, which is the right default for the ~50 read and plumbing
// nodes — and exactly the wrong one for a write node added later. So this test
// enumerates every write node type buildOperatorWrite handles and requires the
// walker to classify each one: either as a precise coordinate, or as `all`.
// A new write node type that nobody teaches the walker about fails here rather
// than silently reinstating rmp #2814.
func TestPendingIndexDelta_EveryWriteIRNodeIsClassified(t *testing.T) {
	// One entry per case of buildOperatorWrite's switch. wantAll marks the nodes
	// whose coordinates are not statically decodable; wantProps / wantLabels the
	// ones that are; and both empty with wantAll false marks a write that touches
	// no node coordinate at all (relationship-only), which must be justified in
	// the note beside it.
	tests := []struct {
		name       string
		node       ir.LogicalPlan
		wantAll    bool
		wantProps  []string
		wantLabels []string
		note       string
	}{
		{name: "CreateNode", node: &ir.CreateNode{Labels: []string{"L"}, Properties: "{s: 'x'}"}, wantAll: true},
		{name: "SetAllProperties", node: &ir.SetAllProperties{EntityVar: "n"}, wantAll: true},
		{name: "DeleteNode", node: &ir.DeleteNode{}, wantAll: true},
		{name: "DetachDelete", node: &ir.DetachDelete{NodeVar: "n"}, wantAll: true},
		{name: "Merge", node: &ir.Merge{}, wantAll: true},
		{name: "MergePattern", node: &ir.MergePattern{}, wantAll: true},

		{name: "SetProperty", node: &ir.SetProperty{EntityVar: "n", PropertyKey: "s"}, wantProps: []string{"s"}},
		{name: "RemoveProperty", node: &ir.RemoveProperty{EntityVar: "n", PropertyKey: "s"}, wantProps: []string{"s"}},
		{name: "SetLabels", node: &ir.SetLabels{NodeVar: "n", Labels: []string{"L", "M"}}, wantLabels: []string{"L", "M"}},
		{name: "RemoveLabels", node: &ir.RemoveLabels{NodeVar: "n", Labels: []string{"L"}}, wantLabels: []string{"L"}},

		{
			name: "CreateRelationship", node: &ir.CreateRelationship{StartVar: "a", EndVar: "b", RelType: "R"},
			note: "endpoints are already bound; it writes only edge state",
		},
		{
			name: "DeleteRelationship", node: &ir.DeleteRelationship{RelVar: "r"},
			note: "removes an edge; no node label or property changes",
		},
		{
			name: "MergeRelationship", node: &ir.MergeRelationship{SrcVar: "a", DstVar: "b", RelType: "R"},
			note: "SrcVar/DstVar must already be bound and every property it writes is a RELATIONSHIP property (cypher/exec/merge_relationship.go touches no node state)",
		},

		{
			// Foreach carries its body in Inner, which Children() exposes, so the
			// classification it needs is the recursion's — proved here by wrapping a
			// node write the walker decodes precisely.
			name: "Foreach", node: &ir.Foreach{Inner: &ir.SetProperty{EntityVar: "n", PropertyKey: "deep"}},
			wantProps: []string{"deep"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d pendingIndexDelta
			d.addPlanWrites(tc.node)
			if d.all != tc.wantAll {
				t.Errorf("all = %v, want %v (%s)", d.all, tc.wantAll, tc.note)
			}
			if tc.wantAll {
				return // props/labels are not consulted once all is set
			}
			// Read the walker's OWN output, not blocksNodeIndex: this test is about
			// what addPlanWrites classifies, and routing it through the predicate
			// would make it fail for a mutation of the predicate instead.
			for _, p := range tc.wantProps {
				if _, ok := d.props[p]; !ok {
					t.Errorf("property %q was not recorded; recorded props=%v", p, d.props)
				}
			}
			for _, l := range tc.wantLabels {
				if _, ok := d.labels[l]; !ok {
					t.Errorf("label %q was not recorded; recorded labels=%v", l, d.labels)
				}
			}
			if len(tc.wantProps) == 0 && len(d.props) != 0 {
				t.Errorf("recorded unexpected properties %v (%s)", d.props, tc.note)
			}
			if len(tc.wantLabels) == 0 && len(d.labels) != 0 {
				t.Errorf("recorded unexpected labels %v (%s)", d.labels, tc.note)
			}
		})
	}
}

// TestPendingIndexDelta_DisableIndexSeekOption pins the kill switch the equality
// rewrite shipped without. Both arms must return the SAME rows: the option
// changes the access path, never the answer.
func TestPendingIndexDelta_DisableIndexSeekOption(t *testing.T) {
	ctx := context.Background()
	run := func(disable bool) int64 {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		for i := 0; i < 512; i++ {
			id := fmt.Sprintf("n%d", i)
			if err := g.AddNode(id); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeLabel(id, "L"); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
			if err := g.SetNodeProperty(id, "s", lpg.StringValue(fmt.Sprintf("v%d", i))); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
		eng := NewEngineWithOptions(g, EngineOptions{DisableIndexSeek: disable})
		if _, err := eng.Run(ctx, "CREATE INDEX l_s_hash FOR (n:L) ON (n.s)", nil); err != nil {
			t.Fatalf("CREATE INDEX: %v", err)
		}
		res, err := eng.Run(ctx, `MATCH (m:L {s: 'v7'}) RETURN count(m) AS c`, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer func() { _ = res.Close() }()
		return idxUncommittedScalar(t, "seek arm", res.Next, res.Record, res.Err)
	}
	on, off := run(false), run(true)
	if on != 1 || off != 1 {
		t.Errorf("DisableIndexSeek changed the ANSWER, not just the access path: enabled=%d disabled=%d, want 1 and 1", on, off)
	}
}
