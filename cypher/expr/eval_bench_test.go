package expr

// eval_bench_test.go — allocation regression guards for the EvalWith hot path.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// BenchmarkEvalWithBindingFree measures EvalWith on a binding-free expression
// (a nil RowContext: e.g. RETURN 1 + 2, a constant projection). This is the
// path #1721 pools the one-entry sentinel map for; the benchmark guards against
// a regression that would reintroduce a per-evaluation map allocation.
func BenchmarkEvalWithBindingFree(b *testing.B) {
	e := &ast.BinaryOp{
		Left:     &ast.IntLiteral{Value: 1},
		Operator: "+",
		Right:    &ast.IntLiteral{Value: 2},
	}
	ctx := context.Background()
	reg := nopReg{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EvalWith(ctx, e, nil, nil, reg, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
