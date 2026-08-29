package audit352_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// selectivityPcts are the sweep points. `bucket` is i%100, so `bucket < K`
// ships exactly K% of the 120 000 scanned nodes while the scan beneath the
// filter always touches all of them. Rows SHIPPED is therefore the only
// variable; rows PRODUCED is constant.
var selectivityPcts = []int{0, 5, 25, 50, 75, 100}

// shipQuery builds the sweep query for a given projected property.
func shipQuery(pct int, projection string) string {
	return fmt.Sprintf(`MATCH (p:Person) WHERE p.bucket < %d RETURN %s`, pct, projection)
}

// TestSweepPreconditions is the harness's own guard. It runs on every
// `go test` of this package (no -bench needed) and fails if either
// invariant the sweep depends on is violated:
//
//	(1) all arms compile to the identical physical plan, and
//	(2) each arm ships exactly the row count its selectivity implies.
//
// Without (1) the sweep would be comparing two different programs; without
// (2) the regression of time on rows would be fitted against a fictional x.
func TestSweepPreconditions(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	for _, projection := range []string{"p.salary", "p.age", "p.firstName"} {
		projection := projection
		t.Run(projection, func(t *testing.T) {
			queries := make([]string, 0, len(selectivityPcts))
			for _, pct := range selectivityPcts {
				queries = append(queries, shipQuery(pct, projection))
			}
			assertSamePlan(t, engine, queries)
			for i, pct := range selectivityPcts {
				want := nodeCount * pct / 100
				if got := countRows(t, engine, queries[i]); got != want {
					t.Fatalf("selectivity %d%%: shipped %d rows, want %d", pct, got, want)
				}
			}
		})
	}
}

// BenchmarkShipSweep_LargeInt sweeps rows shipped while projecting a
// large integer (>=100000). Integer values at or above 256 fall outside the
// runtime's staticuint64s table, so boxing one into an interface allocates.
func BenchmarkShipSweep_LargeInt(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, pct := range selectivityPcts {
		q := shipQuery(pct, "p.salary")
		b.Run(fmt.Sprintf("pct=%03d", pct), func(b *testing.B) { runQuery(b, engine, q) })
	}
}

// BenchmarkShipSweep_SmallInt is BenchmarkShipSweep_LargeInt with the only
// difference being the magnitude of the projected integer (18..82, inside
// staticuint64s). The delta against the LargeInt sweep at the same
// selectivity is the cost of the boxing allocation alone.
func BenchmarkShipSweep_SmallInt(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, pct := range selectivityPcts {
		q := shipQuery(pct, "p.age")
		b.Run(fmt.Sprintf("pct=%03d", pct), func(b *testing.B) { runQuery(b, engine, q) })
	}
}

// BenchmarkShipSweep_String sweeps the same shape projecting a string
// property, the shape that cannot avoid carrying a pointer per row.
func BenchmarkShipSweep_String(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, pct := range selectivityPcts {
		q := shipQuery(pct, "p.firstName")
		b.Run(fmt.Sprintf("pct=%03d", pct), func(b *testing.B) { runQuery(b, engine, q) })
	}
}

// projectionShapes are the matched pair set for PRODUCING vs SHIPPING.
// Every one of them scans all 120 000 :Person nodes; they differ only in
// what leaves the engine.
var projectionShapes = []struct {
	name  string
	query string
}{
	// Produces 120k rows internally, ships 1. The aggregating twin.
	{"count_star", `MATCH (p:Person) RETURN count(*) AS c`},
	{"count_prop", `MATCH (p:Person) RETURN count(p.salary) AS c`},
	// Ships 120k rows, one column.
	{"one_smallint", `MATCH (p:Person) RETURN p.age`},
	{"one_largeint", `MATCH (p:Person) RETURN p.salary`},
	{"one_string", `MATCH (p:Person) RETURN p.firstName`},
	// Ships 120k rows, four columns.
	{"four_cols", `MATCH (p:Person) RETURN p.firstName, p.age, p.salary, p.bucket`},
	// Ships 120k whole nodes — the row-based Project path.
	{"whole_node", `MATCH (p:Person) RETURN p`},
}

