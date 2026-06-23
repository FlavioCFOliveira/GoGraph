package parser

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// firstMatchPath returns the first PathPattern of the first MATCH/OPTIONAL MATCH
// clause of a parsed single query, or nil.
func firstMatchPath(t *testing.T, q ast.Query) *ast.PathPattern {
	t.Helper()
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		t.Fatalf("expected *ast.SingleQuery, got %T", q)
	}
	for _, rc := range sq.ReadingClauses {
		switch c := rc.(type) {
		case *ast.Match:
			if c.Pattern != nil && len(c.Pattern.Paths) > 0 {
				return c.Pattern.Paths[0]
			}
		case *ast.OptionalMatch:
			if c.Pattern != nil && len(c.Pattern.Paths) > 0 {
				return c.Pattern.Paths[0]
			}
		}
	}
	return nil
}

func TestParse_ShortestPath_Kind(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  ast.ShortestKind
	}{
		{"shortestPath", `MATCH p = shortestPath((a)-[*]-(b)) RETURN p`, ast.ShortestSingle},
		{"allShortestPaths", `MATCH p = allShortestPaths((a)-[*]-(b)) RETURN p`, ast.ShortestAll},
		{"plain-path", `MATCH p = (a)-[*]-(b) RETURN p`, ast.ShortestNone},
		{"case-insensitive", `MATCH p = SHORTESTPATH((a)-[*]-(b)) RETURN p`, ast.ShortestSingle},
		{"optional-match", `OPTIONAL MATCH p = shortestPath((a)-[*]-(b)) RETURN p`, ast.ShortestSingle},
		{"typed-bounded-directed", `MATCH p = shortestPath((a)-[:T*1..3]->(b)) RETURN p`, ast.ShortestSingle},
		{"extra-spaces", `MATCH p =  shortestPath(  (a)-[*]-(b)  ) RETURN p`, ast.ShortestSingle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := Parse(c.query)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", c.query, err)
			}
			pp := firstMatchPath(t, q)
			if pp == nil {
				t.Fatalf("no MATCH path pattern found in %q", c.query)
			}
			if pp.Shortest != c.want {
				t.Errorf("Shortest = %d, want %d", pp.Shortest, c.want)
			}
			if pp.Variable == nil || *pp.Variable != "p" {
				t.Errorf("path variable = %v, want p", pp.Variable)
			}
			// The inner pattern must survive: a head node plus at least one hop.
			if pp.Head == nil || pp.Head.Next == nil {
				t.Errorf("inner pattern lost: head=%+v", pp.Head)
			}
		})
	}
}

// TestParse_ShortestPath_PreservesInnerDetail checks the wrapped pattern's
// relationship type filter, variable-length bound and direction are preserved
// through the rewrite (the inner pattern is copied verbatim).
func TestParse_ShortestPath_PreservesInnerDetail(t *testing.T) {
	q, err := Parse(`MATCH p = shortestPath((a)-[:KNOWS|FOLLOWS*1..4]->(b)) RETURN p`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	pp := firstMatchPath(t, q)
	if pp == nil || pp.Head == nil || pp.Head.Next == nil {
		t.Fatalf("inner pattern missing")
	}
	rel := pp.Head.Next.Relationship
	if rel == nil {
		t.Fatalf("relationship pattern missing")
	}
	if len(rel.Types) != 2 || rel.Types[0] != "KNOWS" || rel.Types[1] != "FOLLOWS" {
		t.Errorf("rel types = %v, want [KNOWS FOLLOWS]", rel.Types)
	}
	if rel.Range == nil || rel.Range.Min == nil || *rel.Range.Min != 1 || rel.Range.Max == nil || *rel.Range.Max != 4 {
		t.Errorf("rel range not preserved: %+v", rel.Range)
	}
	if rel.Direction != ast.RelDirectionOutgoing {
		t.Errorf("rel direction = %v, want outgoing", rel.Direction)
	}
}

// TestParse_ShortestPath_NotRewrittenInStringsOrComments ensures the scanner
// only rewrites real path bindings, never the word inside a string literal, a
// backtick identifier, or a comment, and does not misfire on a `=` equality in
// WHERE/RETURN.
func TestParse_ShortestPath_NotRewrittenInStringsOrComments(t *testing.T) {
	cases := []string{
		`MATCH (a) WHERE a.name = 'shortestPath((x)-[*]-(y))' RETURN a`,
		"MATCH (a) RETURN a.`shortestPath` AS s",
		"MATCH (a) // shortestPath((x)-[*]-(y))\nRETURN a",
		`MATCH (a) RETURN 'allShortestPaths' AS s`,
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			q, err := Parse(query)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", query, err)
			}
			// None of these bind a named shortest path, so no PathPattern in the
			// query may carry a non-None Shortest kind.
			walkPathPatterns(q, func(pp *ast.PathPattern) {
				if pp.Shortest != ast.ShortestNone {
					t.Errorf("unexpected Shortest=%d on %q", pp.Shortest, query)
				}
			})
		})
	}
}

// TestRewriteShortestPath_Markers unit-tests the text rewrite in isolation.
func TestRewriteShortestPath_Markers(t *testing.T) {
	in := `MATCH p = shortestPath((a)-[*]-(b)), q = allShortestPaths((c)-[*]-(d)) RETURN p, q`
	out, markers := rewriteShortestPath(in)
	if got := out; got != `MATCH p = (a)-[*]-(b), q = (c)-[*]-(d) RETURN p, q` {
		t.Errorf("rewrite = %q", got)
	}
	if len(markers) != 2 {
		t.Fatalf("markers = %d, want 2", len(markers))
	}
	if markers[0] != (spMarker{pathVar: "p", kind: ast.ShortestSingle}) {
		t.Errorf("markers[0] = %+v", markers[0])
	}
	if markers[1] != (spMarker{pathVar: "q", kind: ast.ShortestAll}) {
		t.Errorf("markers[1] = %+v", markers[1])
	}
}

// TestRewriteShortestPath_NoWrapperUntouched confirms the fast path returns the
// input verbatim with nil markers when no wrapper is present.
func TestRewriteShortestPath_NoWrapperUntouched(t *testing.T) {
	in := `MATCH p = (a)-[*]-(b) WHERE a.x = 1 RETURN p`
	out, markers := rewriteShortestPath(in)
	if out != in {
		t.Errorf("expected untouched, got %q", out)
	}
	if markers != nil {
		t.Errorf("expected nil markers, got %+v", markers)
	}
}
