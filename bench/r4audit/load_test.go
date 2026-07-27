//go:build r4audit

package r4audit

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

// TestEdgeLoadDecomposition isolates the statement the three-way harness uses to
// load edges, which took 35m10s for 199 941 edges at 20 000 nodes both before
// and after the sprint 316-325 remediation. The whole point is to find which
// term of that statement is quadratic, by measuring the same batch against a
// growing node population: a per-row cost flat in N acquits the statement, a
// per-row cost linear in N convicts it.
func TestEdgeLoadDecomposition(t *testing.T) {
	// Each variant is run with the SAME 500-row batch against populations that
	// differ only in N, so any growth is attributable to N.
	variants := []struct{ name, cypherStmt string }{
		{"two-seek-create", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) CREATE (a)-[:K]->(b)`},
		{"two-seek-noop", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) RETURN count(*)`},
		{"one-seek-noop", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN count(*)`},
		{"two-match-clauses", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) MATCH (b:P {sid: r.ts}) RETURN count(*)`},
		{"create-only", `UNWIND $rows AS r CREATE (:Q {v: r.ss})`},
	}
	sizes := []int{2000, 4000, 8000, 16000}
	const batch = 500

	fmt.Printf("%-20s", "variant")
	for _, n := range sizes {
		fmt.Printf("%14s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("   %s\n", "us/row@16k")

	for _, v := range variants {
		fmt.Printf("%-20s", v.name)
		var last time.Duration
		for _, n := range sizes {
			eng, rows := seedForLoad(t, n, batch)
			st := time.Now()
			res, err := eng.RunAny(context.Background(), v.cypherStmt, map[string]any{"rows": rows})
			if err != nil {
				fmt.Printf("  ERROR %v", err)
				break
			}
			for res.Next() {
			}
			if e := res.Err(); e != nil {
				fmt.Printf("  EVALERR %v", e)
				break
			}
			_ = res.Close()
			d := time.Since(st)
			fmt.Printf("%14s", d.Round(time.Microsecond))
			last = d
		}
		fmt.Printf("   %8.1f\n", float64(last.Microseconds())/float64(batch))
	}
}

// seedForLoad builds n :P nodes with an indexed string key `sid` and returns a
// batch of `batch` row maps naming existing keys, mirroring the harness.
func seedForLoad(tb testing.TB, n, batch int) (*cypher.Engine, []any) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			tb.Fatal(err)
		}
		if err := g.SetNodeProperty(key, "sid", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			tb.Fatal(err)
		}
	}
	eng := cypher.NewEngine(g)
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_sid FOR (x:P) ON (x.sid)`, nil); err != nil {
		tb.Fatalf("create index: %v", err)
	}
	rows := make([]any, 0, batch)
	for i := 0; i < batch; i++ {
		rows = append(rows, map[string]any{
			"ss": fmt.Sprintf("s%d", i%n),
			"ts": fmt.Sprintf("s%d", (i*7+1)%n),
		})
	}
	return eng, rows
}
