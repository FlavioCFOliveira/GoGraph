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

// TestW1PartA_GatesEngage checks that the order-neutral gates threaded into the
// write path by #2225 part A actually engage on a shape that can use them — a
// selective RANGE predicate inside a writing statement. Part A is inert on the
// UNWIND bulk-load shape (that one needs the hash join, part B), so without this
// the change would be unmeasured.
//
// A read control runs the same predicate so the two can be compared: before part
// A the write was a full label scan while the read was a range seek.
func TestW1PartA_GatesEngage(t *testing.T) {
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
			if err := g.SetNodeProperty(key, "num", lpg.Int64Value(int64(i))); err != nil {
				t.Fatal(err)
			}
		}
		eng := cypher.NewEngine(g)
		if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_num FOR (x:P) ON (x.num)`, nil); err != nil {
			t.Fatalf("index: %v", err)
		}
		return eng
	}

	cases := []struct{ name, stmt string }{
		{"range read  (control)", `MATCH (a:P) WHERE a.num >= 10 AND a.num < 20 RETURN count(a)`},
		{"range WRITE (part A)", `MATCH (a:P) WHERE a.num >= 10 AND a.num < 20 SET a.touched = true`},
		{"range WRITE, create", `MATCH (a:P) WHERE a.num >= 10 AND a.num < 20 CREATE (a)-[:K]->(:Z)`},
	}
	sizes := []int{4000, 16000, 64000}

	fmt.Printf("%-24s", "case")
	for _, n := range sizes {
		fmt.Printf("%13s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("  growth 16x N\n")

	for _, c := range cases {
		fmt.Printf("%-24s", c.name)
		var first, last time.Duration
		for i, n := range sizes {
			eng := seed(n)
			// warm
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
