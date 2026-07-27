//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestW1PartA_PlanShapes asks Explain whether the range seek and the min-label
// re-anchor appear in a WRITE plan. Unlike the hash join, both of these ARE
// IR-level rewrites, so Explain can see them (finding P3 concerns physical-only
// substitutions). This is the check that decides whether #2225 part A engages at
// all, or whether it must be backed out as inert.
func TestW1PartA_PlanShapes(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < 20000; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			t.Fatal(err)
		}
		// A rare second label so the min-label re-anchor has something to prefer.
		if i%1000 == 0 {
			if err := g.SetNodeLabel(key, "Rare"); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.SetNodeProperty(key, "num", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(key, "sid", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	eng := cypher.NewEngine(g)
	for _, ddl := range []string{
		`CREATE INDEX p_num FOR (x:P) ON (x.num)`,
		`CREATE INDEX p_sid FOR (x:P) ON (x.sid)`,
	} {
		if _, err := eng.RunAny(context.Background(), ddl, nil); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	shapes := []struct{ name, q string }{
		{"range READ", `MATCH (a:P) WHERE a.num >= 10 AND a.num < 20 RETURN count(a)`},
		{"range WRITE set", `MATCH (a:P) WHERE a.num >= 10 AND a.num < 20 SET a.t = true`},
		{"range WRITE create", `MATCH (a:P) WHERE a.num >= 10 AND a.num < 20 CREATE (a)-[:K]->(:Z)`},
		{"literal READ", `MATCH (a:P {sid: 's5'}) RETURN a.sid`},
		{"literal WRITE", `MATCH (a:P {sid: 's5'}) SET a.t = true`},
		{"min-label READ", `MATCH (a:P:Rare) RETURN count(a)`},
		{"min-label WRITE", `MATCH (a:P:Rare) SET a.t = true`},
	}
	for _, s := range shapes {
		plan, err := eng.Explain(s.q, nil)
		if err != nil {
			fmt.Printf("%-20s ERROR %v\n", s.name, err)
			continue
		}
		marks := []string{}
		for _, m := range []string{"NodeByIndexRangeScan", "NodeByIndexSeek", "Rare"} {
			if strings.Contains(plan, m) {
				marks = append(marks, m)
			}
		}
		if len(marks) == 0 {
			marks = append(marks, "(label scan only)")
		}
		fmt.Printf("%-20s %s\n", s.name, strings.Join(marks, " + "))
	}
}

// TestW1PartA_MinLabelWriteWin measures the cardinality reduction the min-label
// re-anchor delivers inside a write statement, which #2225 part A unlocked. The
// :Rare label holds 1/1000 of the :P population, so the re-anchor should make the
// statement cost track the RARE label rather than the whole :P scan.
func TestW1PartA_MinLabelWriteWin(t *testing.T) {
	seed := func(n int) *cypher.Engine {
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		g.SetIndexManager(index.NewManager())
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("n%d", i)
			if err := g.AddNode(key); err != nil {
				t.Fatal(err)
			}
			if err := g.SetNodeLabel(key, "P"); err != nil {
				t.Fatal(err)
			}
			if i%1000 == 0 {
				if err := g.SetNodeLabel(key, "Rare"); err != nil {
					t.Fatal(err)
				}
			}
		}
		return cypher.NewEngine(g)
	}
	cases := []struct{ name, stmt string }{
		{"min-label READ  (control)", `MATCH (a:P:Rare) RETURN count(a)`},
		{"min-label WRITE set", `MATCH (a:P:Rare) SET a.t = true`},
	}
	sizes := []int{20000, 80000}
	fmt.Printf("%-28s", "case")
	for _, n := range sizes {
		fmt.Printf("%13s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("  growth 4x N\n")
	for _, c := range cases {
		fmt.Printf("%-28s", c.name)
		var first, last time.Duration
		for i, n := range sizes {
			eng := seed(n)
			if res, err := eng.RunAny(context.Background(), c.stmt, nil); err == nil {
				for res.Next() {
				}
				_ = res.Close()
			}
			best := time.Hour
			for k := 0; k < 5; k++ {
				st := time.Now()
				res, err := eng.RunAny(context.Background(), c.stmt, nil)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				for res.Next() {
				}
				_ = res.Close()
				if d := time.Since(st); d < best {
					best = d
				}
			}
			fmt.Printf("%13s", best.Round(time.Microsecond))
			if i == 0 {
				first = best
			}
			last = best
		}
		fmt.Printf("  %5.1fx\n", float64(last)/float64(first))
	}
}
