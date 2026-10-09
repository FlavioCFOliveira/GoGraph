package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// BenchmarkDirectWriteUncontended measures direct Go-API node writes with no
// transaction in flight, which is the cost the direct-write conflict test of
// rmp #2947 adds to every such call: one head read under a lock the write
// already holds, and one lock-free load of the node-life gate.
//
// Each op is a real change, so every iteration records a version and takes the
// conflict test; the pairs keep the stored state bounded across iterations.
func BenchmarkDirectWriteUncontended(b *testing.B) {
	newGraph := func(b *testing.B) *Graph[string, float64] {
		b.Helper()
		g := New[string, float64](adjlist.Config{})
		b.Cleanup(func() { _ = g.Close() })
		if err := g.AddNode("n"); err != nil {
			b.Fatal(err)
		}
		return g
	}
	b.Run("set-property", func(b *testing.B) {
		g := newGraph(b)
		vals := [2]PropertyValue{StringValue("a"), StringValue("b")}
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			if err := g.SetNodeProperty("n", "k", vals[i&1]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("set-delete-property", func(b *testing.B) {
		g := newGraph(b)
		v := StringValue("a")
		b.ReportAllocs()
		for b.Loop() {
			if err := g.SetNodeProperty("n", "k", v); err != nil {
				b.Fatal(err)
			}
			if err := g.DelNodeProperty("n", "k"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("add-remove-label", func(b *testing.B) {
		g := newGraph(b)
		b.ReportAllocs()
		for b.Loop() {
			if err := g.SetNodeLabel("n", "L"); err != nil {
				b.Fatal(err)
			}
			if err := g.RemoveNodeLabel("n", "L"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkDirectEdgeWriteUncontended measures direct Go-API edge writes with
// no transaction in flight: the cost the direct-write conflict test of rmp #2947
// adds to the adjacency path — one version-head load per entry written, the
// endpoints' life gate. Each add is paired with its removal so the stored state stays
// bounded.
func BenchmarkDirectEdgeWriteUncontended(b *testing.B) {
	{
		const shape = "directed"
		b.Run(shape+"/add-remove", func(b *testing.B) {
			g := New[string, float64](adjlist.Config{})
			b.Cleanup(func() { _ = g.Close() })
			for _, n := range []string{"a", "b"} {
				if err := g.AddNode(n); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := g.AddEdge("a", "b", 1); err != nil {
					b.Fatal(err)
				}
				if err := g.RemoveEdge("a", "b"); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(shape+"/set-property", func(b *testing.B) {
			g := New[string, float64](adjlist.Config{})
			b.Cleanup(func() { _ = g.Close() })
			if err := g.AddEdge("a", "b", 1); err != nil {
				b.Fatal(err)
			}
			vals := [2]PropertyValue{StringValue("x"), StringValue("y")}
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				if err := g.SetEdgeProperty("a", "b", "k", vals[i&1]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDirectTx measures the implicit transaction a direct write runs as
// (rmp #2947), apart from any store: "empty" opens and closes one that writes
// nothing, which is what a no-op direct write pays; "one-record" also allocates
// its commit record and publishes it at a fresh instant, which is what a direct
// write that changes anything pays on top of its stores.
func BenchmarkDirectTx(b *testing.B) {
	g := New[string, float64](adjlist.Config{})
	b.Cleanup(func() { _ = g.Close() })
	b.Run("empty", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := g.direct(func(*writeCtx) error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("one-record", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := g.direct(func(tx *writeCtx) error {
				_ = tx.record()
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
