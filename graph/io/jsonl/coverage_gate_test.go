package jsonl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// errAfterN is an io.Writer that returns errWriteFailed once a cumulative
// total of more than n bytes has been written. bufio.Writer only flushes to
// the underlying writer when its 64 KiB buffer overflows or on an explicit
// Flush, so the threshold lets a test choose whether the failure surfaces in
// the encode loop (n small relative to the payload) or only at the final
// Flush (n at least the payload size).
type errAfterN struct {
	n       int
	written int
}

var errWriteFailed = errors.New("jsonl test: simulated write failure")

func (w *errAfterN) Write(p []byte) (int, error) {
	if w.written >= w.n {
		return 0, errWriteFailed
	}
	w.written += len(p)
	if w.written > w.n {
		// Accept up to the boundary, then report the failure.
		return len(p), errWriteFailed
	}
	return len(p), nil
}

// bigAdjList builds a directed adjacency list whose serialised node records
// comfortably exceed the 64 KiB bufio buffer, so a failing writer trips
// inside the encode loop rather than only at Flush.
func bigAdjList(t *testing.T, nodes int) *adjlist.AdjList[string, int64] {
	t.Helper()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := range nodes {
		// Long keys inflate each node record so a few thousand nodes
		// overflow the 64 KiB write buffer.
		if err := a.AddNode(fmt.Sprintf("node-with-a-fairly-long-identifier-%06d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	return a
}

// TestReadIntoCtx_ScannerError covers the bufio scanner error branch
// (token too long) in ReadIntoCtx.
func TestReadIntoCtx_ScannerError(t *testing.T) {
	t.Parallel()
	// A single line longer than the 16 MiB scanner cap triggers
	// bufio.ErrTooLong, which is neither io.EOF nor nil.
	huge := strings.Repeat("a", 17*1024*1024)
	_, _, err := ReadInto(strings.NewReader(huge), adjlist.Config{Directed: true})
	if err == nil {
		t.Fatal("expected scanner error for oversized line")
	}
}

// TestReadWithProps_ErrorWrapper covers the error-counter branch of the
// ReadWithProps convenience wrapper.
func TestReadWithProps_ErrorWrapper(t *testing.T) {
	t.Parallel()
	_, _, err := ReadWithProps(strings.NewReader("not json\n"), adjlist.Config{Directed: true})
	if err == nil {
		t.Fatal("expected error from ReadWithProps on malformed JSON")
	}
}

// TestReadWithPropsCtx_Branches exercises the dispatch, validation and
// decode error branches of ReadWithPropsCtx that the roundtrip tests do
// not reach.
func TestReadWithPropsCtx_Branches(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true}

	cases := []struct {
		name  string
		input string
	}{
		{"bad_json", "{not json}\n"},
		{"node_missing_id", `{"type":"node"}` + "\n"},
		{"edge_missing_src", `{"type":"edge","dst":"b"}` + "\n"},
		{"edge_missing_dst", `{"type":"edge","src":"a"}` + "\n"},
		{"property_missing_id", `{"type":"property","key":"k","kind":"string","value":"v"}` + "\n"},
		{"property_missing_key", `{"type":"property","id":"a","kind":"string","value":"v"}` + "\n"},
		{"property_bad_value", `{"type":"node","id":"a"}` + "\n" + `{"type":"property","id":"a","key":"age","kind":"int64","value":"notanint"}` + "\n"},
		{"unknown_type", `{"type":"spaceship"}` + "\n"},
		{"empty_type", `{"type":""}` + "\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ReadWithPropsCtx(context.Background(), strings.NewReader(tc.input), cfg)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// TestReadWithPropsCtx_EmptyLine covers the blank-line skip branch.
func TestReadWithPropsCtx_EmptyLine(t *testing.T) {
	t.Parallel()
	in := "\n" + `{"type":"node","id":"a"}` + "\n" + "\n"
	g, rows, err := ReadWithPropsCtx(context.Background(), strings.NewReader(in), adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("ReadWithPropsCtx: %v", err)
	}
	// Three physical lines are consumed (two blank, one node).
	if rows != 3 {
		t.Fatalf("rows = %d, want 3", rows)
	}
	if _, ok := g.AdjList().Mapper().Lookup("a"); !ok {
		t.Fatal("node a missing")
	}
}

// TestReadWithPropsCtx_Cancelled covers the pre-cancelled ctx.Err() branch.
func TestReadWithPropsCtx_Cancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := `{"type":"node","id":"a"}` + "\n"
	_, _, err := ReadWithPropsCtx(ctx, strings.NewReader(in), adjlist.Config{Directed: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestReadWithPropsCtx_ScannerError covers the bufio scanner error branch.
func TestReadWithPropsCtx_ScannerError(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("a", 17*1024*1024)
	_, _, err := ReadWithPropsCtx(context.Background(), strings.NewReader(huge), adjlist.Config{Directed: true})
	if err == nil {
		t.Fatal("expected scanner error for oversized line")
	}
}

// TestDecodePropertyValue covers every kind of decodePropertyValue,
// including the parse-error sub-branch of each numeric/temporal kind and
// the unknown-kind default.
func TestDecodePropertyValue(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			kind, value string
			want        lpg.PropertyKind
		}{
			{"string", "hello", lpg.PropString},
			{"int64", "42", lpg.PropInt64},
			{"float64", "3.5", lpg.PropFloat64},
			{"bool", "true", lpg.PropBool},
			{"time", "2020-01-02T03:04:05Z", lpg.PropTime},
			{"bytes", "AAEC", lpg.PropBytes}, // base64 of {0,1,2}
		}
		for _, tc := range cases {
			pv, err := decodePropertyValue(tc.kind, tc.value)
			if err != nil {
				t.Fatalf("decodePropertyValue(%q,%q): %v", tc.kind, tc.value, err)
			}
			if pv.Kind() != tc.want {
				t.Fatalf("kind = %v, want %v", pv.Kind(), tc.want)
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		cases := []struct{ kind, value string }{
			{"int64", "notanint"},
			{"float64", "notafloat"},
			{"bool", "notabool"},
			{"time", "notatime"},
			{"bytes", "!!!not-base64!!!"},
			{"mystery", "x"}, // unknown kind default branch
		}
		for _, tc := range cases {
			_, err := decodePropertyValue(tc.kind, tc.value)
			if err == nil {
				t.Fatalf("expected error for kind=%q value=%q", tc.kind, tc.value)
			}
		}
	})
}

// TestEncodePropertyValue_Default covers the zero/unknown-kind default
// branch of encodePropertyValue.
func TestEncodePropertyValue_Default(t *testing.T) {
	t.Parallel()
	kind, value := encodePropertyValue(lpg.PropertyValue{})
	if kind != "unknown" || value != "" {
		t.Fatalf("encodePropertyValue(zero) = (%q,%q), want (\"unknown\",\"\")", kind, value)
	}
}

// TestWrite_FailingWriter covers the error-counter wrapper of Write and the
// flush/encode error branches of WriteCtx.
func TestWrite_FailingWriter(t *testing.T) {
	t.Parallel()

	t.Run("flush_error_small_graph", func(t *testing.T) {
		t.Parallel()
		// A tiny graph fits in the buffer, so the error only surfaces at
		// Flush. n=0 makes every underlying Write fail.
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		if err := a.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if _, err := Write(&errAfterN{n: 0}, a); !errors.Is(err, errWriteFailed) {
			t.Fatalf("expected errWriteFailed, got %v", err)
		}
	})

	t.Run("encode_error_node_loop", func(t *testing.T) {
		t.Parallel()
		// Many large node records overflow the buffer, so the failure
		// surfaces inside the node encode loop.
		a := bigAdjList(t, 4000)
		if _, err := WriteCtx(context.Background(), &errAfterN{n: 0}, a); !errors.Is(err, errWriteFailed) {
			t.Fatalf("expected errWriteFailed, got %v", err)
		}
	})

	t.Run("encode_error_edge_loop", func(t *testing.T) {
		t.Parallel()
		// Let all node records through (n large enough), then fail when the
		// edge loop starts writing, exercising the edge encode error branch.
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		for i := range 4000 {
			if err := a.AddEdge(
				fmt.Sprintf("src-with-a-long-name-%06d", i),
				fmt.Sprintf("dst-with-a-long-name-%06d", i),
				int64(i),
			); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		// Allow the node phase (~4000 short-ish records) but fail partway
		// through edges by capping bytes generously above node output yet
		// below node+edge output.
		var sizer countingWriter
		if _, err := WriteCtx(context.Background(), &sizer, a); err != nil {
			t.Fatalf("sizing pass: %v", err)
		}
		w := &errAfterN{n: sizer.n / 2}
		if _, err := WriteCtx(context.Background(), w, a); !errors.Is(err, errWriteFailed) {
			t.Fatalf("expected errWriteFailed, got %v", err)
		}
	})
}

// TestWriteWithProps_FailingWriter covers the error-counter wrapper of
// WriteWithProps and the encode/flush error branches of WriteWithPropsCtx
// across all three record phases.
func TestWriteWithProps_FailingWriter(t *testing.T) {
	t.Parallel()

	t.Run("flush_error_small_graph", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty("a", "k", lpg.StringValue("v")); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		if _, err := WriteWithProps(&errAfterN{n: 0}, g); !errors.Is(err, errWriteFailed) {
			t.Fatalf("expected errWriteFailed, got %v", err)
		}
	})

	t.Run("encode_error_node_phase", func(t *testing.T) {
		t.Parallel()
		g := bigLPG(t, 4000, false)
		if _, err := WriteWithPropsCtx(context.Background(), &errAfterN{n: 0}, g); !errors.Is(err, errWriteFailed) {
			t.Fatalf("expected errWriteFailed, got %v", err)
		}
	})

	t.Run("encode_error_property_phase", func(t *testing.T) {
		t.Parallel()
		// Small graph with one node and many properties so that the node
		// phase fits in the buffer but the property phase overflows it,
		// surfacing the failure inside the property encode loop.
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		for i := range 5000 {
			if err := g.SetNodeProperty("n",
				fmt.Sprintf("property-key-with-a-long-name-%06d", i),
				lpg.StringValue(fmt.Sprintf("value-with-a-long-payload-%06d", i)),
			); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
		}
		if _, err := WriteWithPropsCtx(context.Background(), &errAfterN{n: 0}, g); !errors.Is(err, errWriteFailed) {
			t.Fatalf("expected errWriteFailed, got %v", err)
		}
	})
}

// TestWriteWithPropsCtx_Cancelled covers the ctx.Err() branches in both the
// edge loop and the property loop of WriteWithPropsCtx.
func TestWriteWithPropsCtx_Cancelled(t *testing.T) {
	t.Parallel()

	t.Run("edge_phase", func(t *testing.T) {
		t.Parallel()
		// 4096 nodes plus one edge: the edge-loop ctx check fires at
		// written==4096 on the first edge iteration.
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		for i := range 4096 {
			if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		if err := g.AddEdge("n0", "n1", 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var sink countingWriter
		if _, err := WriteWithPropsCtx(ctx, &sink, g); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("property_phase", func(t *testing.T) {
		t.Parallel()
		// 4096 nodes, no edges, plus one property: the property-loop ctx
		// check fires at written==4096 on the first property iteration.
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		for i := range 4096 {
			if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		if err := g.SetNodeProperty("n0", "k", lpg.StringValue("v")); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var sink countingWriter
		if _, err := WriteWithPropsCtx(ctx, &sink, g); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}

// countingWriter is an io.Writer that discards its input and records the
// cumulative byte count. It never fails, so it is safe for sizing passes
// and for ctx-cancellation tests that must not also trip a write error.
type countingWriter struct{ n int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

// bigLPG builds a labelled property graph whose serialised node records
// overflow the 64 KiB write buffer. When withEdges is true every adjacent
// pair is linked so the edge phase also produces overflowing output.
func bigLPG(t *testing.T, nodes int, withEdges bool) *lpg.Graph[string, int64] {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for i := range nodes {
		if err := g.AddNode(fmt.Sprintf("node-with-a-fairly-long-identifier-%06d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if withEdges {
		for i := 0; i+1 < nodes; i++ {
			if err := g.AddEdge(
				fmt.Sprintf("node-with-a-fairly-long-identifier-%06d", i),
				fmt.Sprintf("node-with-a-fairly-long-identifier-%06d", i+1),
				int64(i),
			); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	return g
}
