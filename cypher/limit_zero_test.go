package cypher_test

// limit_zero_test.go — T690
//
// Integration tests for LIMIT edge cases: zero, one, and a value larger than
// the result set. Uses a 3-node graph so all three boundary values differ from
// the existing ordering_test.go tests (which use 2-node graphs for LIMIT 0 and
// LIMIT > size, and n-node graphs for LIMIT 2).
//
// Key differences from ordering_test.go:
//   - Graph size is 3 instead of 2, so LIMIT > size returns 3, not 2.
//   - LIMIT 1 is explicitly tested (not covered by ordering_test.go).
//   - All three cases are grouped in a single table-driven test.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newThreeNodeGraph creates a plain unlabeled 3-node graph with keys "x", "y",
// "z". No properties are set — only node identity is needed.
func newThreeNodeGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for _, key := range []string{"x", "y", "z"} {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %q: %v", key, err)
		}
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// Table-driven LIMIT edge cases
// ─────────────────────────────────────────────────────────────────────────────

// TestLimitZero_EdgeCases covers LIMIT 0, LIMIT 1, and LIMIT larger than the
// result set against a 3-node graph. Each case verifies both the returned row
// count and the absence of an error.
func TestLimitZero_EdgeCases(t *testing.T) {
	g := newThreeNodeGraph(t)

	cases := []struct {
		limit int
		want  int
	}{
		{0, 0},
		{1, 1},
		{100, 3},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			eng := cypher.NewEngine(g)
			query := fmt.Sprintf("MATCH (n) RETURN n LIMIT %d", tc.limit)
			res, err := eng.Run(context.Background(), query, nil)
			if err != nil {
				t.Fatalf("Run(limit=%d): %v", tc.limit, err)
			}
			got := countRows(t, res)
			if got != tc.want {
				t.Errorf("LIMIT %d: got %d rows, want %d", tc.limit, got, tc.want)
			}
		})
	}
}
