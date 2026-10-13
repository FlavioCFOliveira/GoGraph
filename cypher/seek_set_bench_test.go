package cypher_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// BenchmarkSeekSet_Lookup runs a three-key set against a hash index, for keys all
// present in the index, some present, and none present, at the seek floor (64
// nodes) and above it (rmp #3062).
func BenchmarkSeekSet_Lookup(b *testing.B) {
	sets := []struct{ name, keys string }{
		{"present", "['v5','v6','v7']"},
		{"mixed", "['v5','x2','v7']"},
		{"absent", "['x1','x2','x3']"},
	}
	for _, n := range []int{64, 2000, 20000} {
		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)
		ctx := context.Background()
		for _, q := range []string{
			fmt.Sprintf("UNWIND range(0, %d) AS i CREATE (:L {p: 'v' + toString(i)})", n-1),
			"CREATE INDEX l_p FOR (n:L) ON (n.p)",
		} {
			res, err := eng.RunAny(ctx, q, nil)
			if err != nil {
				b.Fatalf("%s: %v", q, err)
			}
			if err := res.Close(); err != nil {
				b.Fatalf("%s: close: %v", q, err)
			}
		}
		for _, s := range sets {
			q := "UNWIND " + s.keys + " AS k MATCH (n:L {p: k}) RETURN n.p"
			b.Run(fmt.Sprintf("n=%d/keys=%s", n, s.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					res, err := eng.Run(ctx, q, nil)
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
