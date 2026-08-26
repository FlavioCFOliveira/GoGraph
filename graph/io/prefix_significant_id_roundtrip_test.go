package io_test

// prefix_significant_id_roundtrip_test.go — regression gate for rmp #2533: a
// node identifier whose FIRST character is significant to the export format's
// own syntax must survive an export -> import cycle, or be refused. It must
// never be exported successfully, imported successfully, and quietly absent.
//
// The reported case was the CSV comment character: the writer emitted the id
// verbatim and the reader discarded any line beginning with it, so the node
// vanished with both sides reporting success. rmp #2042 had in fact already
// closed that hole by force-quoting such a cell, but nothing asserted the
// property at the NODE level and nothing had checked the other three formats
// for the same class of exposure. This test asserts both, across every format,
// so the class cannot reopen unnoticed in any of them.

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	csvio "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/dot"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// prefixSignificantNames is one id per format-syntax hazard, each chosen so its
// FIRST character is the significant one — which is the whole point, since a
// prefix is what a line-oriented reader dispatches on.
//
//	"#hash"       the CSV reader's comment character, and DOT's preprocessor line
//	"//slashes"   a DOT line comment
//	"/*block*/"   a DOT block comment
//	"<xml>"       GraphML markup, and an unescaped '<' would break the document
//	"&amp;"       an XML entity reference that must NOT be re-decoded on import
//	"{json}"      a JSONL object opener
//	"-danger"     a live spreadsheet formula (the csv.Options.SanitizeFormulae cell)
//	`a"b`         a quote, which every one of the four formats must escape
var prefixSignificantNames = []string{
	"#hash", "//slashes", "/*block*/", "<xml>", "&amp;", "{json}", "-danger", `a"b`, "plain",
}

// prefixSignificantGraph is a directed cycle over the names above, so every id
// appears once as a source and once as a destination: a format that loses an id
// in only one of the two columns is still caught.
func prefixSignificantGraph(t *testing.T) *adjlist.AdjList[string, int64] {
	t.Helper()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	for i := range prefixSignificantNames {
		src := prefixSignificantNames[i]
		dst := prefixSignificantNames[(i+1)%len(prefixSignificantNames)]
		if err := a.AddEdge(src, dst, int64(i+1)); err != nil {
			t.Fatalf("AddEdge(%q, %q): %v", src, dst, err)
		}
	}
	return a
}

// ioNodeNames returns every live vertex name, sorted.
func ioNodeNames(a *adjlist.AdjList[string, int64]) []string {
	out := make([]string, 0, a.Order())
	a.Mapper().Walk(func(_ graph.NodeID, v string) bool {
		out = append(out, v)
		return true
	})
	sort.Strings(out)
	return out
}

// ioEdgeTriples builds the name-keyed (src, dst, weight) multiset, so two graphs
// compare independently of internal NodeID assignment.
func ioEdgeTriples(a *adjlist.AdjList[string, int64]) map[string]int {
	names := make(map[graph.NodeID]string, a.Order())
	a.Mapper().Walk(func(id graph.NodeID, name string) bool {
		names[id] = name
		return true
	})
	out := make(map[string]int)
	maxID := uint64(a.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		src, ok := names[graph.NodeID(id)]
		if !ok {
			continue
		}
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			dst, ok := names[n]
			if !ok {
				continue
			}
			var w int64
			if ws != nil {
				w = ws[i]
			}
			out[src+"\x00"+dst+"\x00"+strconv.FormatInt(w, 10)]++
		}
	}
	return out
}

// assertSameGraph compares node sets and edge multisets, naming the format so a
// failure says which one lost what.
func assertSameGraph(t *testing.T, format string, want, got *adjlist.AdjList[string, int64]) {
	t.Helper()
	wantNames, gotNames := ioNodeNames(want), ioNodeNames(got)
	if strings.Join(wantNames, "\x00") != strings.Join(gotNames, "\x00") {
		t.Errorf("%s: node set changed across the round-trip\n want %q\n got  %q", format, wantNames, gotNames)
	}
	wantEdges, gotEdges := ioEdgeTriples(want), ioEdgeTriples(got)
	for k, n := range wantEdges {
		if gotEdges[k] != n {
			t.Errorf("%s: edge %q multiplicity %d, want %d", format, strings.ReplaceAll(k, "\x00", " -> "), gotEdges[k], n)
		}
	}
	for k, n := range gotEdges {
		if wantEdges[k] != n {
			t.Errorf("%s: edge %q appeared with multiplicity %d, absent from the source", format, strings.ReplaceAll(k, "\x00", " -> "), n)
		}
	}
}

