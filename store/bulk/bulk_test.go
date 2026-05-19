package bulk

import (
	"context"
	"path/filepath"
	"testing"

	"gograph/store/csrfile"
)

func TestLoader_AddAndFinalise(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "graph.csr")
	l := New(Options{OutputPath: out, Directed: true})
	l.Add(Edge{Src: "a", Dst: "b", Weight: 1})
	l.AddBatch([]Edge{{Src: "b", Dst: "c", Weight: 2}, {Src: "c", Dst: "a", Weight: 3}})
	if l.Rows() != 3 {
		t.Fatalf("Rows = %d, want 3", l.Rows())
	}
	rows, c, err := l.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if rows != 3 {
		t.Fatalf("Finalise rows = %d, want 3", rows)
	}
	if c.Size() != 3 {
		t.Fatalf("csr Size = %d, want 3", c.Size())
	}
	r, err := csrfile.Open(out)
	if err != nil {
		t.Fatalf("csrfile.Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	if r.Header().NEdges != 3 {
		t.Fatalf("csrfile nEdges = %d, want 3", r.Header().NEdges)
	}
}

func TestLoader_DrainChannel(t *testing.T) {
	t.Parallel()
	l := New(Options{Directed: true})
	ch := make(chan Edge, 4)
	ch <- Edge{Src: "x", Dst: "y", Weight: 0}
	ch <- Edge{Src: "y", Dst: "z", Weight: 0}
	close(ch)
	n, err := l.Drain(context.Background(), ch)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 2 {
		t.Fatalf("Drain = %d, want 2", n)
	}
}

func TestLoader_DrainCancelled(t *testing.T) {
	t.Parallel()
	l := New(Options{Directed: true})
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan Edge)
	cancel()
	if _, err := l.Drain(ctx, ch); err == nil {
		t.Fatalf("expected context error")
	}
}
