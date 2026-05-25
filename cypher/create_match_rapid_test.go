package cypher_test

// create_match_rapid_test.go — T877
//
// Property-based test using pgregory.net/rapid.
//
// For n random nodes (1–50), each is CREATEd via the Cypher engine; a
// subsequent MATCH count must equal n. This exercises the full CREATE → MATCH
// pipeline end-to-end with varied inputs and serves as a property-based smoke
// test for the write path.
//
// Layer: short. Race-clean.

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestCreate_ThenMatch_Rapid is a property-based test that creates between 1
// and 50 Item nodes via Cypher and then asserts that MATCH (n:Item) returns
// exactly that count.
func TestCreate_ThenMatch_Rapid(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 50).Draw(rt, "n")

		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)
		ctx := context.Background()

		for i := 0; i < n; i++ {
			name := fmt.Sprintf("node%d", i)
			res, err := eng.RunInTxAny(ctx, `CREATE (n:Item {name: "`+name+`"})`, nil)
			if err != nil {
				rt.Fatalf("CREATE node%d: %v", i, err)
			}
			for res.Next() {
			}
			if iterErr := res.Err(); iterErr != nil {
				_ = res.Close()
				rt.Fatalf("CREATE node%d iter: %v", i, iterErr)
			}
			if closeErr := res.Close(); closeErr != nil {
				rt.Fatalf("CREATE node%d close: %v", i, closeErr)
			}
		}

		// MATCH all Item nodes and assert count == n.
		countRes, err := eng.Run(ctx, `MATCH (n:Item) RETURN count(*) AS cnt`, nil)
		if err != nil {
			rt.Fatalf("MATCH count: %v", err)
		}

		var rows []map[string]any
		for countRes.Next() {
			rec := countRes.Record()
			row := make(map[string]any, len(rec))
			for k, v := range rec {
				row[k] = v
			}
			rows = append(rows, row)
		}
		if iterErr := countRes.Err(); iterErr != nil {
			_ = countRes.Close()
			rt.Fatalf("count iter: %v", iterErr)
		}
		if closeErr := countRes.Close(); closeErr != nil {
			rt.Fatalf("count close: %v", closeErr)
		}

		if len(rows) != 1 {
			rt.Fatalf("count query returned %d rows, want 1", len(rows))
		}
		got := fmtAny(rows[0]["cnt"])
		want := fmt.Sprintf("%d", n)
		if got != want {
			rt.Errorf("MATCH (n:Item) count = %s, want %s (n=%d)", got, want, n)
		}
	})
}
