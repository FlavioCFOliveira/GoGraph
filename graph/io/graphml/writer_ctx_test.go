package graphml

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"gograph/graph/adjlist"
)

// errWriter fails on the first Write call, surfacing a stable error
// so tests can assert the error path.
type errWriter struct {
	err     error
	written int
}

func (w *errWriter) Write(p []byte) (int, error) {
	w.written += len(p)
	return 0, w.err
}

// TestWriteCtx_CancelledContext exercises the early ctx.Err() return
// at the top of WriteCtx.
func TestWriteCtx_CancelledContext(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := WriteCtx(ctx, &buf, a); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteCtx on cancelled ctx returned %v, want context.Canceled", err)
	}
}

// TestWriteCtx_HeaderWriteFails exercises the early IO-error return
// from the XML declaration line.
func TestWriteCtx_HeaderWriteFails(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	sentinel := errors.New("write blew up")
	w := &errWriter{err: sentinel}
	if err := Write(w, a); !errors.Is(err, sentinel) {
		t.Fatalf("Write returned %v, want sentinel", err)
	}
}

// limitedWriter accepts the first n bytes and then fails. This drives
// the node-encoding error path: the header / xmlns / key tag / graph
// open all pass, and the first <node> encoding triggers the error.
type limitedWriter struct {
	cap     int
	written int
	err     error
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.written >= w.cap {
		return 0, w.err
	}
	left := w.cap - w.written
	if len(p) <= left {
		w.written += len(p)
		return len(p), nil
	}
	w.written += left
	return left, w.err
}

// TestWriteCtx_NodeEncodingFails forces a write failure deep enough
// in the document that the failure surfaces from encodeNodes (and
// therefore from the encErr-set branch of encodeNodes' callback).
func TestWriteCtx_NodeEncodingFails(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < 64; i++ {
		if err := a.AddNode("node-prefix-deliberately-long-" + string(rune('a'+i%26))); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}

	// Find a cap that consumes the header and prologue but fails
	// mid-stream. The XML header + xmlns + key + graph open is
	// roughly 200 bytes; cap to that range to trigger node encoding
	// failure inside encodeNodes.
	sentinel := errors.New("limit hit")
	for _, cap := range []int{180, 220, 260} {
		w := &limitedWriter{cap: cap, err: sentinel}
		if err := Write(w, a); err == nil {
			// At extreme caps the prologue itself fails — either way
			// the call must report an error; we accept any non-nil
			// error here because the exact failure point depends on
			// the encoder's internal buffer flush boundary. The
			// important assertion is that we never silently succeed.
			t.Fatalf("cap=%d: Write succeeded, expected an IO error", cap)
		}
	}
}

// TestWriteCtx_EmptyGraph exercises the encoder happy path on an
// AdjList with no nodes — the smallest non-trivial code path through
// encodeNodes / encodeEdges (both walk an empty mapper).
func TestWriteCtx_EmptyGraph(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := Write(io.Discard, a); err != nil {
		t.Fatalf("Write empty graph: %v", err)
	}
}
