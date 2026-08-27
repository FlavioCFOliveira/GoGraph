package audit352_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestLogPlans records the physical plan of every shape this harness
// measures. Attribution of a benchmark delta to an operator is only valid
// if the operator is actually in the plan, so the plans are captured in the
// same session and the same binary as the numbers.
func TestLogPlans(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	log := func(name, q string) {
		p, err := engine.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", q, err)
		}
		t.Logf("--- %s\n%s\n%s", name, q, p)
	}
	for _, pct := range selectivityPcts {
		log("sweep_largeint", shipQuery(pct, "p.salary"))
	}
	for _, s := range projectionShapes {
		log("shape_"+s.name, s.query)
	}
}

// TestProfileShapes captures the engine's own per-operator PROFILE output
// for the headline shapes. This is instrumented attribution (it has its own
// observer overhead) and is used only to form hypotheses that a pprof
// profile then confirms independently.
func TestProfileShapes(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	for _, q := range []string{
		`MATCH (p:Person) RETURN count(*) AS c`,
		`MATCH (p:Person) RETURN p.salary`,
		`MATCH (p:Person) RETURN p`,
		`MATCH (p:Person) RETURN p.firstName, p.age, p.salary, p.bucket`,
		`MATCH (p:Person) WHERE p.bucket < 5 RETURN p.salary`,
		`MATCH (p:Person) WHERE p.bucket < 100 RETURN p.salary`,
		`MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN count(*) AS c`,
		`MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN b.salary`,
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT 10`,
		`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`,
	} {
		s, err := engine.Profile(t.Context(), q, nil)
		if err != nil {
			t.Fatalf("Profile(%q): %v", q, err)
		}
		t.Logf("--- %s\n%s", q, s)
	}
}
