package dot_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
)

// Security regression battery for the DOT writer's handling of node ids that
// collide with the DOT language's reserved keywords.
//
// #1489 (FIXED): The Graphviz DOT grammar
// (https://graphviz.org/doc/info/lang.html) defines node, edge, graph,
// digraph, subgraph, and strict as case-INDEPENDENT keywords that "may not be
// used as identifiers" unless quoted. The writer's isSimpleID previously
// treated them as ordinary alphabetic identifiers and emitted them UNQUOTED, so
// a graph that legitimately contains a vertex named (for example) "node" —
// entirely plausible: a Node.js dependency graph, or a metamodel whose vertices
// are literally "graph"/"edge"/"node" — produced a document Graphviz misparses
// or reinterprets:
//
//	digraph G {
//	  node -> safe;   // "node" is a keyword: Graphviz reads this as a default
//	                  // node-attribute statement, NOT as an edge from a vertex
//	                  // called node — silently corrupting the exported graph.
//	}
//
// This was an export-integrity defect (CWE-116 improper output encoding;
// related CWE-838 inappropriate encoding for output context): the round-trip
// was silently lossy and the emitted DOT no longer represented the graph it was
// given. The fix quotes any id equal (case-insensitively) to a DOT keyword,
// exactly as the writer already quotes ids carrying metacharacters.

// dotReservedKeywords is the complete case-independent keyword set per the
// Graphviz DOT language reference.
var dotReservedKeywords = []string{"node", "edge", "graph", "digraph", "subgraph", "strict"}

// dotKeywordCasings returns the keyword in the casings an attacker (or an
// ordinary data set) might present: lowercase, UPPERCASE, and Title case. All
// three are keywords to Graphviz because keyword matching is case-independent.
func dotKeywordCasings(kw string) []string {
	return []string{kw, strings.ToUpper(kw), strings.ToUpper(kw[:1]) + kw[1:]}
}

func emitEdgeDOT(t *testing.T, src, dst string) string {
	t.Helper()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(src, dst, 0); err != nil {
		t.Fatalf("AddEdge(%q,%q): %v", src, dst, err)
	}
	var buf bytes.Buffer
	if err := dot.Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.String()
}

// bareKeywordHead matches a keyword appearing UNQUOTED as the head (left
// operand) of an edge statement: `<indent>node -> `. The (?m) anchors to a
// line start so the "digraph G {" header is never matched.
func bareKeywordHead(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(id) + `\s+->`)
}

// TestSec_IO_DOTExportQuotesReservedKeywords is the strict gate of the secure
// behaviour: every DOT reserved keyword, in every casing, used as a node id
// must be emitted QUOTED so it survives as a vertex name, and must never appear
// bare (which Graphviz would reinterpret as a keyword, corrupting the export).
func TestSec_IO_DOTExportQuotesReservedKeywords(t *testing.T) {
	t.Parallel()

	for _, kw := range dotReservedKeywords {
		for _, id := range dotKeywordCasings(kw) {
			id := id
			t.Run(id, func(t *testing.T) {
				t.Parallel()
				out := emitEdgeDOT(t, id, "safe")
				if !strings.Contains(out, `"`+id+`"`) {
					t.Errorf("DOT reserved keyword %q must be QUOTED to survive as a vertex name; got:\n%s", id, out)
				}
				if bareKeywordHead(id).MatchString(out) {
					t.Errorf("DOT reserved keyword %q emitted UNQUOTED — Graphviz will reinterpret it as a keyword, corrupting the export:\n%s", id, out)
				}
			})
		}
	}
}

// TestSec_IO_DOTExportDoesNotOverQuote guards against an over-quoting
// regression: ordinary (non-keyword) alphanumeric ids must remain UNQUOTED.
func TestSec_IO_DOTExportDoesNotOverQuote(t *testing.T) {
	t.Parallel()

	ordinary := []string{"safe", "alpha", "node1", "edge_case", "Graphics", "subgraphX", "myNode"}
	for _, id := range ordinary {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			out := emitEdgeDOT(t, id, "dst")
			if strings.Contains(out, `"`+id+`"`) {
				t.Errorf("ordinary id %q must NOT be quoted (over-quoting regression); got:\n%s", id, out)
			}
		})
	}
}
