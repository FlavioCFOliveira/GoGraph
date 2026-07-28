//go:build r4audit

package r4audit

// inlj_test.go — rmp #2233 acceptance measurement for the index nested-loop join.
//
// The shape is the UNWIND-bound bulk-load join #2228 collapsed from 35m10s to
// 2.206s with a hash join, and whose decision record
// (docs/benchmarks/write-path-hash-join-2026-07-27.md §6) deferred the index
// nested-loop join because the two plans win in opposite regimes:
//
//	hash join              Θ(N+B)
//	index nested-loop join Θ(B·log N)
//
// So the acceptance criteria are two-sided. At SMALL batch against a LARGE
// population the seek must beat the hash join (AC1); at the harness's own LARGE
// batch the gate must still pick the hash join and the load must not regress
// (AC2). A single-direction measurement would show a win and hide a regression.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// inljSeed builds n :P nodes with an indexed numeric `key` property, through
// Cypher's own CREATE INDEX so the numeric btree companion the seek uses is the
// one production builds.
func inljSeed(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("n%d", i)
		if err := g.AddNode(name); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(name, "P"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		// Distinct keys, so a seek returns one row and the measurement is the SEEK
		// rather than a long posting-list drain.
		if err := g.SetNodeProperty(name, "key", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty(key): %v", err)
		}
		if err := g.SetNodeProperty(name, "id", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty(id): %v", err)
		}
	}
	eng := cypher.NewEngine(g)
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_key FOR (x:P) ON (x.key)`, nil); err != nil {
		tb.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// inljBatch returns the UNWIND parameter for a batch of b keys spread across the
// population, so the seeks touch the whole index rather than one hot subtree.
func inljBatch(b, n int) []any {
	rows := make([]any, 0, b)
	stride := 1
	if b > 0 {
		stride = n / b
		if stride < 1 {
			stride = 1
		}
	}
	for i := 0; i < b; i++ {
		rows = append(rows, map[string]any{"k": int64((i * stride) % n)})
	}
	return rows
}

// TestIndexNestedLoopJoinBench measures the read and write bound-key shapes at
// both batch regimes against two population sizes.
//
// The gate decides which plan each cell takes; that decision is asserted from the
// planner counter in cypher's own TestIndexNestedLoopJoin_CostGatePicksTheRightPlan
// (acceptance criterion 4 requires a counter, not Explain). Here the point is the
// wall clock the decision buys.
func TestIndexNestedLoopJoinBench(t *testing.T) {
	sizes := []int{20000, 80000}
	batches := []int{500, 5000}

	cases := []struct {
		name string
		q    string
		// write marks a statement that mutates, so each timed run needs a fresh
		// engine rather than the shared read-only one.
		write bool
	}{
		{
			name: "read: UNWIND-bound equi-join",
			q:    `UNWIND $rows AS r MATCH (b:P) WHERE b.key = r.k RETURN count(b)`,
		},
		{
			name:  "write: UNWIND-bound equi-join then SET",
			q:     `UNWIND $rows AS r MATCH (b:P) WHERE b.key = r.k SET b.touched = r.k`,
			write: true,
		},
	}

	fmt.Printf("%-42s %8s", "case", "B")
	for _, n := range sizes {
		fmt.Printf("%14s", fmt.Sprintf("N=%d", n))
	}
	fmt.Println()

	for _, c := range cases {
		for _, b := range batches {
			fmt.Printf("%-42s %8d", c.name, b)
			for _, n := range sizes {
				params := map[string]any{"rows": inljBatch(b, n)}
				best := time.Hour
				// Fewer repetitions for the write shape: each one needs a fresh graph,
				// and the seed dominates the harness otherwise.
				reps := 5
				if c.write {
					reps = 3
				}
				var eng *cypher.Engine
				if !c.write {
					eng = inljSeed(t, n)
					// Warm the plan cache so the timed runs measure execution.
					if _, err := eng.RunAny(context.Background(), c.q, params); err != nil {
						t.Fatalf("%s: %v", c.name, err)
					}
				}
				for k := 0; k < reps; k++ {
					if c.write {
						eng = inljSeed(t, n)
					}
					st := time.Now()
					res, err := eng.RunAny(context.Background(), c.q, params)
					if err != nil {
						t.Fatalf("%s: %v", c.name, err)
					}
					for res.Next() {
					}
					if err := res.Err(); err != nil {
						t.Fatalf("%s: %v", c.name, err)
					}
					_ = res.Close()
					if d := time.Since(st); d < best {
						best = d
					}
				}
				fmt.Printf("%14s", best.Round(time.Microsecond))
			}
			fmt.Println()
		}
	}
}

// TestIndexNestedLoopCrossover finds where Θ(B·log N) actually loses to Θ(N+B),
// as opposed to where the unit arithmetic says it should.
//
// #2228's decision record compared UNIT counts — build rows against tree levels —
// and concluded the hash join leads at B=5000, N=20000 (25 000 against 71 500).
// Measured, the seek is 6.7× FASTER there. The units are not comparable: a hash
// build row allocates and copies a whole row, while a btree level is a bounded
// search inside a cached node. So the crossover has to be measured, and this is
// the harness that measures it. Drive it with the gate forced each way.
func TestIndexNestedLoopCrossover(t *testing.T) {
	const n = 20000
	const q = `UNWIND $rows AS r MATCH (b:P) WHERE b.key = r.k RETURN count(b)`
	eng := inljSeed(t, n)

	fmt.Printf("%10s %10s %14s\n", "B", "B/N", "time")
	for _, b := range []int{500, 5000, 20000, 50000, 200000, 500000} {
		params := map[string]any{"rows": inljBatch(b, n)}
		if _, err := eng.RunAny(context.Background(), q, params); err != nil {
			t.Fatalf("B=%d: %v", b, err)
		}
		best := time.Hour
		for k := 0; k < 3; k++ {
			st := time.Now()
			res, err := eng.RunAny(context.Background(), q, params)
			if err != nil {
				t.Fatalf("B=%d: %v", b, err)
			}
			for res.Next() {
			}
			_ = res.Close()
			if d := time.Since(st); d < best {
				best = d
			}
		}
		fmt.Printf("%10d %10.2f %14s\n", b, float64(b)/float64(n), best.Round(time.Microsecond))
	}
}
