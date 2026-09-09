package cypher_test

// profile_dbhits_countstore_test.go — the end-to-end db-hits gate on the two
// count-store leaves (rmp #2777).
//
// Both answer a group-by-less count from a maintained counter, and both have a
// fallback behind that counter which reads records. rmp #2760 classified them
// together as UNKNOWN for that reason; rmp #2773 narrowed LabelCountScan's
// fallback and reopened the question. It was settled by MEASURING which arm
// answers on the shapes a real query plans, and the two leaves came out
// DIFFERENTLY:
//
//   - AllNodesCountScan's fallback is INSIDE the operator — Init walks
//     WalkNodeIDs itself — so the operator can count it, and does. Its cell is a
//     measured 0 on the counter path and a real count on the walk.
//   - LabelCountScan's is BEHIND THE RESOLVER: lpg.Graph.LabelCountAsOf resolves
//     the filtered bitmap when the churn concerns the counted label, and
//     ResolveLabelCountAsOf reports only (count, ok), so the operator cannot tell
//     which arm answered. Its cell stays "?".
//
// What makes this a gate rather than a restatement is the FALLBACK ARM. The
// reachability was measured, not assumed: ReadView.LiveNodeCountExact declines
// whenever a node-life record is invisible to the reader's snapshot, so an
// ordinary `MATCH (n) RETURN count(*)` takes the walk while ANY other transaction
// holds an uncommitted create — which is what countStoreOpenTx constructs here.
//
// Race-clean; short layer. Nothing here spawns a goroutine of its own.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countStoreNodes is the population both leaves count. It is deliberately not a
// round power of two, so a figure that happened to be a capacity or a chunk size
// could not be mistaken for the count.
const countStoreNodes = 37

// newCountStoreEngine builds countStoreNodes :N nodes and drains the MVCC
// history, so the counter paths are genuinely reachable before any arm below
// deliberately makes them decline.
func newCountStoreEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < countStoreNodes; i++ {
		id := fmt.Sprintf("cs%d", i)
		if err := g.AddNode(id); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", id, err)
		}
	}
	g.ReclaimNow()
	return cypher.NewEngine(g)
}

// countStoreOpenTx opens an explicit write transaction, creates one labelled node
// inside it, and leaves it UNCOMMITTED for the rest of the subtest. That is the
// state in which a snapshot reader cannot be given the present-time live count,
// so the whole-graph count pushdown must walk instead.
//
// label is the label the uncommitted node carries, and it matters: a write under
// the label being counted also makes lpg.Graph.LabelCountAsOf take its bitmap
// arm, while a write elsewhere leaves it on the raw counter.
func countStoreOpenTx(t *testing.T, eng *cypher.Engine, label string) {
	t.Helper()
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	r, err := tx.Exec("CREATE (x:"+label+" {v:1})", nil)
	if err != nil {
		t.Fatalf("uncommitted CREATE: %v", err)
	}
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("uncommitted CREATE drain: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("uncommitted CREATE close: %v", err)
	}
}

// countStoreCell profiles q and returns the named operator's rows and db-hits
// cell. An absent operator is a HARD failure: the arm would otherwise assert
// something about a plan it never built.
func countStoreCell(t *testing.T, eng *cypher.Engine, q, operator string) (rows, hits int64, known bool, plan string) {
	t.Helper()
	plan, err := eng.Profile(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Profile(%q): %v", q, err)
	}
	r, d, k, found := profiledCells(plan, operator)
	if !found {
		t.Fatalf("the plan for %q holds no %s, so this arm exercises nothing:\n%s",
			q, operator, plan)
	}
	return r, d, k, plan
}

