package jsonl_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
)

// TestJSONL_CtxCancelMidStream verifies that context cancellation and
// deadline expiry are propagated correctly by the context-aware read
// and write functions.
//
// The jsonl package is fully synchronous — it spawns no goroutines —
// so goroutine-leak detection is not applicable here.
func TestJSONL_CtxCancelMidStream(t *testing.T) {
	t.Parallel()

	t.Run("pre_cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel before any I/O

		in := `{"type":"node","id":"alice"}` + "\n"
		_, _, err := jsonl.ReadIntoCtx(ctx, strings.NewReader(in), adjlist.Config{Directed: true})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		// Build a large stream (50 000 edge records) to ensure the
		// context deadline fires before the reader finishes. The check
		// interval inside ReadIntoCtx is every 4096 rows, so we need
		// more than 4096 rows; 50 000 provides a comfortable margin.
		var sb strings.Builder
		for i := range 50000 {
			fmt.Fprintf(&sb, `{"type":"edge","src":"a%d","dst":"b%d","weight":1}`+"\n", i, i)
		}
		// A 1 ns timeout will expire before the reader can process
		// 4096 rows, triggering the deadline path.
		ctx, cancel := context.WithTimeout(context.Background(), 1)
		defer cancel()

		_, _, err := jsonl.ReadIntoCtx(ctx, strings.NewReader(sb.String()), adjlist.Config{Directed: true})
		if err == nil {
			t.Fatal("expected deadline error, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context deadline/cancel, got %v", err)
		}
	})

	t.Run("write_cancel", func(t *testing.T) {
		t.Parallel()
		// Build a large adjacency list so WriteCtx has enough work to
		// trigger its ctx check (every 4096 records in the edge loop).
		// We add 4096 nodes plus one edge so the first edge-loop ctx
		// check fires at written==4096.
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		for i := range 4096 {
			if err := a.AddNode(fmt.Sprintf("n%d", i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		if err := a.AddEdge("n0", "n1", 0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel before writing

		var buf bytes.Buffer
		_, err := jsonl.WriteCtx(ctx, &buf, a)
		if err == nil {
			t.Fatal("expected context error from WriteCtx, got nil")
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context cancel/deadline, got %v", err)
		}
	})
}
