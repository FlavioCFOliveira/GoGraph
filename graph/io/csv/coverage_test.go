package csv

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestReadIntoCtx_ContextCancelled covers the ctx.Err() path in ReadIntoCtx.
// The check fires at rows=0 (0&0xFFF==0) before any row is consumed.
func TestReadIntoCtx_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ReadIntoCtx(ctx, strings.NewReader("alice,bob,0\n"), Options{Directed: true})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// TestWriteCtx_ContextCancelled covers the ctx.Err() path in WriteCtx.
// written=0 when the edge loop starts, so the check fires immediately.
func TestWriteCtx_ContextCancelled(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_, err := WriteCtx(ctx, &buf, a, Options{})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// TestWriteCtx_HasHeader covers the opts.HasHeader branch in WriteCtx.
func TestWriteCtx_HasHeader(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("alice", "bob", 42); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	n, err := WriteCtx(context.Background(), &buf, a, Options{HasHeader: true})
	if err != nil {
		t.Fatalf("WriteCtx with header: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 edge written, got %d", n)
	}
	if !strings.Contains(buf.String(), "src,dst,weight") {
		t.Errorf("expected header in output: %q", buf.String())
	}
}
