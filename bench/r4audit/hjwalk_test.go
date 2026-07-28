//go:build r4audit

package r4audit

// hjwalk_test.go — rmp #2234 acceptance criterion 5: measure whether retiring the
// read path's hash-join order-safety scan is worth anything in wall clock, and
// record the answer either way.
//
// The task's premise was that the scan is "a per-query whole-plan IR walk the read
// path runs on every Run". That was true when #1719 was filed and false by the
// time #2234 ran: #1719 had already memoised it onto the plan-cache entry, so the
// execution path only READ a bool and the walk itself happened once per plan-cache
// MISS. So there are two costs to separate, and only the second can have moved:
//
//   - warm cache: a field read. Retiring it saves nothing measurable.
//   - cold plan build: one whole-plan IR walk per miss. That is what is gone.
//
// ClearPlanCache before each iteration forces the miss path, so the cold column
// includes parse + plan + the walk; the warm column includes none of them. The
// walk's share is whatever the cold column lost between the two trees, which is
// why this must be run on both.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestHashJoinWalkCost times the plan-cache-miss path against the warm path for a
// query shaped to make the retired scan as expensive as it could be: a deep plan
// (the walk visits every node) that also carries the operators the scan looked
// for.
func TestHashJoinWalkCost(t *testing.T) {
	eng := newEng(t, 200)

	cases := []struct{ name, q string }{
		{"equijoin + bare LIMIT", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id LIMIT 5`},
		{"equijoin + collect", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN collect(a.id)`},
		{"equijoin + ORDER BY (scan said safe)", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id ORDER BY a.id`},
		{
			"deep plan, many WITH stages",
			`MATCH (a:P), (b:P) WHERE a.age = b.age WITH a, b WHERE a.id < b.id ` +
				`WITH a, b ORDER BY a.id WITH a, b LIMIT 50 WITH a, b SKIP 1 RETURN a.id, b.id`,
		},
	}

	const reps = 200
	fmt.Printf("%-40s %14s %14s %14s\n", "case", "cold/build", "warm", "delta")
	for _, c := range cases {
		// Warm-up so the first-ever run's lazy initialisation is not attributed to
		// either column.
		if _, err := eng.RunAny(context.Background(), c.q, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		cold := time.Hour
		for k := 0; k < reps; k++ {
			eng.ClearPlanCache()
			st := time.Now()
			res, err := eng.RunAny(context.Background(), c.q, nil)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			for res.Next() {
			}
			_ = res.Close()
			if d := time.Since(st); d < cold {
				cold = d
			}
		}

		warm := time.Hour
		for k := 0; k < reps; k++ {
			st := time.Now()
			res, err := eng.RunAny(context.Background(), c.q, nil)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			for res.Next() {
			}
			_ = res.Close()
			if d := time.Since(st); d < warm {
				warm = d
			}
		}
		fmt.Printf("%-40s %14s %14s %14s\n", c.name,
			cold.Round(time.Nanosecond), warm.Round(time.Nanosecond), (cold - warm).Round(time.Nanosecond))
	}
}
