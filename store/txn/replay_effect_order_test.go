package txn_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// gateValidator parks the first write of the property key "gate" until release
// is closed, so a test can hold a commit inside its in-memory apply — after its
// earlier ops have run and before its WAL append — while another commit runs to
// completion.
type gateValidator struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (v *gateValidator) Validate(key string, _ lpg.PropertyValue) error {
	if key == "gate" {
		v.once.Do(func() {
			close(v.reached)
			<-v.release
		})
	}
	return nil
}

// effectState renders everything the replay-order cases can change about node
// x: liveness, labels, properties and its out-edges to y.
func effectState(g *lpg.Graph[string, float64]) string {
	id, ok := g.AdjList().Mapper().Lookup("x")
	if !ok {
		return "unmapped"
	}
	labels := g.NodeLabels("x")
	sort.Strings(labels)
	props := g.NodeProperties("x")
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pv := ""
	for _, k := range keys {
		pv += fmt.Sprintf("%s=%v;", k, props[k])
	}
	return fmt.Sprintf("tombstoned=%v labels=%v props=%s edge(x,y)=%v",
		g.IsTombstoned(id), labels, pv, g.AdjList().HasEdge("x", "y"))
}

// TestCommit_NoOpOpsAreNotLoggedSoReplayMatchesMemory pins rmp #2965 round 5,
// finding R5-F1. A store commit A applies an op that changes nothing — and so
// holds no claim — and is then held inside its apply while commit B changes the
// same object and logs first. Before the fix A logged its no-op after B's
// record, so replay evaluated it against B's state, where it was no longer a
// no-op, and recovery rebuilt a graph that never existed in memory. The WAL now
// records only effects, so the recovered state equals the acknowledged one, and
// a commit whose every op is a no-op appends nothing at all.
func TestCommit_NoOpOpsAreNotLoggedSoReplayMatchesMemory(t *testing.T) {
	type tx = *txn.Tx[string, float64]
	cases := []struct {
		name string
		// noOpWritesNothing: a is a no-op on the seeded state and writes no
		// version, so committing it alone must append nothing.
		noOpWritesNothing bool
		setup             func(tx)
		a                 func(tx) // the no-op A applies before it parks
		b                 func(tx) // the conflicting change B commits meanwhile
	}{
		{"AddNode(live) vs RemoveNode", true,
			func(s tx) {},
			func(a tx) { _ = a.AddNode("x") },
			func(b tx) { _ = b.RemoveNode("x") }},
		{"SetNodeLabel(present) vs RemoveNodeLabel", true,
			func(s tx) { _ = s.SetNodeLabel("x", "L") },
			func(a tx) { _ = a.SetNodeLabel("x", "L") },
			func(b tx) { _ = b.RemoveNodeLabel("x", "L") }},
		{"SetNodeProperty(same) vs DelNodeProperty", true,
			func(s tx) { _ = s.SetNodeProperty("x", "p", lpg.Int64Value(1)) },
			func(a tx) { _ = a.SetNodeProperty("x", "p", lpg.Int64Value(1)) },
			func(b tx) { _ = b.DelNodeProperty("x", "p") }},
		{"DelNodeProperty(absent) vs SetNodeProperty", true,
			func(s tx) {},
			func(a tx) { _ = a.DelNodeProperty("x", "p") },
			func(b tx) { _ = b.SetNodeProperty("x", "p", lpg.Int64Value(7)) }},
		{"RemoveNodeLabel(absent) vs SetNodeLabel", true,
			func(s tx) {},
			func(a tx) { _ = a.RemoveNodeLabel("x", "L") },
			func(b tx) { _ = b.SetNodeLabel("x", "L") }},
		{"RemoveEdge(absent) vs AddEdge", false,
			func(s tx) { _ = s.AddNode("y") },
			func(a tx) { _ = a.RemoveEdge("x", "y") },
			func(b tx) { _ = b.AddEdge("x", "y", 1) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			g := lpg.New[string, float64](adjlist.Config{})
			opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
			st := txn.NewStoreWithOptions[string, float64](g, w, opts)
			seed := st.Begin()
			_ = seed.AddNode("x")
			_ = seed.AddNode("g")
			c.setup(seed)
			if err := seed.Commit(); err != nil {
				t.Fatal(err)
			}
			if c.noOpWritesNothing {
				// A commit whose every op is a no-op writes no byte to the WAL.
				if err := w.Sync(); err != nil {
					t.Fatal(err)
				}
				before := walSize(t, dir)
				n := st.Begin()
				c.a(n)
				if err := n.Commit(); err != nil {
					t.Fatalf("all-no-op commit: %v", err)
				}
				if err := w.Sync(); err != nil {
					t.Fatal(err)
				}
				if after := walSize(t, dir); after != before {
					t.Errorf("all-no-op commit appended %d WAL bytes, want 0", after-before)
				}
			}
			a := st.Begin()
			c.a(a)
			_ = a.SetNodeProperty("g", "gate", lpg.Int64Value(1)) // A's apply parks here
			// Installed after A buffered its ops, because [txn.Tx.SetNodeProperty]
			// validates at buffer time too; the apply validates again, and parks.
			v := &gateValidator{reached: make(chan struct{}), release: make(chan struct{})}
			g.SetValidator(v)
			doneA := make(chan error, 1)
			go func() { doneA <- a.Commit() }()
			<-v.reached
			b := st.Begin()
			c.b(b)
			// B may be refused when A's no-op still holds a claim (a removal of
			// an absent edge claims the adjacency entry); either outcome must
			// recover to the acknowledged state.
			if err := b.Commit(); err != nil && !errors.Is(err, mvcc.ErrSerializationConflict) {
				close(v.release)
				<-doneA
				t.Fatalf("B commit: %v", err)
			}
			close(v.release)
			if err := <-doneA; err != nil {
				t.Fatalf("A commit: %v", err)
			}
			g.SetValidator(nil)

			mem := effectState(g)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
			if err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if rec := effectState(res.Graph); rec != mem {
				t.Errorf("recovered state differs from the acknowledged state\n memory:    %s\n recovered: %s", mem, rec)
			}
		})
	}
}

// walSize sums the size of every file under dir/wal.
func walSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(filepath.Join(dir, "wal"), func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
