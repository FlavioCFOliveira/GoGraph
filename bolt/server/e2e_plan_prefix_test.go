package server_test

// e2e_plan_prefix_test.go — end-to-end gate for the Bolt `plan` / `profile`
// SUCCESS metadata (rmp #2721).
//
// The assertions are made through the OFFICIAL driver's own ResultSummary, which
// is the only way to prove the wire contract end to end: the key names, the
// value types, the nesting of `children`, and the placement on the TERMINAL
// SUCCESS all have to be right for ResultSummary.Plan()/Profile() to be
// populated at all. A server-side unit test on the metadata map would pass with
// the field on the wrong message.
//
// Every positive assertion is paired with a NEGATIVE control — the same query
// with no prefix must report a nil plan — so a test that passed because the
// driver reports something for every query would be visible.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_PlanPrefix_ExplainPopulatesPlan is the headline case: the gap the
// driver-compat inventory recorded was that ResultSummary.Plan() stayed nil
// because no statement-level prefix existed.
func TestE2E_PlanPrefix_ExplainPopulatesPlan(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	if _, err := session.Run(ctx, `CREATE (:Plan {n: 1}), (:Plan {n: 2})`, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := session.Run(ctx, `EXPLAIN MATCH (n:Plan) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Run(EXPLAIN): %v", err)
	}
	// An EXPLAIN executes nothing, so it must stream no records.
	var rows int
	for res.Next(ctx) {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if rows != 0 {
		t.Errorf("EXPLAIN streamed %d records, want 0", rows)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	plan := summary.Plan()
	if plan == nil {
		t.Fatal("ResultSummary.Plan() is nil after EXPLAIN")
	}
	if plan.Operator() == "" {
		t.Error("plan root has an empty operatorType")
	}
	if len(plan.Children()) == 0 {
		t.Errorf("plan root %q has no children; the tree did not survive the wire", plan.Operator())
	}
	// The access path must be reachable from the root, which is what proves the
	// nested `children` encoding round-tripped rather than only the top node.
	if !planContains(plan, "NodeByLabelScan") {
		t.Errorf("plan does not name the access path:\n%s", renderDriverPlan(plan, ""))
	}
	// An EXPLAIN reports estimates, never measurements, so `profile` must be
	// absent from the same SUCCESS.
	if summary.Profile() != nil {
		t.Error("ResultSummary.Profile() is non-nil after EXPLAIN")
	}

	// ── Control: the same query with no prefix must report no plan. ──────────
	ctrl, err := session.Run(ctx, `MATCH (n:Plan) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Run(control): %v", err)
	}
	ctrlSummary, err := ctrl.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume(control): %v", err)
	}
	if ctrlSummary.Plan() != nil {
		t.Error("an un-prefixed statement reported a plan; the assertion above proves nothing")
	}
	if ctrlSummary.Profile() != nil {
		t.Error("an un-prefixed statement reported a profile")
	}
}

