package cypher_test

// aggregation_advanced_test.go — eager_aggregation with count/sum/avg/min/max/collect
// over a labelled Employee graph (T733).
//
// The existing aggregation_test.go covers generic unlabelled nodes with "num"
// and "group" properties. This file uses the :Employee label with "salary" and
// "name" properties so there is zero overlap with the existing suite.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newEmployeeGraph creates an engine with 5 Employee nodes whose salaries are
// 50000, 60000, 70000, 80000, 90000 and names are emp0…emp4.
func newEmployeeGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	salaries := []int64{50000, 60000, 70000, 80000, 90000}
	for i, sal := range salaries {
		q := buildEmployeeCreate(i, sal)
		runSetup(t, eng, q)
	}
	return eng
}

// buildEmployeeCreate returns a CREATE statement for a single Employee node.
func buildEmployeeCreate(idx int, salary int64) string {
	return "CREATE (:Employee {name: 'emp" + itoa(idx) + "', salary: " + itoa64(salary) + "})"
}

// itoa converts a non-negative int to its decimal string representation.
// Avoids importing strconv inside the test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// itoa64 converts a non-negative int64 to its decimal string representation.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// TestAggregationAdvanced_AllAggregates runs a single query that exercises
// count(*), sum, avg, min, max, and collect together on the Employee graph.
//
// Expected values for salaries [50000, 60000, 70000, 80000, 90000]:
//   - count(*) = 5
//   - sum      = 350000
//   - avg      = 70000.0
//   - min      = 50000
//   - max      = 90000
//   - collect  = 5 elements (order unspecified)
func TestAggregationAdvanced_AllAggregates(t *testing.T) {
	t.Parallel()
	eng := newEmployeeGraph(t)

	const q = `
		MATCH (n:Employee)
		RETURN
		  count(*)          AS c,
		  sum(n.salary)     AS s,
		  avg(n.salary)     AS a,
		  min(n.salary)     AS lo,
		  max(n.salary)     AS hi,
		  collect(n.name)   AS names
	`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", len(rows))
	}
	row := rows[0]

	mustInt(t, "count(*)", row["c"], 5)
	mustInt(t, "sum(salary)", row["s"], 350000)
	mustFloat(t, "avg(salary)", row["a"], 70000.0)
	mustInt(t, "min(salary)", row["lo"], 50000)
	mustInt(t, "max(salary)", row["hi"], 90000)

	names, ok := row["names"].(expr.ListValue)
	if !ok {
		t.Fatalf("collect(n.name): expected ListValue, got %T (%v)", row["names"], row["names"])
	}
	if len(names) != 5 {
		t.Errorf("collect(n.name) length = %d, want 5", len(names))
	}
}

// TestAggregationAdvanced_GroupBySalaryBucket tests GROUP BY on a computed
// expression by using a property that acts as a natural bucket.
//
// Graph: 3 "junior" employees (salary < 70000) and 2 "senior" (salary ≥ 70000).
// Query groups by "tier" (a string property) and checks per-group sums.
func TestAggregationAdvanced_GroupBySalaryBucket(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// tier='junior': 50000 + 60000 + 65000 = 175000
	// tier='senior': 80000 + 90000 = 170000
	for _, q := range []string{
		`CREATE (:Worker {name: 'w1', tier: 'junior', sal: 50000})`,
		`CREATE (:Worker {name: 'w2', tier: 'junior', sal: 60000})`,
		`CREATE (:Worker {name: 'w3', tier: 'junior', sal: 65000})`,
		`CREATE (:Worker {name: 'w4', tier: 'senior', sal: 80000})`,
		`CREATE (:Worker {name: 'w5', tier: 'senior', sal: 90000})`,
	} {
		runSetup(t, eng, q)
	}

	res, err := eng.Run(context.Background(), `
		MATCH (n:Worker)
		RETURN n.tier AS tier, sum(n.sal) AS total, count(*) AS cnt
	`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 2 {
		t.Fatalf("expected 2 tier groups, got %d: %v", len(rows), rows)
	}

	sums := map[string]int64{}
	counts := map[string]int64{}
	for _, row := range rows {
		tier, ok := row["tier"].(expr.StringValue)
		if !ok {
			t.Fatalf("tier: expected StringValue, got %T (%v)", row["tier"], row["tier"])
		}
		total, ok2 := row["total"].(expr.IntegerValue)
		if !ok2 {
			t.Fatalf("total: expected IntegerValue, got %T (%v)", row["total"], row["total"])
		}
		cnt, ok3 := row["cnt"].(expr.IntegerValue)
		if !ok3 {
			t.Fatalf("cnt: expected IntegerValue, got %T (%v)", row["cnt"], row["cnt"])
		}
		sums[string(tier)] = int64(total)
		counts[string(tier)] = int64(cnt)
	}

	if sums["junior"] != 175000 {
		t.Errorf("sum(junior) = %d, want 175000", sums["junior"])
	}
	if sums["senior"] != 170000 {
		t.Errorf("sum(senior) = %d, want 170000", sums["senior"])
	}
	if counts["junior"] != 3 {
		t.Errorf("count(junior) = %d, want 3", counts["junior"])
	}
	if counts["senior"] != 2 {
		t.Errorf("count(senior) = %d, want 2", counts["senior"])
	}
}

// TestAggregationAdvanced_EmptyLabelScan verifies that querying an empty label
// returns one neutral row (same guarantee as TestAggregation_EmptyInputNeutralRow
// but for a labelled scan, exercising the label scan → aggregate path).
func TestAggregationAdvanced_EmptyLabelScan(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Employee) RETURN count(*) AS c, sum(n.salary) AS s`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 neutral row, got %d", len(rows))
	}
	mustInt(t, "count(*) empty", rows[0]["c"], 0)
}
