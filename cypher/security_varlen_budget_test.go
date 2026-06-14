package cypher_test

// security_varlen_budget_test.go — REGRESSION GUARD for SECURITY-GAP #1478:
// variable-length-path (VLE) expansion now carries a PER-QUERY aggregate edge
// budget (not merely a per-input-row one) plus a finite default hop ceiling for
// unbounded patterns.
//
// A bounded VLE pattern such as (a)-[:R*1..K]->(x) is limited to the paths
// reachable from ONE anchor row. When the same VLE is driven by M input rows (a
// Cartesian-style UNWIND, a multi-row MATCH, or a fan-out earlier in the
// pipeline), the aggregate expansion work used to scale as M × (per-row cost)
// with no per-QUERY ceiling, so an attacker who could inflate the driving
// cardinality multiplied the cost of an otherwise-bounded VLE.
//
// The fix adds an aggregate counter (exec.defaultMaxTotalEdgesTraversed,
// 100,000,000) that is not reset per input row, and a default hop ceiling
// (exec.defaultMaxUnboundedHops) for -[*]- patterns. The deterministic
// assertion that the aggregate budget trips ErrVarLenCapExceeded once the
// per-query total is exceeded lives next to the operator's own test harness, in
// cypher/exec/security_varlen_perquery_budget_test.go (a small injected cap
// proves the per-query semantics without performing 100M real traversals).
//
// This file fences the engine-level invariants that must NOT regress under the
// new caps: (1) bounded multi-row VLE still returns exactly M × per-row rows on
// a tiny graph (the aggregate cap is generous, no false positive); and (2) an
// unbounded -[*]- pattern on a tiny graph still terminates and returns the
// correct results (the default hop ceiling is far above any TCK-sized path and
// the no-repeated-edge rule still bounds the traversal).

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secCypherBuildChain creates a directed chain Hub{id:0}-[:R]->Hub{id:1}-> … ->Hub{id:K}
// on eng so that a VLE [:R*1..K] from Hub{id:0} reaches exactly K distinct nodes
// (one per hop length 1..K). It returns nothing; failures fail the test.
func secCypherBuildChain(t *testing.T, eng *cypher.Engine, k int) {
	t.Helper()
	secCypherDrain(t, eng, `CREATE (:Hub {id:0})`, nil)
	for i := 1; i <= k; i++ {
		secCypherDrain(t, eng,
			`MATCH (p:Hub {id:$pid}) CREATE (p)-[:R]->(:Hub {id:$cid})`,
			map[string]expr.Value{
				"pid": expr.IntegerValue(int64(i - 1)),
				"cid": expr.IntegerValue(int64(i)),
			})
	}
}

// secCypherCountRows runs a read query and returns the number of result rows,
// guarded by an upper bound so a runaway expansion fails fast rather than
// exhausting memory.
func secCypherCountRows(t *testing.T, eng *cypher.Engine, q string) int {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	n := 0
	for res.Next() {
		_ = res.Record()
		n++
		if n > 100_000 {
			t.Fatalf("Run(%q): produced >100k rows on a tiny graph — VLE expansion is unexpectedly large", q)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Run(%q) iter: %v", q, err)
	}
	return n
}

// TestSec_Cypher_VarLen_BoundedMultiRowStillSucceeds fences the #1478 fix
// against a false positive. On an 8-edge chain it measures the single-anchor VLE
// row count, then drives the SAME VLE with M = 1, 10, 100 input rows and asserts
// the total equals M × per-row. The aggregate per-query edge budget
// (100,000,000) is far above the handful of traversals this workload performs,
// so the bounded multi-row case must still complete with the full result — the
// new cap bounds only pathological aggregate work, never legitimate fan-out.
func TestSec_Cypher_VarLen_BoundedMultiRowStillSucceeds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	const k = 8 // chain length; VLE [:R*1..8] from the head reaches 8 nodes.
	secCypherBuildChain(t, eng, k)

	// Baseline: VLE from the single head anchor.
	perRow := secCypherCountRows(t, eng, `MATCH (a:Hub {id:0})-[:R*1..8]->(x) RETURN x`)
	if perRow != k {
		t.Fatalf("single-anchor VLE produced %d rows; want %d (one per hop length 1..%d)", perRow, k, k)
	}

	// Drive the SAME VLE anchor with M input rows via UNWIND. Total rows must
	// still be M × perRow — the per-query budget does not reject this bounded
	// workload.
	for _, m := range []int{1, 10, 100} {
		ms := strconv.Itoa(m)
		q := `UNWIND range(1,` + ms + `) AS s MATCH (a:Hub {id:0})-[:R*1..8]->(x) RETURN s, x`
		got := secCypherCountRows(t, eng, q)
		want := m * perRow
		if got != want {
			t.Fatalf("M=%d: total VLE rows = %d; want %d (= M × per-row %d) — the per-query budget (#1478) must not reject bounded multi-row VLE", m, got, want, perRow)
		}
	}
}

// TestSec_Cypher_VarLen_UnboundedStarTerminates fences the #1478 default hop
// ceiling against a regression in unbounded-pattern semantics. An unbounded
// -[:R*]- pattern on the same tiny chain must still terminate and return exactly
// the K reachable nodes: the no-repeated-edge rule bounds the BFS far below the
// finite default hop ceiling (65,536), so clamping math.MaxInt → that ceiling is
// invisible on any tiny graph (which is all openCypher TCK graphs are).
func TestSec_Cypher_VarLen_UnboundedStarTerminates(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	const k = 8
	secCypherBuildChain(t, eng, k)

	// Unbounded upper bound: -[:R*]- from the head reaches all K chain nodes,
	// one per distinct hop length 1..K (the chain has no cycles, so the
	// traversal terminates by exhausting reachable edges, not by the ceiling).
	got := secCypherCountRows(t, eng, `MATCH (a:Hub {id:0})-[:R*]->(x) RETURN x`)
	if got != k {
		t.Fatalf("unbounded -[:R*]- produced %d rows; want %d (one per hop length 1..%d) — the default hop ceiling must not alter tiny-graph results", got, k, k)
	}
}
