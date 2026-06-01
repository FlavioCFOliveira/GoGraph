package dot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestWrite_QuoteWithDoubleQuote covers the '\' branch in quote().
func TestWrite_QuoteWithSpecialChars(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(`has"quote`, `has\backslash`, 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, a); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `\"`) {
		t.Errorf("expected escaped double-quote in output: %q", out)
	}
}

// TestIsSimpleID_Empty covers the empty-string fast path in isSimpleID.
func TestIsSimpleID_Empty(t *testing.T) {
	t.Parallel()
	if isSimpleID("") {
		t.Error("isSimpleID(\"\") should return false")
	}
}

// TestIsSimpleID_StartsWithDigit covers the digit-at-position-0 rejection.
func TestIsSimpleID_StartsWithDigit(t *testing.T) {
	t.Parallel()
	if isSimpleID("1node") {
		t.Error("isSimpleID(\"1node\") should return false")
	}
}

// TestWriteCtx_ContextCancelled covers the ctx.Err() path in WriteCtx.
func TestWriteCtx_ContextCancelled(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	// Add enough nodes to ensure the loop runs at least once.
	for i := range 5 {
		src := string(rune('a' + i))
		dst := string(rune('a' + (i+1)%5))
		if err := a.AddEdge(src, dst, int64(i)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the loop hits ctx.Err() on first iteration
	var buf bytes.Buffer
	err := WriteCtx(ctx, &buf, a)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
