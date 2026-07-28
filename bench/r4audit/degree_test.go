//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPerOuterRowCost_DegreeEligible is the acceptance measurement for rmp
// #2232. It contrasts the shapes a faithful getDegreeRewriter port CAN admit —
// no label or property anywhere in the pattern, because Neo4j's
// QuerySolvableByGetDegree requires Selections.empty — against the ones it
// cannot, including the labelled shape the round-4 audit happened to benchmark.
//
// Before #2232, per outer row at N=8000 against a 0.028 µs bare-scan baseline:
//
//	ELIGIBLE count>0 untyped   0.929 µs   33.6×
//	ELIGIBLE count>0 typed     1.605 µs   58.1×
//	ELIGIBLE count=n typed     1.624 µs   58.8×
//	ELIGIBLE exists typed      0.243 µs    8.8×
//	ELIGIBLE size(pattern)     2.275 µs   82.4×
//	INELIGIBLE far label       2.282 µs   82.6×
//
// Two of those are expected NOT to move and are kept as controls. `exists typed`
// never reaches the rewrite: a top-level EXISTS is lowered to SemiApply in the
// IR before the expression evaluator sees it, which is why it was already the
// cheapest shape. `size(pattern)` in a RETURN projection is hoisted into a
// RollUpApply for the same reason; the rewrite covers size() where the
// comprehension survives as an expression, e.g. in a WHERE, which
// TestDegreeRewrite_Identity pins.
func TestPerOuterRowCost_DegreeEligible(t *testing.T) {
	shapes := []struct{ name, q string }{
		{"baseline-scan", `MATCH (a:P) RETURN count(a)`},
		{"ELIGIBLE count>0 untyped", `MATCH (a:P) WHERE COUNT { (a)-->() } > 0 RETURN count(a)`},
		{"ELIGIBLE count>0 typed", `MATCH (a:P) WHERE COUNT { (a)-[:K]->() } > 0 RETURN count(a)`},
		{"ELIGIBLE exists typed", `MATCH (a:P) WHERE EXISTS { (a)-[:K]->() } RETURN count(a)`},
		{"ELIGIBLE count=n typed", `MATCH (a:P) WHERE COUNT { (a)-[:K]->() } = 4 RETURN count(a)`},
		{"ELIGIBLE size(pattern)", `MATCH (a:P) RETURN sum(size([ (a)-[:K]->(b) | b ]))`},
		{"INELIGIBLE far label", `MATCH (a:P) WHERE COUNT { (a)-[:K]->(:P) } > 0 RETURN count(a)`},
		{"INELIGIBLE binds far node", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b) RETURN b } RETURN count(a)`},
	}
	sizes := []int{2000, 8000}

	fmt.Printf("%-30s", "shape")
	for _, n := range sizes {
		fmt.Printf("%12d", n)
	}
	fmt.Printf("   %10s %8s\n", "us/row@8k", "vs base")

	var base float64
	for i, s := range shapes {
		fmt.Printf("%-30s", s.name)
		var last time.Duration
		ok := true
		for _, n := range sizes {
			eng := newEng(t, n)
			if _, err := eng.RunAny(context.Background(), s.q, nil); err != nil {
				fmt.Printf("  ERROR %v", err)
				ok = false
				break
			}
			best := time.Hour
			for k := 0; k < 5; k++ {
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
		if !ok {
			fmt.Println()
			continue
		}
		per := float64(last.Microseconds()) / 8000.0
		if i == 0 {
			base = per
		}
		fmt.Printf("   %10.3f %7.1fx\n", per, per/base)
	}
}
