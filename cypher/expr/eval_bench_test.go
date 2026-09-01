package expr

// eval_bench_test.go — allocation regression guards for the EvalWith hot path.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// BenchmarkEvalWithBindingFree measures EvalWith on a binding-free expression
// (a nil RowContext: e.g. RETURN 1 + 2, a constant projection). #1721 served
// this path with a pooled one-entry map, because the per-evaluation state was
// smuggled through the RowContext and a nil row had no slot to carry it; #2653
// made that state an explicit parameter, so a nil row now stays nil and the
// path touches no map at all. The benchmark guards against a regression that
// would reintroduce a per-evaluation allocation here.
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
