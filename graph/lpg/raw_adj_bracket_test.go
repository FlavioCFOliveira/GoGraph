package lpg

import (
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// TestRawAdjacencyWrite_NeverJoinsAnExclusiveBracket is rmp #2967: a raw
// [Graph.AdjList] write made with no transaction while an exclusive bracket was
// open resolved its version through the write stamp's ambient slot, so it took
// the bracket's commit record. When the bracket was then doomed, the write had
// been acknowledged and was lost: its version carried the aborted record, so
// every snapshot reader stepped back over it, and the abort withdrawal — which
// visits only the bracket's own write set — never put the entry back. A raw
// write over an entry the bracket had written was not refused either, because
// the conflict test adopted the slot's transaction as the writer's own.
//
// A raw write is now its own single-operation transaction. Over an entry the
// bracket has not written it commits at once and survives the bracket's abort;
// over one the bracket has written it is refused retryably and changes nothing,
// and the abort then leaves the pre-image. Never acknowledged and lost.
func TestRawAdjacencyWrite_NeverJoinsAnExclusiveBracket(t *testing.T) {
	type bracketFn func(t *testing.T, g *Graph[string, float64], body func(WriteTx)) error
	brackets := map[string]bracketFn{
		"ApplyAtomicallyTx": func(_ *testing.T, g *Graph[string, float64], body func(WriteTx)) error {
			return g.ApplyAtomicallyTx(func(tx WriteTx) error {
				body(tx)
				doomForTest(tx)
				return tx.Err()
			})
		},
		"ApplyAtomically": func(_ *testing.T, g *Graph[string, float64], body func(WriteTx)) error {
			return g.ApplyAtomically(func() error {
				tx := g.AmbientWriteTx()
				body(tx)
				doomForTest(tx)
				return nil
			})
		},
		"LockBarrierCtx": func(t *testing.T, g *Graph[string, float64], body func(WriteTx)) error {
			if err := g.LockBarrierCtx(t.Context()); err != nil {
				t.Fatalf("LockBarrierCtx: %v", err)
			}
			tx := g.AmbientWriteTx()
			body(tx)
			doomForTest(tx)
			err := tx.Err()
			g.UnlockBarrier()
			return err
		},
	}
	keys := []string{"a", "b", "c", "d", "p", "q", "x"}
	seed := func(t *testing.T, directed bool) *Graph[string, float64] {
		g := New[string, float64](adjlist.Config{Directed: directed, Multigraph: true})
		t.Cleanup(func() { _ = g.Close() })
		for _, err := range []error{
			g.AddEdge("a", "b", 1), g.AddEdge("p", "b", 2),
			g.AddNode("c"), g.AddNode("d"), g.AddNode("q"),
			g.SetNodeProperty("x", "v", Int64Value(0)),
		} {
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		return g
	}
	for _, directed := range []bool{true, false} {
		for bn, bracket := range brackets {
			name := fmt.Sprintf("directed=%v/%s", directed, bn)

			// Over entries the bracket never wrote: the raw writes commit at
			// once and survive the bracket's abort.
			t.Run(name+"/untouched-entry-survives", func(t *testing.T) {
				g := seed(t, directed)
				ref := seed(t, directed)
				var addErr, delErr error
				err := bracket(t, g, func(tx WriteTx) {
					if err := g.Writer(tx).SetNodeProperty("x", "v", Int64Value(1)); err != nil {
						t.Fatalf("bracket write: %v", err)
					}
					addErr = g.AdjList().AddEdge("q", "c", 5)
					delErr = g.AdjList().RemoveEdge("p", "b")
				})
				if !errors.Is(err, mvcc.ErrSerializationConflict) {
					t.Fatalf("the doomed bracket returned %v, want a serialization conflict", err)
				}
				if addErr != nil || delErr != nil {
					t.Fatalf("raw writes over untouched entries were refused: add=%v remove=%v", addErr, delErr)
				}
				if err := ref.AdjList().AddEdge("q", "c", 5); err != nil {
					t.Fatalf("reference add: %v", err)
				}
				if err := ref.AdjList().RemoveEdge("p", "b"); err != nil {
					t.Fatalf("reference remove: %v", err)
				}
				if got, want := adjAbortState(g, keys...), adjAbortState(ref, keys...); got != want {
					t.Errorf("an acknowledged raw write was lost with the bracket's abort\ngot:\n%swant:\n%s", got, want)
				}
				requireAdjInvariants(t, g, "after the abort")
			})

			// Over an entry the bracket wrote: refused retryably, so the abort
			// leaves the pre-image and nothing acknowledged is lost.
			t.Run(name+"/owned-entry-refuses", func(t *testing.T) {
				g := seed(t, directed)
				before := adjAbortState(g, keys...)
				var rawErr error
				err := bracket(t, g, func(tx WriteTx) {
					if err := g.Writer(tx).AddEdge("a", "c", 3); err != nil {
						t.Fatalf("bracket append: %v", err)
					}
					rawErr = g.AdjList().AddEdge("a", "d", 4)
				})
				if !errors.Is(err, mvcc.ErrSerializationConflict) {
					t.Fatalf("the doomed bracket returned %v, want a serialization conflict", err)
				}
				var c *mvcc.Conflict
				if !errors.As(rawErr, &c) || c.Store != mvcc.StoreAdjacency {
					t.Fatalf("a raw write over the bracket's uncommitted entry returned %v, want a *mvcc.Conflict for the adjacency", rawErr)
				}
				if got := adjAbortState(g, keys...); got != before {
					t.Errorf("the aborted bracket left the adjacency changed\nbefore:\n%safter:\n%s", before, got)
				}
				requireAdjInvariants(t, g, "after the abort")
				// The refusal is retryable: once the bracket has aborted the
				// same raw write applies.
				if err := g.AdjList().AddEdge("a", "d", 4); err != nil {
					t.Fatalf("the raw write after the abort: %v", err)
				}
				snap := g.BeginRead()
				defer g.EndRead(snap)
				if !g.HasEdgeAsOf("a", "d", snap) || g.HasEdgeAsOf("a", "c", snap) {
					t.Errorf("after the retry a fresh snapshot sees a->d=%t a->c=%t, want true false",
						g.HasEdgeAsOf("a", "d", snap), g.HasEdgeAsOf("a", "c", snap))
				}
			})
		}
	}
}

// TestRawAdjacencyWrite_CommitsOutsideTheBracketRecord pins the stamping half of
// rmp #2967 directly: a raw adjacency write made while an exclusive bracket is
// open resolves no version through the ambient slot, so it is visible to a
// reader that begins while the bracket is still open.
func TestRawAdjacencyWrite_CommitsOutsideTheBracketRecord(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.AddNode("m"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddNode("n"); err != nil {
		t.Fatal(err)
	}
	before := g.AmbientVersionResolutions()
	var visibleInside bool
	if err := g.ApplyAtomicallyTx(func(tx WriteTx) error {
		if err := g.Writer(tx).SetNodeProperty("m", "k", Int64Value(1)); err != nil {
			return err
		}
		if err := g.AdjList().AddEdge("m", "n", 1); err != nil {
			return err
		}
		snap := g.BeginRead()
		visibleInside = g.HasEdgeAsOf("m", "n", snap)
		g.EndRead(snap)
		return tx.Err()
	}); err != nil {
		t.Fatalf("bracket: %v", err)
	}
	if !visibleInside {
		t.Error("a raw adjacency write made inside an open bracket is invisible to a reader that began after it: it joined the bracket's record")
	}
	if got := g.AmbientVersionResolutions() - before; got != 0 {
		t.Errorf("%d version(s) resolved their transaction through the ambient slot, want 0", got)
	}
}
