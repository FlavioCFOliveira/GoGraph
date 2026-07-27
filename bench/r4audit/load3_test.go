//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSeekReachesWriteStatements decides between the two candidate causes of the
// O(N) edge-load cost: (a) the correlated (UNWIND-bound) seek does not reach a
// statement that also writes, or (b) relationship creation is itself O(N).
//
// Each pair differs in ONE thing — whether the key is a bound row value or a
// literal — so a literal-key case that is FLAT in N while the bound-key case
// grows convicts (a); both growing convicts (b).
func TestSeekReachesWriteStatements(t *testing.T) {
	const batch = 500
	sizes := []int{2000, 4000, 8000, 16000}

	cases := []struct{ name, stmt string }{
		{"bound-key  read", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN count(a)`},
		{"literal-key read", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) RETURN count(a)`},
		{"bound-key  write", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`},
		{"literal-key write", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) CREATE (a)-[:K]->(:Z)`},
	}

	fmt.Printf("%-20s", "case")
	for _, n := range sizes {
		fmt.Printf("%14s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("   growth (8x N)\n")

	for _, c := range cases {
		fmt.Printf("%-20s", c.name)
		var first, last time.Duration
		for i, n := range sizes {
			eng, rows := seedForLoadOpt(t, n, batch, true)
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