// TestProfileDbHits_CountStoreLeaves is rmp #2777's end-to-end acceptance gate:
// the classification asserted through the public PROFILE surface, on planned
// queries, in both of AllNodesCountScan's directions.
func TestProfileDbHits_CountStoreLeaves(t *testing.T) {
	t.Parallel()
	const (
		allNodes = "MATCH (n) RETURN count(*) AS c"
		labelled = "MATCH (n:N) RETURN count(n) AS c"
	)

	// ── AllNodesCountScan, counter path: a MEASURED ZERO ─────────────────────
	t.Run("AllNodesCountScan_counterPathIsAMeasuredZero", func(t *testing.T) {
		t.Parallel()
		eng := newCountStoreEngine(t)
		rows, hits, known, plan := countStoreCell(t, eng, allNodes, "AllNodesCountScan")
		if !known {
			t.Fatalf("the O(1) count pushdown rendered db-hits %q. It reads one maintained "+
				"counter and no records, which is a figure — and it is 0:\n%s",
				exec.DbHitsUnknown, plan)
		}
		if hits != 0 {
			t.Errorf("the O(1) count pushdown reported dbhits=%d, want 0 — it walks nothing:\n%s",
				hits, plan)
		}
		// Non-vacuity: a zero measured over an operator that answered nothing, or
		// answered wrongly, would prove nothing at all.
		if rows != 1 {
			t.Fatalf("the count leaf emitted %d rows, want 1:\n%s", rows, plan)
		}
		if got := countStoreScalar(t, eng, allNodes); got != countStoreNodes {
			t.Fatalf("the query returned %d, want %d — the zero describes the wrong "+
				"answer", got, countStoreNodes)
		}
	})

	// ── AllNodesCountScan, fallback: a REAL COUNT ────────────────────────────
	//
	// The direction the acceptance criterion names, and the one a type-level
	// noStorageAccess marker would have got wrong.
	t.Run("AllNodesCountScan_fallbackCountsItsWalk", func(t *testing.T) {
		t.Parallel()
		eng := newCountStoreEngine(t)

		// The control comes FIRST, on the same engine, so the difference below is
		// attributable to the uncommitted transaction and to nothing else.
		_, quiet, quietKnown, quietPlan := countStoreCell(t, eng, allNodes, "AllNodesCountScan")
		if !quietKnown || quiet != 0 {
			t.Fatalf("control: the drained substrate reported dbhits=%d (known=%v), want a "+
				"known 0; without that baseline the arm below compares against nothing:\n%s",
				quiet, quietKnown, quietPlan)
		}

		countStoreOpenTx(t, eng, "N")

		rows, hits, known, plan := countStoreCell(t, eng, allNodes, "AllNodesCountScan")
		if !known {
			t.Fatalf("the fallback rendered db-hits %q; the walk counts its own node ids:\n%s",
				exec.DbHitsUnknown, plan)
		}
		if hits != countStoreNodes {
			t.Errorf("the fallback reported dbhits=%d for a walk of %d live nodes. The charge "+
				"is one node reference per node id WalkNodeIDs yielded, which is what the "+
				"equivalent AllNodesScan reports for the same walk:\n%s",
				hits, countStoreNodes, plan)
		}
		if rows != 1 {
			t.Fatalf("the count leaf emitted %d rows, want 1:\n%s", rows, plan)
		}
		// The two directions must genuinely differ, or a flat figure passes both.
		if hits == quiet {
			t.Errorf("the counter path and the fallback both reported dbhits=%d, so this gate "+
				"no longer distinguishes them:\n%s", hits, plan)
		}
		// The independent oracle: the same whole-graph walk done by the DERIVED
		// scan, which charges one node reference per emitted row. The count leaf's
		// fallback reads the same node ids, so the two may not disagree.
		_, scanHits, scanKnown, scanPlan := countStoreCell(t, eng,
			"MATCH (n) RETURN n", "AllNodesScan")
		if !scanKnown || scanHits != hits {
			t.Errorf("AllNodesScan reported dbhits=%d (known=%v) for the same whole-graph walk "+
				"the count leaf's fallback reported %d for:\n%s",
				scanHits, scanKnown, hits, scanPlan)
		}
		// And the answer is still the snapshot's, so the walk was not a licence to
		// read the uncommitted node.
		if got := countStoreScalar(t, eng, allNodes); got != countStoreNodes {
			t.Errorf("the fallback returned %d, want %d — the uncommitted node must not be "+
				"visible", got, countStoreNodes)
		}
	})

	// ── LabelCountScan stays "?" ─────────────────────────────────────────────
	//
	// Re-decided on the post-#2773 code and recorded here so the question is not
	// reopened a third time. The cell is "?" in BOTH substrate states, which is the
	// point: the two states differ in whether lpg.Graph.LabelCountAsOf materialises
	// the filtered bitmap, and the operator cannot tell them apart — so no figure it
	// could publish would be true in both.
	t.Run("LabelCountScan_staysUnknown", func(t *testing.T) {
		t.Parallel()
		for _, arm := range []struct {
			name  string
			setup func(*testing.T, *cypher.Engine)
		}{
			{"drained substrate", func(*testing.T, *cypher.Engine) {}},
			{"uncommitted write on ANOTHER label", func(t *testing.T, e *cypher.Engine) {
				countStoreOpenTx(t, e, "ZZOther")
			}},
			{"uncommitted write on the COUNTED label", func(t *testing.T, e *cypher.Engine) {
				countStoreOpenTx(t, e, "N")
			}},
		} {
			t.Run(arm.name, func(t *testing.T) {
				t.Parallel()
				eng := newCountStoreEngine(t)
				arm.setup(t, eng)
				rows, hits, known, plan := countStoreCell(t, eng, labelled, "LabelCountScan")
				if known {
					t.Errorf("LabelCountScan reported dbhits=%d on a %s. It answers through "+
						"ResolveLabelCountAsOf, which reports only (count, ok) — so the operator "+
						"cannot know whether lpg.Graph.LabelCountAsOf read the raw counter or "+
						"resolved the filtered bitmap, and any figure it published would be false "+
						"in one of those states. Re-decided in rmp #2777; if this is being changed "+
						"a third time, the resolver has to report the arm first:\n%s",
						hits, arm.name, plan)
				}
				if rows != 1 {
					t.Fatalf("LabelCountScan emitted %d rows, want 1 — the arm did not run the "+
						"query it names:\n%s", rows, plan)
				}
				if got := countStoreScalar(t, eng, labelled); got != countStoreNodes {
					t.Errorf("the labelled count returned %d, want %d", got, countStoreNodes)
				}
			})
		}
	})
}

// countStoreScalar runs q and returns its single integer column, which is the
// oracle that every db-hits assertion above describes a query that actually
// answered correctly.
func countStoreScalar(t *testing.T, eng *cypher.Engine, q string) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("Run(%q) produced no row (err=%v)", q, res.Err())
	}
	v := res.ValueAt(0)
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		t.Fatalf("Run(%q) column 0 is %T, want an integer", q, v)
	}
	return int64(iv)
}