// TestE2E_PlanPrefix_ProfilePopulatesProfileAndReturnsRows holds the other half
// of the pair: PROFILE executes, so the client gets the query's rows AND the
// measured plan.
func TestE2E_PlanPrefix_ProfilePopulatesProfileAndReturnsRows(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	if _, err := session.Run(ctx, `CREATE (:Prof {n: 1}), (:Prof {n: 2}), (:Prof {n: 3})`, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := session.Run(ctx, `PROFILE MATCH (n:Prof) RETURN n.n AS n`, nil)
	if err != nil {
		t.Fatalf("Run(PROFILE): %v", err)
	}
	var rows int
	for res.Next(ctx) {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if rows != 3 {
		t.Errorf("PROFILE streamed %d records, want 3 — PROFILE must execute the query", rows)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	prof := summary.Profile()
	if prof == nil {
		t.Fatal("ResultSummary.Profile() is nil after PROFILE")
	}
	if summary.Plan() != nil {
		t.Error("ResultSummary.Plan() is non-nil after PROFILE; the two must not both be sent")
	}
	if got := prof.Records(); got != int64(rows) {
		t.Errorf("profile root Records() = %d, want %d", got, rows)
	}
	if len(prof.Children()) == 0 {
		t.Fatalf("profile root %q has no children", prof.Operator())
	}
	// The db-hits must reach the client as an integer, from the operator that
	// actually reads records. Summing the tree avoids depending on which operator
	// the planner chose.
	if total := profileDbHits(prof); total <= 0 {
		t.Errorf("profile reports %d db-hits in total; the measurements did not reach the driver", total)
	}
}

// TestE2E_PlanPrefix_ProfileSurvivesDiscard covers the DISCARD terminal SUCCESS.
// A client that consumes a result without reading every record still ends the
// stream with DISCARD, and the driver builds its summary from whichever message
// ends it — so the profile has to be on both.
func TestE2E_PlanPrefix_ProfileSurvivesDiscard(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	// FetchSize 1 guarantees the first PULL reports has_more, so Consume ends the
	// stream with a DISCARD rather than with the PULL that already exhausted it.
	session := driver.NewSession(ctx, neo4j.SessionConfig{FetchSize: 1})
	defer session.Close(ctx) //nolint:errcheck

	if _, err := session.Run(ctx, `CREATE (:Disc {n: 1}), (:Disc {n: 2}), (:Disc {n: 3}), (:Disc {n: 4})`, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := session.Run(ctx, `PROFILE MATCH (n:Disc) RETURN n.n AS n`, nil)
	if err != nil {
		t.Fatalf("Run(PROFILE): %v", err)
	}
	// Read exactly one record, then abandon the rest.
	if !res.Next(ctx) {
		t.Fatalf("no first record: %v", res.Err())
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if summary.Profile() == nil {
		t.Fatal("ResultSummary.Profile() is nil after a DISCARD-terminated PROFILE")
	}
}

// TestE2E_PlanPrefix_ExplainOfWriteChangesNothing is the safety case over the
// wire: RunAny routes a writing statement to the write path, so this is the one
// that proves the prefix diverts before a transaction is opened. Its control arm
// runs the same statement without the prefix and requires the graph to move.
func TestE2E_PlanPrefix_ExplainOfWriteChangesNothing(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	count := func() int64 {
		t.Helper()
		res, err := session.Run(ctx, `MATCH (n:Wr) RETURN count(n) AS c`, nil)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		rec, err := res.Single(ctx)
		if err != nil {
			t.Fatalf("count single: %v", err)
		}
		v, _ := rec.Get("c")
		n, ok := v.(int64)
		if !ok {
			t.Fatalf("count returned %T, want int64", v)
		}
		return n
	}

	before := count()

	res, err := session.Run(ctx, `EXPLAIN CREATE (:Wr {n: 1})`, nil)
	if err != nil {
		t.Fatalf("Run(EXPLAIN CREATE): %v", err)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if summary.Plan() == nil {
		t.Error("ResultSummary.Plan() is nil after EXPLAIN of a write")
	}
	if summary.Counters().ContainsUpdates() {
		t.Error("EXPLAIN of a write reported updates")
	}
	if after := count(); after != before {
		t.Errorf("EXPLAIN CREATE created %d node(s)", after-before)
	}

	// ── Control: without the prefix the same statement MUST create a node. ───
	if _, err := session.Run(ctx, `CREATE (:Wr {n: 1})`, nil); err != nil {
		t.Fatalf("Run(control CREATE): %v", err)
	}
	if after := count(); after != before+1 {
		t.Fatalf("control CREATE moved the count from %d to %d; the assertion above proves nothing",
			before, after)
	}
}

// planContains reports whether op names any operator in the plan tree.
func planContains(p neo4j.Plan, op string) bool {
	if p.Operator() == op {
		return true
	}
	for _, c := range p.Children() {
		if planContains(c, op) {
			return true
		}
	}
	return false
}

// renderDriverPlan renders a driver-side plan tree for a failure message.
func renderDriverPlan(p neo4j.Plan, indent string) string {
	out := indent + p.Operator() + "\n"
	for _, c := range p.Children() {
		out += renderDriverPlan(c, indent+"  ")
	}
	return out
}

// profileDbHits sums the db-hits reported across a driver-side profile tree.
func profileDbHits(p neo4j.ProfiledPlan) int64 {
	total := p.DbHits()
	for _, c := range p.Children() {
		total += profileDbHits(c)
	}
	return total
}
