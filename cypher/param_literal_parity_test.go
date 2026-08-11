package cypher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// These tests pin rmp #2414: a PARAMETER must get the same access path as the
// equivalent LITERAL.
//
// The defect they guard was not subtle in effect, only in visibility. The string
// range extractor admitted literals only — "parameter range seeks are
// deliberately out of scope for this increment" — while its numeric companion
// had been extended to parameters. So `n.sk = 'lit'` seeked a btree index and
// `n.sk = $p`, the spelling every driver sends and every guide recommends, fell
// back to a full NodeByLabelScan. The engine was at its worst on exactly the
// shape callers are told to use, and nothing failed: the answers were right, the
// plans merely collapsed.
//
// Each case therefore asserts BOTH arms of the pair, not just the parameter one.
// Asserting only the parameter would pass on a build where the literal had
// regressed to a scan too, which is the same "compares nothing" failure the
// btree differential fell into once parameters started seeking.

// parityCase is one predicate written twice — once inlined, once parameterised.
type parityCase struct {
	name    string
	literal string
	param   string
	binding map[string]expr.Value
}

func parityCases() []parityCase {
	str := func(s string) map[string]expr.Value { return map[string]expr.Value{"p": expr.StringValue(s)} }
	return []parityCase{
		{
			"equality in a pattern property map",
			`MATCH (a:K {sk: 's250'}) RETURN a.sk AS k`,
			`MATCH (a:K {sk: $p}) RETURN a.sk AS k`,
			str("s250"),
		},
		{
			"equality in WHERE",
			`MATCH (a:K) WHERE a.sk = 's250' RETURN a.sk AS k`,
			`MATCH (a:K) WHERE a.sk = $p RETURN a.sk AS k`,
			str("s250"),
		},
		{
			"equality with the property on the right",
			`MATCH (a:K) WHERE 's250' = a.sk RETURN a.sk AS k`,
			`MATCH (a:K) WHERE $p = a.sk RETURN a.sk AS k`,
			str("s250"),
		},
		{
			"STARTS WITH",
			`MATCH (a:K) WHERE a.sk STARTS WITH 's25' RETURN a.sk AS k`,
			`MATCH (a:K) WHERE a.sk STARTS WITH $p RETURN a.sk AS k`,
			str("s25"),
		},
		{
			// A BOUNDED range. An open one (`>= 's250'` alone) selects about
			// half the label, which the selectivity gate rightly keeps on a
			// scan for BOTH spellings — a pair that agrees on "no seek" would
			// pass this test while proving nothing about parity.
			"bounded range",
			`MATCH (a:K) WHERE a.sk >= 's250' AND a.sk < 's251' RETURN a.sk AS k`,
			`MATCH (a:K) WHERE a.sk >= $p AND a.sk < 's251' RETURN a.sk AS k`,
			str("s250"),
		},
	}
}

// TestParamLiteralParity_SameAccessPath is the plan-shape half.
func TestParamLiteralParity_SameAccessPath(t *testing.T) {
	t.Parallel()
	eng := newBtreeStringEngine(t, 2000)

	for _, tc := range parityCases() {
		t.Run(tc.name, func(t *testing.T) {
			litPlan, err := eng.Explain(tc.literal, nil)
			if err != nil {
				t.Fatalf("Explain literal: %v", err)
			}
			parPlan, err := eng.Explain(tc.param, tc.binding)
			if err != nil {
				t.Fatalf("Explain param: %v", err)
			}

			litSeeks := strings.Contains(litPlan, "NodeByIndexRangeScan")
			parSeeks := strings.Contains(parPlan, "NodeByIndexRangeScan")

			// The literal arm is asserted too: if it stopped seeking, the
			// parity assertion below would hold vacuously on two scans.
			if !litSeeks {
				t.Fatalf("the LITERAL arm does not seek, so this pair proves nothing:\n%s", litPlan)
			}
			if !parSeeks {
				t.Errorf("the parameter arm falls back to a scan while the identical literal seeks:\n"+
					"literal:\n%s\nparameter:\n%s", litPlan, parPlan)
			}
		})
	}
}

// TestParamLiteralParity_SameRows is the answer half. A seek that returns the
// wrong rows is worse than a scan that returns the right ones, so the plan-shape
// assertions above are only safe alongside this.
func TestParamLiteralParity_SameRows(t *testing.T) {
	t.Parallel()
	eng := newBtreeStringEngine(t, 2000)

	for _, tc := range parityCases() {
		t.Run(tc.name, func(t *testing.T) {
			lit := rowsFor(t, eng, tc.literal, nil)

			res, err := eng.Run(context.Background(), tc.param, tc.binding)
			if err != nil {
				t.Fatalf("run param: %v", err)
			}
			var par []string
			for res.Next() {
				if v, ok := res.Record()["k"]; ok {
					par = append(par, strings.Trim(v.(expr.StringValue).String(), `"`))
				}
			}
			if err := res.Err(); err != nil {
				t.Fatalf("run param: %v", err)
			}
			_ = res.Close()

			if len(lit) == 0 {
				t.Fatalf("the literal arm returned no rows, so agreement proves nothing")
			}
			if len(lit) != len(par) {
				t.Fatalf("literal returned %d rows, parameter %d", len(lit), len(par))
			}
		})
	}
}

// TestParamLiteralParity_NonStringParamDeclines guards the other direction. A
// parameter that is absent, null, or not a string must NOT produce a string
// range bound; the scan-and-filter path answers instead. Widening the extractor
// to accept a parameter must not have widened it to accept a wrong-typed one.
func TestParamLiteralParity_NonStringParamDeclines(t *testing.T) {
	t.Parallel()
	eng := newBtreeStringEngine(t, 2000)
	const q = `MATCH (a:K) WHERE a.sk STARTS WITH $p RETURN a.sk AS k`

	for _, tc := range []struct {
		name string
		bind map[string]expr.Value
	}{
		{"absent", map[string]expr.Value{}},
		{"null", map[string]expr.Value{"p": expr.Null}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := eng.Explain(q, tc.bind)
			if err != nil {
				// An absent parameter may legitimately be an error; what must
				// not happen is a seek built from a value that is not a string.
				return
			}
			if strings.Contains(plan, "NodeByIndexRangeScan") {
				t.Errorf("a %s parameter produced a range seek:\n%s", tc.name, plan)
			}
		})
	}
}

// The one asymmetry NOT asserted here is the parameter type check: a
// type-incompatible parameter can raise a *sema.ParamTypeError where the
// equivalent literal yields zero rows. That is deliberate, it is index-type
// dependent, and it is already pinned in both directions by
// index_seek_param_type_test.go. It is called out because it is the reason
// literal hoisting (#2412) must exempt its auto-parameters: a hoisted literal
// has to behave like the literal it replaced, not like a user parameter.
