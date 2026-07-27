//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPerOuterRowCost measures how the CorrelatedApply-backed shapes scale in
// the number of OUTER rows, holding the inner work per row constant. A linear
// fit means the per-outer-row re-Init of the inner sub-plan is a constant tax;
// a super-linear fit means the inner plan re-scans.
func TestPerOuterRowCost(t *testing.T) {
	shapes := []struct{ name, q string }{
		{"baseline-scan", `MATCH (a:P) RETURN count(a)`},
		{"exists-subquery", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b:P) RETURN b } RETURN count(a)`},
		{"count-subquery", `MATCH (a:P) WHERE COUNT { (a)-[:K]->(:P) } > 0 RETURN count(a)`},
		{"pattern-predicate", `MATCH (a:P) WHERE (a)-[:K]->(:P) RETURN count(a)`},
		{"optional-match", `MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b) RETURN count(a)`},
		{"list-comprehension", `MATCH (a:P) RETURN count([ (a)-[:K]->(b) | b.id ])`},
	}
	sizes := []int{1000, 2000, 4000, 8000}

	fmt.Printf("%-22s", "shape")
	for _, n := range sizes {
		fmt.Printf("%12d", n)
	}
	fmt.Printf("   %10s\n", "us/row@8k")

	for _, s := range shapes {
		fmt.Printf("%-22s", s.name)
		var last time.Duration
		for _, n := range sizes {
			eng := newEng(t, n)
			// warm
			if _, err := eng.RunAny(context.Background(), s.q, nil); err != nil {
				fmt.Printf("  ERROR %v", err)
				break
			}
			best := time.Hour
			for i := 0; i < 5; i++ {
				st := time.Now()
				res, err := eng.RunAny(context.Background(), s.q, nil)
				if err != nil {
					t.Fatalf("%s: %v", s.name, err)
				}
				for res.Next() {
				}
				_ = res.Close()
				if d := time.Since(st); d < best {
					best = d
				}
			}
			fmt.Printf("%12s", best.Round(time.Microsecond))
			last = best
		}
		fmt.Printf("   %10.3f\n", float64(last.Microseconds())/8000.0)
	}
}
