package cypher_test

// map_value_call_commas_2975_test.go — regression gate for rmp #2975.
//
// openCypher allows any expression as a map value. The write path splits a
// property map's source text into items at its commas, and the splitter tracked
// strings, maps and lists but not parentheses, so a value that is a call with
// more than one argument — `reduce(s = 'z', i IN range(1, 16) | s + s)`,
// `substring('abc', 0, 1)` — was cut inside the call and the plan failed with
// "missing ':' in map item". Every write clause that carries a property map
// shares the splitter: CREATE node and relationship maps, MERGE, and SET = /
// SET += with a map literal.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestMapValueCallWithCommas_2975(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup string // optional statement run first
		write string
		check string // returns one row, one value
		want  string
	}{
		{
			name:  "CREATE node map, reduce (the reported shape)",
			write: "CREATE (:Doc {blob: reduce(s = 'z', i IN range(1, 16) | s + s)})",
			check: "MATCH (d:Doc) WITH collect(size(d.blob)) AS sizes RETURN sizes AS v",
			want:  "[65536]", // one node, blob of length 65536
		},
		{
			name:  "CREATE node map, multi-argument function",
			write: "CREATE (:Doc {x: substring('abc', 0, 1)})",
			check: "MATCH (d:Doc) RETURN d.x AS v",
			want:  `"a"`,
		},
		{
			name:  "CREATE node map, call followed by a sibling item",
			write: "CREATE (:Doc {blob: reduce(s = 'z', i IN [1, 2] | s + s), y: 7})",
			check: "MATCH (d:Doc) RETURN d.blob + toString(d.y) AS v",
			want:  `"zzzz7"`,
		},
		{
			name:  "CREATE node map, call inside a list value",
			write: "CREATE (:Doc {xs: [substring('ab', 0, 1), 'c']})",
			check: "MATCH (d:Doc) RETURN d.xs[0] + d.xs[1] AS v",
			want:  `"ac"`,
		},
		{
			name:  "CREATE relationship map",
			write: "CREATE (:Doc)-[:R {blob: reduce(s = 'z', i IN range(1, 2) | s + s)}]->(:Doc)",
			check: "MATCH ()-[r:R]->() RETURN r.blob AS v",
			want:  `"zzzz"`,
		},
		{
			name:  "MERGE node map creates, then matches",
			setup: "MERGE (:Doc {blob: reduce(s = 'z', i IN range(1, 2) | s + s)})",
			write: "MERGE (:Doc {blob: reduce(s = 'z', i IN range(1, 2) | s + s)})",
			check: "MATCH (d:Doc) RETURN collect(d.blob) AS v",
			want:  `["zzzz"]`,
		},
		{
			name:  "SET += map",
			setup: "CREATE (:Doc {k: 1})",
			write: "MATCH (d:Doc) SET d += {blob: reduce(s = 'z', i IN range(1, 2) | s + s)}",
			check: "MATCH (d:Doc) RETURN toString(d.k) + d.blob AS v",
			want:  `"1zzzz"`,
		},
		{
			name:  "SET = map",
			setup: "CREATE (:Doc {k: 1})",
			write: "MATCH (d:Doc) SET d = {blob: substring('abc', 1, 2)}",
			check: "MATCH (d:Doc) RETURN coalesce(toString(d.k), '-') + d.blob AS v",
			want:  `"-bc"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
			if c.setup != "" {
				if _, err := e.RunAny(ctx, c.setup, nil); err != nil {
					t.Fatalf("setup %s: %v", c.setup, err)
				}
			}
			if _, err := e.RunAny(ctx, c.write, nil); err != nil {
				t.Fatalf("%s: %v", c.write, err)
			}
			res, err := e.RunAny(ctx, c.check, nil)
			if err != nil {
				t.Fatalf("%s: %v", c.check, err)
			}
			defer func() { _ = res.Close() }() // read-only use; Close error carries nothing here
			if !res.Next() {
				t.Fatalf("%s: no row (err %v)", c.check, res.Err())
			}
			if got := res.ValueAt(0).String(); got != c.want {
				t.Fatalf("%s: got %s, want %s", c.check, got, c.want)
			}
		})
	}
}
