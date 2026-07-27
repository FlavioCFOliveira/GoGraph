//go:build r4audit

package r4audit

import (
	"fmt"
	"testing"
)

// TestHashJoinTrigger isolates which projections let the #1506 hash join fire.
// The order-safety guard disables it when an "arrival-order" aggregation is
// anywhere in the plan; this checks whether order-INSENSITIVE aggregations
// (count/sum/min/max) are caught by the same net.
func TestHashJoinTrigger(t *testing.T) {
	eng := newEng(t, 200)
	shapes := []struct{ name, q string }{
		{"equijoin-entities", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a, b`},
		{"equijoin-scalars", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id, b.id`},
		{"equijoin-count", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)`},
		{"equijoin-sum", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN sum(a.id)`},
		{"equijoin-min", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN min(a.id)`},
		{"equijoin-collect", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN collect(a.id)`},
		{"equijoin-orderby", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id ORDER BY a.id`},
		{"equijoin-limit", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN a.id LIMIT 5`},
		{"equijoin-distinct", `MATCH (a:P), (b:P) WHERE a.age = b.age RETURN DISTINCT a.id`},
		{"equijoin-with-count", `MATCH (a:P), (b:P) WHERE a.age = b.age WITH a RETURN count(a)`},
	}
	for _, s := range shapes {
		plan, err := eng.Explain(s.q, nil)
		if err != nil {
			fmt.Printf("%-22s ERROR %v\n", s.name, err)
			continue
		}
		verdict := "CartesianProduct (nested loop)"
		if contains(plan, "HashJoin") {
			verdict = "HashJoin"
		}
		fmt.Printf("%-22s %s\n", s.name, verdict)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
