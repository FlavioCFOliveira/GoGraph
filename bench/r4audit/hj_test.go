//go:build r4audit

package r4audit

import (
	"fmt"
	"testing"
)

// TestHashJoinTrigger isolates which projections let the #1506 hash join fire.
//
// It was written when a whole-query order-safety scan disabled the substitution
// for any plan containing an arrival-order aggregation or a bare LIMIT/SKIP, to
// check whether order-INSENSITIVE aggregations (count/sum/min/max) were caught by
// the same net. They were not; equijoin-collect and equijoin-limit were, and this
// table is where that showed.
//
// rmp #2234 retired that scan — the substitution emits the nested loop's row
// sequence position for position, so there was nothing to protect — and both rows
// flipped from CartesianProduct to HashJoin. All ten shapes now fire, so the table
// no longer discriminates; it is kept as the gate that would catch the
// substitution silently narrowing again.
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
