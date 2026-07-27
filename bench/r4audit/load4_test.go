//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestIsTheWriteClauseRelevant checks the round-4 report's stated root cause for
// W1 — "the row-bound seek does not reach a statement that also writes" — against
// the alternative reading that a PER-ROW VARYING key never reaches the index at
// all, and that the read side merely looked fast because `RETURN count(*)` is
// absorbed by the columnar aggregate path.
//
// If a NON-aggregating read with the same per-row key is as slow as the write,
// the write clause is irrelevant and the report's root cause is wrong.
func TestIsTheWriteClauseRelevant(t *testing.T) {
	const batch = 500
	sizes := []int{2000, 8000, 16000}

	cases := []struct{ name, stmt string }{
		{"per-row key, aggregate read", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN count(a)`},
		{"per-row key, scalar read", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN a.sid`},
		{"per-row key, entity read", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN a`},
		{"per-row key, WRITE", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`},
		{"literal key, aggregate read", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) RETURN count(a)`},
		{"literal key, entity read", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) RETURN a`},
		{"literal key, WRITE", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) CREATE (a)-[:K]->(:Z)`},
	}

	fmt.Printf("%-30s", "case")
	for _, n := range sizes {
		fmt.Printf("%13s", fmt.Sprintf("N=%d", n))
	}
	fmt.Printf("  growth  seek?\n")

	for _, c := range cases {
		fmt.Printf("%-30s", c.name)
		var first, last time.Duration
		seek := "?"
		for i, n := range sizes {
			eng, rows := seedForLoadOpt(t, n, batch, true)
			if i == 0 {
				if plan, err := eng.Explain(c.stmt, nil); err == nil {
					if strings.Contains(plan, "NodeByIndexSeek") {
						seek = "SEEK"
					} else {
						seek = "scan"
					}
				}
			}
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
			fmt.Printf("%13s", d.Round(time.Microsecond))
			if i == 0 {
				first = d
			}
			last = d
		}
		if first > 0 {
			fmt.Printf("  %5.1fx  %s\n", float64(last)/float64(first), seek)
		} else {
			fmt.Println()
		}
	}
}
