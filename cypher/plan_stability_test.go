package cypher_test

// plan_stability_test.go — T924: same canonical query yields identical plan
// twice (determinism + plan-cache consistency).
//
// Acceptance criteria:
//  1. Two compilations of the same query produce identical plan serialisation.
//  2. Race-clean.
//  3. goleak-clean (enforced by TestMain in testmain_test.go).
//
// The second Explain call may be served from the plan cache; equality of the
// two plan strings validates both plan determinism and cache round-trip fidelity.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newPlanStabilityEngine returns an engine with a small labelled graph
// suitable for exercising all canonical query shapes.
func newPlanStabilityEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for i := range 5 {
		q := fmt.Sprintf(`CREATE (:Person {name: 'p%d', age: %d})`, i, 20+i*5)
		runSetup(t, eng, q)
	}
	// Add one edge for the varlen case.
	runSetup(t, eng, `MATCH (a:Person {name:'p0'}), (b:Person {name:'p1'}) CREATE (a)-[:KNOWS]->(b)`)
	return eng
}

// planStabilityQueries lists the queries whose plan must be stable across two
// consecutive Explain calls.
var planStabilityQueries = []string{
	`MATCH (n) RETURN n`,
	`MATCH (n:Person) RETURN n`,
	`MATCH (n:Person) WHERE n.name = 'p0' RETURN n`,
	`MATCH (a)-[*1..3]->(b) RETURN a, b`,
	`MATCH (n:Person) RETURN n.age, count(*)`,
}

// TestPlanStability_IdenticalTwice verifies that calling Explain twice on the
// same query returns the same plan string.
func TestPlanStability_IdenticalTwice(t *testing.T) {
	t.Parallel()

	eng := newPlanStabilityEngine(t)

	for _, q := range planStabilityQueries {
		q := q // capture
		t.Run(queryLabel(q), func(t *testing.T) {
			t.Parallel()

			plan1, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("Explain (1st): %v", err)
			}
			plan2, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("Explain (2nd): %v", err)
			}

			if plan1 != plan2 {
				t.Errorf("plan not stable across two calls:\n--- first ---\n%s\n--- second ---\n%s",
					plan1, plan2)
			}
			if plan1 == "" {
				t.Fatal("Explain returned empty plan")
			}
			if !strings.Contains(plan1, "ProduceResults") {
				t.Errorf("plan missing ProduceResults:\n%s", plan1)
			}
		})
	}
}

// TestPlanStability_RaceClean runs Explain concurrently on the same query to
// satisfy the race-clean acceptance criterion and verify cache correctness
// under contention.
func TestPlanStability_RaceClean(t *testing.T) {
	t.Parallel()

	eng := newPlanStabilityEngine(t)

	const workers = 8
	done := make(chan struct{}, workers)
	type result struct {
		plan string
		err  error
	}
	results := make(chan result, workers)

	q := planStabilityQueries[0] // `MATCH (n) RETURN n`
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			p, err := eng.Explain(q, nil)
			results <- result{plan: p, err: err}
		}()
	}

	for range workers {
		<-done
	}
	close(results)

	var first string
	for r := range results {
		if r.err != nil {
			t.Errorf("concurrent Explain: %v", r.err)
			continue
		}
		if first == "" {
			first = r.plan
			continue
		}
		if r.plan != first {
			t.Errorf("plan differs under concurrency:\n--- first ---\n%s\n--- other ---\n%s",
				first, r.plan)
		}
	}
}

// TestPlanStability_AllQueriesNonEmpty verifies that Explain returns a
// non-empty plan for all canonical queries (guards against silent failures).
func TestPlanStability_AllQueriesNonEmpty(t *testing.T) {
	t.Parallel()

	eng := newPlanStabilityEngine(t)

	for _, q := range planStabilityQueries {
		q := q
		t.Run(queryLabel(q), func(t *testing.T) {
			t.Parallel()
			plan, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if plan == "" {
				t.Error("Explain returned empty plan")
			}
		})
	}
}

// queryLabel returns a short, safe test name derived from a Cypher query string.
// Replaces spaces and special characters with underscores and truncates to 64
// characters so subtests have readable names.
func queryLabel(q string) string {
	const maxLen = 64
	r := make([]byte, 0, len(q))
	for i := range len(q) {
		c := q[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			r = append(r, c)
		default:
			if len(r) > 0 && r[len(r)-1] != '_' {
				r = append(r, '_')
			}
		}
	}
	// Trim trailing underscore.
	for len(r) > 0 && r[len(r)-1] == '_' {
		r = r[:len(r)-1]
	}
	if len(r) > maxLen {
		r = r[:maxLen]
	}
	return string(r)
}
