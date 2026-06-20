package cypher

// presence_scalar_use_internal_test.go — sprint 222 #1638 (white box).
//
// analyseNodeScalarUse classifies, per variable, the property keys read as a
// pure presence test (`v.k IS [NOT] NULL` and nowhere else) into presenceKeys,
// distinct from the value-use keys set. These tests pin the classification and
// the C1 invariant — a key used BOTH for presence and value is value-needed and
// must NOT appear in presenceKeys — directly on the analyser, which a black-box
// query test cannot observe (the placeholder it gates never escapes a row).
// Layer: short.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// v builds a bound-variable reference.
func varRef(name string) *ast.Variable { return &ast.Variable{Name: name} }

// prop builds `recv.key`.
func prop(recv ast.Expression, key string) *ast.Property {
	return &ast.Property{Receiver: recv, Key: key}
}

// isNull / isNotNull build the postfix null predicates.
func isNull(operand ast.Expression) *ast.UnaryOp {
	return &ast.UnaryOp{Operator: "IS NULL", Operand: operand}
}
func isNotNull(operand ast.Expression) *ast.UnaryOp {
	return &ast.UnaryOp{Operator: "IS NOT NULL", Operand: operand}
}

func keySet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAnalyseNodeScalarUse_PresenceClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		expr         ast.Expression
		wantBail     bool
		wantVar      string
		wantPresence []string // expected presenceKeys for wantVar
		wantValue    []string // expected value keys for wantVar
		wantWhole    bool     // expected needsWholeNode for wantVar
	}{
		{
			name:         "bare IS NOT NULL is presence-only",
			expr:         isNotNull(prop(varRef("r"), "since")),
			wantVar:      "r",
			wantPresence: []string{"since"},
			wantValue:    nil,
		},
		{
			name:         "bare IS NULL is presence-only",
			expr:         isNull(prop(varRef("r"), "since")),
			wantVar:      "r",
			wantPresence: []string{"since"},
			wantValue:    nil,
		},
		{
			// `r.since IS NOT NULL AND r.k > 1` — since is presence-only, k is a
			// value use. They are different keys, so since stays presence.
			name: "distinct presence and value keys coexist",
			expr: &ast.BinaryOp{
				Operator: "AND",
				Left:     isNotNull(prop(varRef("r"), "since")),
				Right: &ast.BinaryOp{
					Operator: ">",
					Left:     prop(varRef("r"), "k"),
					Right:    &ast.IntLiteral{Value: 1},
				},
			},
			wantVar:      "r",
			wantPresence: []string{"since"},
			wantValue:    []string{"k"},
		},
		{
			// C1: the SAME key used for both presence and value is value-needed —
			// `r.since IS NOT NULL AND r.since > 2020` must drop since from
			// presenceKeys.
			name: "C1 same key presence+value -> value only",
			expr: &ast.BinaryOp{
				Operator: "AND",
				Left:     isNotNull(prop(varRef("r"), "since")),
				Right: &ast.BinaryOp{
					Operator: ">",
					Left:     prop(varRef("r"), "since"),
					Right:    &ast.IntLiteral{Value: 2020},
				},
			},
			wantVar:      "r",
			wantPresence: nil,
			wantValue:    []string{"since"},
		},
		{
			// A property inside a function arg is a value use even under IS NULL:
			// `toLower(r.name) IS NULL` — the operand is a FunctionInvocation, not
			// a direct r.name, so it walks into a value use.
			name:         "IS NULL over function arg is value use",
			expr:         isNull(&ast.FunctionInvocation{Name: "toLower", Args: []ast.Expression{prop(varRef("r"), "name")}}),
			wantVar:      "r",
			wantPresence: nil,
			wantValue:    []string{"name"},
		},
		{
			// `r["since"] IS NULL` (string-literal subscript) is intentionally NOT
			// presence-only — only the direct *ast.Property shape qualifies — so it
			// records a value key.
			name: "string-literal subscript under IS NULL is value use",
			expr: isNull(&ast.SubscriptExpr{
				Expr:  varRef("r"),
				Index: &ast.StringLiteral{Value: "since"},
			}),
			wantVar:      "r",
			wantPresence: nil,
			wantValue:    []string{"since"},
		},
		{
			// A bare variable under IS NULL (`r IS NULL`) needs the whole entity,
			// not a property presence.
			name:      "bare variable IS NULL needs whole node",
			expr:      isNull(varRef("r")),
			wantVar:   "r",
			wantWhole: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			uses, bail := analyseNodeScalarUse(tc.expr)
			if bail != tc.wantBail {
				t.Fatalf("bailout = %v, want %v", bail, tc.wantBail)
			}
			u, ok := uses[tc.wantVar]
			if !ok {
				t.Fatalf("no use recorded for %q; uses=%v", tc.wantVar, uses)
			}
			assertKeys(t, "presenceKeys", u.presenceKeys, tc.wantPresence)
			assertKeys(t, "keys", u.keys, tc.wantValue)
			if u.needsWholeNode != tc.wantWhole {
				t.Fatalf("needsWholeNode = %v, want %v", u.needsWholeNode, tc.wantWhole)
			}
			// C1 invariant, always: presenceKeys and keys are disjoint.
			for k := range u.presenceKeys {
				if _, both := u.keys[k]; both {
					t.Fatalf("C1 violated: key %q is in both presenceKeys and keys", k)
				}
			}
		})
	}
}

func assertKeys(t *testing.T, label string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, keySet(got), want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s missing %q (have %v)", label, k, keySet(got))
		}
	}
}
