package jsonl

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"gograph/graph/adjlist"
)

// TestReadIntoCtx_EmptyLines covers the empty-line skip branch in ReadIntoCtx.
func TestReadIntoCtx_EmptyLines(t *testing.T) {
	t.Parallel()
	// Input has blank lines between records.
	in := `{"type":"node","id":"alice"}

{"type":"node","id":"bob"}

`
	a, _, err := ReadInto(strings.NewReader(in), adjlist.Config{})
	if err != nil {
		t.Fatalf("ReadInto with empty lines: %v", err)
	}
	if _, ok := a.Mapper().Lookup("alice"); !ok {
		t.Error("expected alice node to be present")
	}
	if _, ok := a.Mapper().Lookup("bob"); !ok {
		t.Error("expected bob node to be present")
	}
}

// TestReadIntoCtx_ContextCancelled covers the ctx.Err() path in ReadIntoCtx.
func TestReadIntoCtx_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := `{"type":"node","id":"alice"}` + "\n"
	_, _, err := ReadIntoCtx(ctx, strings.NewReader(in), adjlist.Config{})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// TestWriteCtx_ContextCancelled covers the ctx.Err() path in WriteCtx.
// WriteCtx checks ctx.Err() only when written&0xFFF==0 (multiples of 4096).
// We need exactly 4096 nodes so that written=4096 when the edge loop starts,
// causing the check to fire on the very first edge iteration.
func TestWriteCtx_ContextCancelled(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	const nodeCount = 4096
	for i := range nodeCount {
		if err := a.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	if err := a.AddEdge("n0", "n1", 0); err != nil { // edge loop must run for the ctx check to be reached
		t.Fatalf("AddEdge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_, err := WriteCtx(ctx, &buf, a)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