// BenchmarkProjectionShape measures each shape in projectionShapes. The
// difference between an aggregating arm and a shipping arm over the same
// scan separates the cost of PRODUCING a row from the cost of SHIPPING it.
func BenchmarkProjectionShape(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range projectionShapes {
		s := s
		b.Run(s.name, func(b *testing.B) { runQuery(b, engine, s.query) })
	}
}

// TestProjectionShapeRowCounts records what each shape actually ships, so
// the benchmark table can never be read as if two shapes moved the same
// number of rows when they did not.
func TestProjectionShapeRowCounts(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range projectionShapes {
		t.Logf("%-14s ships %d rows", s.name, countRows(t, engine, s.query))
	}
}

// paginationShapes probe the cost of the ordinary pagination idiom. Every arm
// orders the same 120 000-row scan; they differ only in the page they ask for.
//
// wantOps is the ordering/pagination operator sequence the arm MUST compile to.
// Before rmp #2509 the SKIP arms planned Limit→Skip→Sort — a full sort of the
// whole scan for a ten-row page — and this table's predecessor recorded that
// with t.Logf and asserted nothing, so it could not have failed however the
// planner behaved. The expectation is now declared, because a benchmark table
// read against a plan that has silently changed is worse than no table.
var paginationShapes = []struct {
	name    string
	query   string
	wantOps []string
}{
	{"limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT 10`,
		[]string{"Top"}},
	{"skip0_limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`,
		[]string{"Skip", "Top"}},
	{"skip100_limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 100 LIMIT 10`,
		[]string{"Skip", "Top"}},
	{"skip10000_limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 10000 LIMIT 10`,
		[]string{"Skip", "Top"}},
	{"limit110_noskip", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT 110`,
		[]string{"Top"}},
	// Deep pagination: the fused bound (100 010) is most of the 120 000-row
	// input, the regime in which the bounded operator has the least to gain and
	// the most transient buffer to pay for. It is here so the trade-off is
	// measured rather than assumed.
	{"skip100000_limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 100000 LIMIT 10`,
		[]string{"Skip", "Top"}},
	{"unlimited_sort", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary`,
		[]string{"Sort"}},
}

func BenchmarkPagination(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range paginationShapes {
		s := s
		b.Run(s.name, func(b *testing.B) { runQuery(b, engine, s.query) })
	}
}

// orderingPlanOps extracts, in plan order, the ordering and pagination operator
// names a rendered physical plan contains.
func orderingPlanOps(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimLeft(line, " \u2502\u251c\u2514\u2500\t")
		name, _, _ := strings.Cut(trimmed, " ")
		switch name {
		case "Sort", "Top", "Limit", "Skip":
			out = append(out, name)
		}
	}
	return out
}

// TestPaginationPlans is this harness's own precondition guard, in the same
// spirit as TestSweepPreconditions: it fails if an arm stops compiling to the
// plan its benchmark number is attributed to. It runs on every `go test` of this
// package, with no -bench needed.
func TestPaginationPlans(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range paginationShapes {
		p, err := engine.Explain(s.query, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", s.query, err)
		}
		got := orderingPlanOps(p)
		if strings.Join(got, ",") != strings.Join(s.wantOps, ",") {
			t.Errorf("%s: ordering operators %v, want %v\n%s", s.name, got, s.wantOps, p)
			continue
		}
		t.Logf("%-18s ops=%v ships %d rows", s.name, got, countRows(t, engine, s.query))
	}
}

// expandShapes probe the 1-hop expand path: the same traversal, once
// shipping every row and once aggregating them away.
var expandShapes = []struct {
	name  string
	query string
}{
	{"expand_count", `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN count(*) AS c`},
	{"expand_ship_2col", `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.firstName, b.salary`},
	{"expand_ship_1col", `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN b.salary`},
	{"expand_untyped_count", `MATCH (a:Person)-->(b:Person) RETURN count(*) AS c`},
}

func BenchmarkExpandShape(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range expandShapes {
		s := s
		b.Run(s.name, func(b *testing.B) { runQuery(b, engine, s.query) })
	}
}

func TestExpandShapePlans(t *testing.T) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range expandShapes {
		p, err := engine.Explain(s.query, nil)
		if err != nil {
			t.Fatalf("Explain(%q): %v", s.query, err)
		}
		t.Logf("%s ships %d rows\n%s", s.name, countRows(t, engine, s.query), p)
	}
}
