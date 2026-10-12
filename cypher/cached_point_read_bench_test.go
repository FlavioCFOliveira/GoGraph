package cypher

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// BenchmarkCachedPointRead measures one plan-cache-hit execution of the indexed
// point read that dominates example 35 (rmp #3096, #3097): parse and plan are
// cached, so the cost is the snapshot, the physical build, and the drain. Every
// answer is checked.
func BenchmarkCachedPointRead(b *testing.B) {
	const n = 3000
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("a%d", i)
		if err := g.AddNode(id); err != nil {
			b.Fatal(err)
		}
		if err := g.SetNodeLabel(id, "Account"); err != nil {
			b.Fatal(err)
		}
		if err := g.SetNodeProperty(id, "id", lpg.Int64Value(int64(i))); err != nil {
			b.Fatal(err)
		}
		if err := g.SetNodeProperty(id, "balance", lpg.Int64Value(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	e := NewEngine(g)
	ctx := context.Background()
	if _, err := e.RunInTx(ctx, "CREATE INDEX FOR (n:Account) ON (n.id)", nil); err != nil {
		b.Fatal(err)
	}
	const q = "MATCH (n:Account {id: $id}) RETURN n.balance AS b"
	params := map[string]expr.Value{"id": expr.IntegerValue(0)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := int64(i % n)
		params["id"] = expr.IntegerValue(k)
		r, err := e.Run(ctx, q, params)
		if err != nil {
			b.Fatal(err)
		}
		if !r.Next() || r.ValueAt(0) != expr.IntegerValue(k) {
			b.Fatalf("id=%d: wrong answer", k)
		}
	}
}
