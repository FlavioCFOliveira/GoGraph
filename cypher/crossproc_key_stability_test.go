package cypher_test

// crossproc_key_stability_test.go — T898
//
// Two independent processes compiling the same Cypher query against the same
// logical schema must produce plan-cache keys (and therefore plans) that are
// byte-equal. This proves the planner's canonicalisation is
// process-independent and depends only on schema + query text + parameters.
//
// Architecture:
//   - Child (proc A): builds an empty Engine (trivial schema — Family 1),
//     calls engine.Explain for each canonical query, writes one line per query
//     to stdout in the form "<query>\t<explain>" and exits 0.
//   - Parent (proc B): builds its own Engine on the same schema, calls
//     engine.Explain locally, then parses proc A's output and compares the
//     results byte-for-byte.
//
// The plan cache key is the query string itself (see planCache.loadOrStore in
// plan_cache.go). Byte-equal Explain output proves that the IR translation —
// and therefore the plan cache hit/miss decision — is process-independent for
// these queries.
//
// Layer: short.  Race-clean (no shared state across process boundary).

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/subproc"
)

// canonicalQueries is the set of read queries exercised by T898.
// Family 1 (trivial schema) + Family 19 (Cypher-specific patterns).
//
// Write queries (CREATE, MERGE) are intentionally excluded: engine.Explain
// panics on write IR nodes that contain nil plan children (pre-existing
// limitation). The plan-cache key stability contract is satisfied by read
// queries, which cover all plan-building paths that affect cache key lookup.
var canonicalQueries = []string{
	// Family 1 — trivial graph patterns.
	`MATCH (n) RETURN n`,
	`MATCH (n:Person) RETURN n`,
	`MATCH (n:Person) WHERE n.name = "Alice" RETURN n`,
	`MATCH (a)-[r]->(b) RETURN a, r, b`,
	`MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a, b`,
	// Family 19 — Cypher-specific read patterns.
	`MATCH (n:Person) RETURN count(n)`,
	`MATCH (n:Person) RETURN n ORDER BY n.name`,
	`MATCH (n:Person) RETURN n LIMIT 10`,
	`MATCH (n:Person) RETURN n SKIP 5 LIMIT 10`,
	`MATCH (n:Person) WHERE n.age > 18 RETURN n`,
}

// sep is the field separator used between query text and explain output in the
// child's stdout. U+0001 (SOH) cannot appear in a valid Cypher query or a plan
// explanation, making it a safe delimiter.
const sep = "\x01"

func init() {
	// Register the child-process handler for T898. The mode name is unique
	// across the cypher package test binary so that subproc.Dispatch routes
	// to the correct handler when the binary is re-executed as a child.
	subproc.Register("cypher-plan-cache-key-child", func(_ []string) int {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		eng := cypher.NewEngine(g)
		for _, q := range canonicalQueries {
			explain, err := eng.Explain(q, nil)
			if err != nil {
				fmt.Printf("ERR%s%s%s%v\n", sep, q, sep, err)
				return 1
			}
			// Flatten newlines within the explain output so each record fits
			// on a single stdout line. The parent reverses the substitution.
			flat := strings.ReplaceAll(explain, "\n", "\\n")
			fmt.Printf("%s%s%s\n", q, sep, flat)
		}
		return 0
	})
}

// TestCrossProc_PlanCacheKeyStability spawns a child process (proc A) that
// compiles each canonical query against an empty engine and emits the Explain
// plan to stdout. The parent (proc B) independently compiles the same queries
// and compares the results byte-for-byte.
//
// A failure indicates that the planner's IR translation is non-deterministic
// across OS processes — which would break the plan-cache hit contract and any
// downstream system that relies on stable plan fingerprints.
func TestCrossProc_PlanCacheKeyStability(t *testing.T) {
	t.Parallel()

	// Spawn proc A.
	stdout, stderr, err := subproc.Run(t, "cypher-plan-cache-key-child")
	if err != nil {
		t.Fatalf("proc A failed: %v\nstderr: %s", err, stderr)
	}
	if len(stderr) > 0 {
		t.Logf("proc A stderr: %s", stderr)
	}

	// Parse proc A's output into a query→explain map.
	childPlans := make(map[string]string, len(canonicalQueries))
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, sep)
		if idx < 0 {
			t.Fatalf("malformed child output line (no separator): %q", line)
		}
		q := line[:idx]
		flat := line[idx+len(sep):]
		if strings.HasPrefix(q, "ERR") {
			t.Fatalf("child reported error: %s", flat)
		}
		childPlans[q] = strings.ReplaceAll(flat, "\\n", "\n")
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan child stdout: %v", err)
	}

	// Proc B: build a local engine with the same trivial schema and compare.
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for _, q := range canonicalQueries {
		childExplain, ok := childPlans[q]
		if !ok {
			t.Errorf("child did not emit plan for query: %q", q)
			continue
		}
		parentExplain, err := eng.Explain(q, nil)
		if err != nil {
			t.Errorf("parent Explain(%q): %v", q, err)
			continue
		}
		if childExplain != parentExplain {
			t.Errorf("plan mismatch for query %q:\n  child:  %q\n  parent: %q",
				q, childExplain, parentExplain)
		}
	}
	if len(childPlans) != len(canonicalQueries) {
		t.Errorf("child emitted %d plans, want %d", len(childPlans), len(canonicalQueries))
	}
}
