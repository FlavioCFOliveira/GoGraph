package lpg

// mvcc_abort_life_restore_test.go — an abort restores a node's previous life
// exactly (ACID audit round 6, finding C2): a revival of a committed-dead node
// returns it to dead and born, a removal of a dead node changes nothing, and
// only a key whose first existence aborted is left unborn.
//
// Layer: short.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

var errInjectedDurable = errors.New("injected durable-step failure")

// abortedDurable runs apply as a bounded transaction whose durable step fails,
// so every version it wrote is withdrawn by the abort.
func abortedDurable(t *testing.T, g *Graph[string, float64], apply func(WriteView[string, float64]) error) {
	t.Helper()
	err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error { return apply(g.Writer(wtx)) },
		func() error { return errInjectedDurable })
	if !errors.Is(err, errInjectedDurable) {
		t.Fatalf("aborted transaction returned %v, want the injected failure", err)
	}
}

func committedDurable(t *testing.T, g *Graph[string, float64], apply func(WriteView[string, float64]) error) {
	t.Helper()
	if err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error { return apply(g.Writer(wtx)) },
		func() error { return nil }); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestAbortLifeRestore(t *testing.T) {
	type state struct{ alive, unborn bool }
	cases := []struct {
		name    string
		setup   func(w WriteView[string, float64]) error
		aborted func(w WriteView[string, float64]) error
		want    state
	}{
		{
			name:    "aborted first creation leaves the key unborn",
			setup:   func(w WriteView[string, float64]) error { return w.AddNode("other") },
			aborted: func(w WriteView[string, float64]) error { return w.AddNode("x") },
			want:    state{alive: false, unborn: true},
		},
		{
			name: "aborted revival of a committed-dead node leaves it dead and born",
			setup: func(w WriteView[string, float64]) error {
				if err := w.AddNode("x"); err != nil {
					return err
				}
				_, _ = w.RemoveNode("x")
				return nil
			},
			aborted: func(w WriteView[string, float64]) error { return w.AddNode("x") },
			want:    state{alive: false, unborn: false},
		},
		{
			name: "aborted removal of a committed-dead node leaves it dead",
			setup: func(w WriteView[string, float64]) error {
				if err := w.AddNode("x"); err != nil {
					return err
				}
				_, _ = w.RemoveNode("x")
				return nil
			},
			aborted: func(w WriteView[string, float64]) error { _, _ = w.RemoveNode("x"); return nil },
			want:    state{alive: false, unborn: false},
		},
		{
			name:  "aborted revive-then-remove of a committed-dead node leaves it dead and born",
			setup: func(w WriteView[string, float64]) error { _ = w.AddNode("x"); _, _ = w.RemoveNode("x"); return nil },
			aborted: func(w WriteView[string, float64]) error {
				if err := w.AddNode("x"); err != nil {
					return err
				}
				_, _ = w.RemoveNode("x")
				return nil
			},
			want: state{alive: false, unborn: false},
		},
		{
			name:  "aborted create-remove-create of a new key leaves it unborn",
			setup: func(w WriteView[string, float64]) error { return w.AddNode("other") },
			aborted: func(w WriteView[string, float64]) error {
				_ = w.AddNode("x")
				_, _ = w.RemoveNode("x")
				return w.AddNode("x")
			},
			want: state{alive: false, unborn: true},
		},
		{
			name:    "aborted removal of a live node leaves it alive",
			setup:   func(w WriteView[string, float64]) error { return w.AddNode("x") },
			aborted: func(w WriteView[string, float64]) error { _, _ = w.RemoveNode("x"); return nil },
			want:    state{alive: true, unborn: false},
		},
		{
			name:    "aborted property write creating a key leaves it unborn",
			setup:   func(w WriteView[string, float64]) error { return w.AddNode("other") },
			aborted: func(w WriteView[string, float64]) error { return w.SetNodeProperty("x", "p", Int64Value(1)) },
			want:    state{alive: false, unborn: true},
		},
		{
			name:    "aborted label write creating a key leaves it unborn",
			setup:   func(w WriteView[string, float64]) error { return w.AddNode("other") },
			aborted: func(w WriteView[string, float64]) error { return w.SetNodeLabel("x", "L") },
			want:    state{alive: false, unborn: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			committedDurable(t, g, c.setup)
			abortedDurable(t, g, c.aborted)
			id, ok := g.adj.Mapper().Lookup("x")
			if !ok {
				t.Fatal("x was never interned")
			}
			got := state{alive: !g.IsTombstoned(id), unborn: g.isUnborn(id)}
			if got != c.want {
				t.Errorf("after the abort: alive=%v unborn=%v, want alive=%v unborn=%v", got.alive, got.unborn, c.want.alive, c.want.unborn)
			}
			if n := g.NodeLifeVersionCount(); n < 0 {
				t.Errorf("life record gate went negative: %d", n)
			}
		})
	}
}

// TestAbortLifeRestore_RevivalOfUnbornStaysUnborn: a key whose first creation
// aborted, revived by a second transaction that also aborts, still never
// existed.
func TestAbortLifeRestore_RevivalOfUnbornStaysUnborn(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	abortedDurable(t, g, func(w WriteView[string, float64]) error { return w.AddNode("x") })
	abortedDurable(t, g, func(w WriteView[string, float64]) error { return w.AddNode("x") })
	id, _ := g.adj.Mapper().Lookup("x")
	if !g.IsTombstoned(id) || !g.isUnborn(id) {
		t.Errorf("alive=%v unborn=%v, want dead and unborn", !g.IsTombstoned(id), g.isUnborn(id))
	}
}