// TestPrefixSignificantIDs_CSVRoundTrip is the reported case. The '#'-leading id
// must not be swallowed as a comment line, and — the guard that matters — the
// force-quoting must not be defeated by a NON-DEFAULT comment rune either.
func TestPrefixSignificantIDs_CSVRoundTrip(t *testing.T) {
	t.Parallel()
	src := prefixSignificantGraph(t)
	cases := []struct {
		name string
		opts func() csvio.Options
	}{
		{"default", csvio.DefaultOptions},
		{"tab_delimited", func() csvio.Options { o := csvio.DefaultOptions(); o.Delimiter = '\t'; return o }},
		// The comment rune is '/', so BOTH "//slashes" and "/*block*/" are now
		// comment-prefixed and must be force-quoted: the writer has to key on the
		// configured rune, not on a hard-coded '#'.
		{"slash_comment", func() csvio.Options { o := csvio.DefaultOptions(); o.Comment = '/'; return o }},
		// '-' as the comment rune makes the formula cell the comment cell too.
		{"dash_comment", func() csvio.Options { o := csvio.DefaultOptions(); o.Comment = '-'; return o }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if _, err := csvio.Write(&buf, src, tc.opts()); err != nil {
				t.Fatalf("Write: %v", err)
			}
			got, _, err := csvio.ReadInto(bytes.NewReader(buf.Bytes()), tc.opts())
			if err != nil {
				t.Fatalf("ReadInto: %v\npayload:\n%s", err, buf.String())
			}
			assertSameGraph(t, "csv/"+tc.name, src, got)
			if t.Failed() {
				t.Logf("payload:\n%s", buf.String())
			}
		})
	}
}

// TestPrefixSignificantIDs_JSONLRoundTrip records the answer for JSONL: its
// reader has no comment convention at all, and every id travels inside a JSON
// string that encoding/json escapes, so the class does not arise. Asserted
// rather than assumed.
func TestPrefixSignificantIDs_JSONLRoundTrip(t *testing.T) {
	t.Parallel()
	src := prefixSignificantGraph(t)
	var buf bytes.Buffer
	if _, err := jsonl.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _, err := jsonl.ReadInto(bytes.NewReader(buf.Bytes()), adjlist.Config{Directed: true, Multigraph: false})
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	assertSameGraph(t, "jsonl", src, got)
}

// TestPrefixSignificantIDs_GraphMLRoundTrip records the answer for GraphML: ids
// are XML attribute values, so the hazard is markup rather than a line prefix.
// The two ids that probe it are "<xml>", which would break the document if
// emitted raw, and "&amp;", which must come back as the five characters it is
// and NOT be decoded to "&" — a double-unescape is the silent-corruption shape
// of this class in an XML format.
func TestPrefixSignificantIDs_GraphMLRoundTrip(t *testing.T) {
	t.Parallel()
	src := prefixSignificantGraph(t)
	var buf bytes.Buffer
	if err := graphml.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _, err := graphml.ReadInto(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadInto: %v", err)
	}
	assertSameGraph(t, "graphml", src, got)
}

// TestPrefixSignificantIDs_DOTQuotesEveryHazard records the answer for DOT,
// which is WRITE-ONLY: there is no importer to lose anything, so the property
// asserted is the one a third-party DOT parser depends on — every hazardous id
// is emitted inside double quotes, never bare, so none of them can be read as a
// comment, a preprocessor line, or an operator.
func TestPrefixSignificantIDs_DOTQuotesEveryHazard(t *testing.T) {
	t.Parallel()
	src := prefixSignificantGraph(t)
	var buf bytes.Buffer
	if err := dot.Write(&buf, src); err != nil {
		t.Fatalf("Write: %v", err)
	}
	payload := buf.String()
	for _, name := range prefixSignificantNames {
		if name == "plain" {
			continue // the bare-id path: deliberately NOT quoted
		}
		quoted := `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
		if !strings.Contains(payload, quoted) {
			t.Errorf("dot: id %q does not appear quoted as %s; a bare form would be read as syntax by a DOT parser\npayload:\n%s", name, quoted, payload)
		}
	}
	// No hazardous id may begin a line: even quoted, a line starting with '#'
	// is a preprocessor line to a DOT parser.
	for i, line := range strings.Split(payload, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			t.Errorf("dot: line %d begins with DOT comment syntax: %q", i+1, line)
		}
	}
}
