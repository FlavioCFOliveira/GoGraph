//go:build r4audit

package r4audit

import (
	"fmt"
	"testing"
)

// TestUnwindSeekPlans shows the plan shape for a per-row-varying seek key, which
// is what an UNWIND-driven load produces. The question it answers: is the MATCH
// already the inner arm of a per-row Apply (so an Init-time key evaluation would
// be driven correctly), or is it a single scan the UNWIND feeds?
func TestUnwindSeekPlans(t *testing.T) {
	eng, _ := seedForLoadOpt(t, 200, 1, true)
	shapes := []struct{ name, q string }{
		{"per-row key, read", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) RETURN a.sid`},
		{"per-row key, write", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}) CREATE (a)-[:K]->(:Z)`},
		{"literal key, read", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) RETURN a.sid`},
		{"literal key, write", `UNWIND $rows AS r MATCH (a:P {sid: 's5'}) CREATE (a)-[:K]->(:Z)`},
		{"per-row two-seek write", `UNWIND $rows AS r MATCH (a:P {sid: r.ss}), (b:P {sid: r.ts}) CREATE (a)-[:K]->(b)`},
		{"WITH-bound literal", `WITH 's5' AS k MATCH (a:P {sid: k}) RETURN a.sid`},
		{"per-row key via WHERE", `UNWIND $rows AS r MATCH (a:P) WHERE a.sid = r.ss RETURN a.sid`},
	}
	for _, s := range shapes {
		plan, err := eng.Explain(s.q, nil)
		if err != nil {
			fmt.Printf("=== %-24s ERROR %v\n", s.name, err)
			continue
		}
		fmt.Printf("=== %s\n%s\n", s.name, plan)
	}
}
