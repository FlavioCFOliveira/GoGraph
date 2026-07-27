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

// TestRelCreateRootCause narrows why creating a relationship between two matched
// nodes costs O(N) in the node population. Each row of the matrix changes ONE
// thing, so the term that disappears identifies the cause.
func TestRelCreateRootCause(t *testing.T) {
	const batch = 500
	sizes := []int{2000, 4000, 8000, 16000}

	cases := []struct {
		name    string
		indexed bool
		stmt    string
	}{
		{"rel-create/indexed", true, `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) CREATE (a)-[:K]->(b)`},
		{"rel-create/NO-index", false, `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) CREATE (a)-[:K]->(b)`},
		{"match-only/indexed", true, `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) RETURN count(*)`},
		{"node-create/indexed", true, `UNWIND $rows AS r CREATE (:P {sid: r.ss})`},
		{"rel-create-anon/idx", true, `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`},
	}

	fmt.Printf("%-22s", "case")
	for _, n := range sizes {
		fmt.Printf("%14s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("   %s\n", "growth 2k->16k (8x N)")

	for _, c := range cases {
		fmt.Printf("%-22s", c.name)
		var first, last time.Duration
		for i, n := range sizes {
			eng, rows := seedForLoadOpt(t, n, batch, c.indexed)
			st := time.Now()
			res, err := eng.RunAny(context.Background(), c.stmt, map[string]any{"rows": rows})
			if err != nil {
				fmt.Printf("  ERROR %v", err)
				break
			}
			for res.Next() {
			}
			_ = res.Close()
			d := time.Since(st)
			fmt.Printf("%14s", d.Round(time.Microsecond))
			if i == 0 {
				first = d
			}
			last = d
		}
		if first > 0 {
			fmt.Printf("   %.1fx\n", float64(last)/float64(first))
		} else {
			fmt.Println()
		}
	}
}

func seedForLoadOpt(tb testing.TB, n, batch int, indexed bool) (*cypher.Engine, []any) {
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
	if indexed {
		if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_sid FOR (x:P) ON (x.sid)`, nil); err != nil {
			tb.Fatalf("create index: %v", err)
		}
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
