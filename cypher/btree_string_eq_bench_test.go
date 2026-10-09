package cypher_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// BenchmarkBTreeStringEq_Lookup runs a parameterised string equality against a
// btree-only index, for a value present in the index and one absent from it, at
// the seek floor (64 nodes) and above it (rmp #3061).
func BenchmarkBTreeStringEq_Lookup(b *testing.B) {
	for _, n := range []int{64, 2000, 20000} {
		eng := newBtreeStringEngine(b, n)
		for _, val := range []string{"s5", "absent"} {
			params, err := cypher.BindParams(map[string]any{"k": val})
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("n=%d/val=%s", n, val), func(b *testing.B) {
				ctx := context.Background()
				b.ReportAllocs()
				for b.Loop() {
					res, err := eng.Run(ctx, "MATCH (a:K) WHERE a.sk = $k RETURN a.sk AS k", params)
					if err != nil {
						b.Fatal(err)
					}
					for res.Next() {
					}
					if err := res.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
