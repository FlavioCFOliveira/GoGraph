package cypher

// rel_type_source_3080_test.go — the regression gate for rmp #3080: the
// relationship value's TYPE is resolved from the first source that answers —
// the by-handle record, then the per-CREATE instance record, then the per-pair
// union — and the per-pair union is read only when neither per-instance source
// answered. The union reads are keyed by the row's NodeIDs, swapped with the
// storage orientation, rather than by the endpoint keys.
//
// Every expectation is a hand-computed ABSOLUTE value. The asymmetric cases are
// the ones that fail if the NodeID pair is not swapped together with the key
// pair on a hop taken against storage direction: (a -> b) and (b -> a) carry
// different types, so reading the wrong orientation reports the wrong type.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestRelTypeSource3080(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, g *lpg.Graph[string, float64], eng *Engine)
		query string
		want  []string // sorted rows
	}{
		{
			// Go-API edge: no by-handle record, no CREATE count, so the per-pair
			// union decides. Read forward.
			name: "go-api edge, forward hop",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				if err := g.AddEdgeLabeledWithProperty("a", "b", 1, "K", "w", lpg.Int64Value(7)); err != nil {
					t.Fatal(err)
				}
			},
			query: "MATCH (x:P {id: 0})-[r]->(y) RETURN type(r), r.w, startNode(r).id, endNode(r).id",
			want:  []string{"K|7|0|1"},
		},
		{
			// The same edge reached against storage direction: the union must be
			// read on the STORAGE pair (a, b), not on the traversal pair (b, a).
			name: "go-api edge, undirected hop against storage direction",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				if err := g.AddEdgeLabeledWithProperty("a", "b", 1, "K", "w", lpg.Int64Value(7)); err != nil {
					t.Fatal(err)
				}
			},
			query: "MATCH (x:P {id: 1})-[r]-(y) RETURN type(r), r.w, startNode(r).id, endNode(r).id",
			want:  []string{"K|7|0|1"},
		},
		{
			// Asymmetric pair: (a -> b) is K and (b -> a) is M. Both undirected
			// rows from a must report their own storage pair's type.
			name: "go-api asymmetric pair, undirected from a",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
					t.Fatal(err)
				}
				if err := g.AddEdgeLabeled("b", "a", 1, "M"); err != nil {
					t.Fatal(err)
				}
			},
			query: "MATCH (x:P {id: 0})-[r]-(y) RETURN type(r), startNode(r).id, endNode(r).id",
			want:  []string{"K|0|1", "M|1|0"},
		},
		{
			name: "go-api asymmetric pair, undirected from b",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
					t.Fatal(err)
				}
				if err := g.AddEdgeLabeled("b", "a", 1, "M"); err != nil {
					t.Fatal(err)
				}
			},
			query: "MATCH (x:P {id: 1})-[r]-(y) RETURN type(r), startNode(r).id, endNode(r).id",
			want:  []string{"K|0|1", "M|1|0"},
		},
		{
			// Cypher-created parallel edges: each carries a by-handle type record,
			// which decides; the per-pair union {X, Y} must not leak into either.
			name: "cypher parallel edges, forward",
			build: func(t *testing.T, _ *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 2, "Y")
				createTyped(t, eng, 0, 2, "X")
			},
			query: "MATCH (x:P {id: 0})-[r]->(y:P {id: 2}) RETURN type(r)",
			want:  []string{"X", "Y"},
		},
		{
			name: "cypher parallel edges, undirected against storage direction",
			build: func(t *testing.T, _ *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 2, "Y")
				createTyped(t, eng, 0, 2, "X")
			},
			query: "MATCH (x:P {id: 2})-[r]-(y) RETURN type(r), startNode(r).id, endNode(r).id",
			want:  []string{"X|0|2", "Y|0|2"},
		},
		{
			// Cypher asymmetric pair: (a -> c) is X and (c -> a) is Z.
			name: "cypher asymmetric pair, undirected from c",
			build: func(t *testing.T, _ *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 2, "X")
				createTyped(t, eng, 2, 0, "Z")
			},
			query: "MATCH (x:P {id: 2})-[r]-(y) RETURN type(r), startNode(r).id, endNode(r).id",
			want:  []string{"X|0|2", "Z|2|0"},
		},
		{
			// Mixed pair: one Cypher (by-handle) edge and one Go-API edge on the
			// same storage pair. The Cypher edge reports its own type; the Go-API
			// edge falls through to the union, whose smallest member is K.
			name: "mixed cypher and go-api edges on one pair",
			build: func(t *testing.T, g *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 1, "Z")
				if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
					t.Fatal(err)
				}
			},
			query: "MATCH (x:P {id: 0})-[r:Z]->(y) RETURN type(r)",
			want:  []string{"Z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, eng := perSlotFixture(t)
			tc.build(t, g, eng)
			got := runRows(t, eng, tc.query)
			for i := range got {
				got[i] = strings.ReplaceAll(got[i], `"`, "") // strings render quoted
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s\n got  %q\n want %q", tc.query, got, tc.want)
			}
		})
	}
}

// BenchmarkRelTypeSource3080 drives buildRelationshipValueFromRow on a hub of
// fan-out deg, so the per-pair union scan (O(deg) per row) is visible. "cypher"
// builds every edge through CREATE (by-handle typed: the union is never needed);
// "goapi" builds them through AddEdgeLabeledWithProperty (no by-handle record:
// the union decides the type).
func BenchmarkRelTypeSource3080(b *testing.B) {
	const deg = 256
	const q = "MATCH (x:P {id: 0})-[r]->(y) RETURN type(r), r.w"
	for _, mode := range []string{"cypher", "goapi"} {
		b.Run(mode, func(b *testing.B) {
			g := lpg.New[string, float64](adjlist.Config{})
			g.SetIndexManager(index.NewManager())
			eng := NewEngine(g)
			for i := 0; i <= deg; i++ {
				k := fmt.Sprintf("n%d", i)
				if err := g.AddNode(k); err != nil {
					b.Fatal(err)
				}
				if err := g.SetNodeLabel(k, "P"); err != nil {
					b.Fatal(err)
				}
				if err := g.SetNodeProperty(k, "id", lpg.Int64Value(int64(i))); err != nil {
					b.Fatal(err)
				}
			}
			for i := 1; i <= deg; i++ {
				if mode == "cypher" {
					res, err := eng.RunInTx(context.Background(), fmt.Sprintf(
						"MATCH (x:P {id: 0}), (y:P {id: %d}) CREATE (x)-[:K {w: %d}]->(y)", i, i), nil)
					if err != nil {
						b.Fatal(err)
					}
					_ = res.Close()
					continue
				}
				if err := g.AddEdgeLabeledWithProperty("n0", fmt.Sprintf("n%d", i), 1, "K", "w", lpg.Int64Value(int64(i))); err != nil {
					b.Fatal(err)
				}
			}
			if got := len(runRowsB(b, eng, q)); got != deg {
				b.Fatalf("rows = %d, want %d", got, deg)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				runRowsB(b, eng, q)
			}
		})
	}
}

func runRowsB(b *testing.B, eng *Engine, q string) []expr.Value {
	b.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	var out []expr.Value
	for res.Next() {
		out = append(out, res.ValueAt(0))
	}
	if err := res.Err(); err != nil {
		b.Fatal(err)
	}
	return out
}
