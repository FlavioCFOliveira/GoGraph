package lpg

// noop_over_inflight_test.go — every WriteView primitive, run while another
// transaction holds a pending write on the same object that then aborts, must
// leave the state the serial history "peer aborted, this write committed"
// leaves (ACID audit round 6, finding C1).
//
// The store-level matrix (store/txn noop_over_inflight_test.go) covers the ops a
// durable store commit buffers, through the WAL and recovery. This one covers
// the whole WriteView surface the Cypher engine also writes through — revival,
// bulk arc removal, pair-label removal and the by-ordinal stores included — at
// the in-memory level, where a verdict of "nothing to do" taken against the
// peer's uncommitted write shows up as a final state that differs from the
// serial one.
//
// Each cell runs A three ways: as a durable store commit's bounded transaction
// and as a direct write's implicit one, which must wait for B, and as an
// explicit transaction, which B may refuse at once but must never acknowledge a
// verdict B's abort then undoes.
//
// Layer: short for the same-op diagonal and the creation/removal rows; the full
// matrix is soak.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

type wvOp struct {
	name string
	run  func(w WriteView[string, float64]) error
}

const wvHandle, wvFreshHandle = 7, 9

var wvOps = []wvOp{
	{"AddNode", func(w WriteView[string, float64]) error { return w.AddNode("x") }},
	{"RemoveNode", func(w WriteView[string, float64]) error { _, _ = w.RemoveNode("x"); return nil }},
	{"Revive", func(w WriteView[string, float64]) error { return w.Revive("x") }},
	{"SetNodeLabel", func(w WriteView[string, float64]) error { return w.SetNodeLabel("x", "L") }},
	{"RemoveNodeLabel", func(w WriteView[string, float64]) error { return w.RemoveNodeLabel("x", "L") }},
	{"SetNodeProperty", func(w WriteView[string, float64]) error { return w.SetNodeProperty("x", "p", Int64Value(1)) }},
	{"DelNodeProperty", func(w WriteView[string, float64]) error { return w.DelNodeProperty("x", "p") }},
	{"AddEdgeHIfAbsent", func(w WriteView[string, float64]) error {
		_, err := w.AddEdgeHIfAbsent("x", "y", 1, wvHandle)
		return err
	}},
	{"AddEdgeHIfAbsentFresh", func(w WriteView[string, float64]) error {
		_, err := w.AddEdgeHIfAbsent("x", "y", 2, wvFreshHandle)
		return err
	}},
	{"RemoveEdge", func(w WriteView[string, float64]) error { w.RemoveEdge("x", "y"); return nil }},
	{"RemoveEdgeByHandle", func(w WriteView[string, float64]) error { w.RemoveEdgeByHandle("x", "y", wvHandle); return nil }},
	{"RemoveAllEdgesFrom", func(w WriteView[string, float64]) error { w.RemoveAllEdgesFrom("x"); return nil }},
	{"SetEdgeLabel", func(w WriteView[string, float64]) error { return w.SetEdgeLabel("x", "y", "R") }},
	{"RemoveEdgeLabel", func(w WriteView[string, float64]) error { return w.RemoveEdgeLabel("x", "y", "R") }},
	{"SetEdgeProperty", func(w WriteView[string, float64]) error { return w.SetEdgeProperty("x", "y", "e", Int64Value(1)) }},
	{"DelEdgeProperty", func(w WriteView[string, float64]) error { return w.DelEdgeProperty("x", "y", "e") }},
	{"SetEdgeLabelAt", func(w WriteView[string, float64]) error { return w.SetEdgeLabelAt("x", "y", 0, "S") }},
	{"SetEdgePropertyAt", func(w WriteView[string, float64]) error { return w.SetEdgePropertyAt("x", "y", 0, "f", Int64Value(1)) }},
	{"RemoveEdgeInstance", func(w WriteView[string, float64]) error { return w.RemoveEdgeInstance("x", "y", 0) }},
	{"SetEdgeLabelByHandle", func(w WriteView[string, float64]) error { return w.SetEdgeLabelByHandle("x", "y", wvHandle, "R") }},
	{"SetEdgePropertyByHandle", func(w WriteView[string, float64]) error {
		return w.SetEdgePropertyByHandle("x", "y", wvHandle, "q", Int64Value(1))
	}},
	{"DelEdgePropertyByHandle", func(w WriteView[string, float64]) error { return w.DelEdgePropertyByHandle("x", "y", wvHandle, "q") }},
	{"RemoveEdgeInstanceByHandle", func(w WriteView[string, float64]) error { return w.RemoveEdgeInstanceByHandle("x", "y", wvHandle) }},
}

var wvSetups = []struct {
	name string
	run  func(w WriteView[string, float64]) error
}{
	{"empty", func(WriteView[string, float64]) error { return nil }},
	{"x,y alive", func(w WriteView[string, float64]) error {
		if err := w.AddNode("x"); err != nil {
			return err
		}
		return w.AddNode("y")
	}},
	{"x labelled, edge with metadata", func(w WriteView[string, float64]) error {
		for _, err := range []error{
			w.AddNode("x"), w.AddNode("y"),
			w.SetNodeLabel("x", "L"), w.SetNodeProperty("x", "p", Int64Value(1)),
		} {
			if err != nil {
				return err
			}
		}
		if _, err := w.AddEdgeHIfAbsent("x", "y", 1, wvHandle); err != nil {
			return err
		}
		for _, err := range []error{
			w.SetEdgeLabelByHandle("x", "y", wvHandle, "R"),
			w.SetEdgePropertyByHandle("x", "y", wvHandle, "q", Int64Value(1)),
			w.SetEdgeLabel("x", "y", "R"),
			w.SetEdgeProperty("x", "y", "e", Int64Value(1)),
			w.SetEdgeLabelAt("x", "y", 0, "S"),
			w.SetEdgePropertyAt("x", "y", 0, "f", Int64Value(1)),
		} {
			if err != nil {
				return err
			}
		}
		return nil
	}},
	{"x dead", func(w WriteView[string, float64]) error {
		for _, err := range []error{w.AddNode("x"), w.AddNode("y"), w.SetNodeLabel("x", "L"), w.SetNodeProperty("x", "p", Int64Value(1))} {
			if err != nil {
				return err
			}
		}
		_, _ = w.RemoveNode("x")
		return nil
	}},
}

// wvDump renders the committed observable state of x and y.
func wvDump(g *Graph[string, float64]) string {
	var b strings.Builder
	props := func(m map[string]PropertyValue) string {
		ks := make([]string, 0, len(m))
		for k, v := range m {
			ks = append(ks, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	sorted := func(s []string) []string { s = append([]string(nil), s...); sort.Strings(s); return s }
	for _, k := range []string{"x", "y"} {
		id, ok := g.adj.Mapper().Lookup(k)
		alive := ok && !g.IsTombstoned(id)
		fmt.Fprintf(&b, "%s alive=%v", k, alive)
		if !alive {
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, " labels=%v props={%s}\n", sorted(g.NodeLabels(k)), props(g.NodeProperties(k)))
		var cs Snapshot
		v := g.EntryViewAsOf(id, g.latestCommitted(&cs))
		var es []string
		for j, d := range v.Neighbours {
			dk, _ := g.adj.Mapper().Resolve(d)
			var h uint64
			if j < len(v.Handles) {
				h = v.Handles[j]
			}
			e := fmt.Sprintf("  -> %s h=%d", dk, h)
			if h == wvHandle || h == wvFreshHandle {
				e += fmt.Sprintf(" hl=%v hp={%s}", sorted(g.EdgeLabelsByHandle(k, dk, h)), props(g.EdgePropertiesByHandle(k, dk, h)))
			}
			es = append(es, e)
		}
		sort.Strings(es)
		for _, e := range es {
			b.WriteString(e + "\n")
		}
		fmt.Fprintf(&b, "  pair labels=%v props={%s} at0 labels=%v props={%s}\n",
			sorted(g.EdgeLabels(k, "y")), props(g.EdgeProperties(k, "y")),
			sorted(g.EdgeLabelsAt(k, "y", 0)), props(g.EdgePropertiesAt(k, "y", 0)))
	}
	return b.String()
}

var errWVInjected = errors.New("injected durable-step failure")

func wvNew(t *testing.T, setup func(WriteView[string, float64]) error) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{})
	if err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error { return setup(g.Writer(wtx)) }, func() error { return nil }); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return g
}

func wvCommit(g *Graph[string, float64], op wvOp) error {
	return g.ApplyDurable(context.Background(), func(wtx WriteTx) error { return op.run(g.Writer(wtx)) }, func() error { return nil })
}

// wvMode is how A runs: as the bounded transaction of a durable store commit,
// which waits for B; as an explicit transaction, which B refuses at once; or as
// a direct write's implicit transaction, which waits for B.
type wvMode int

const (
	wvBounded wvMode = iota
	wvExplicit
	wvDirect
)

func (m wvMode) String() string { return [...]string{"bounded", "explicit", "direct"}[m] }

// wvRunA runs op as mode prescribes and reports its error and whether it was a
// retryable refusal.
func wvRunA(g *Graph[string, float64], op wvOp, mode wvMode) (refused bool, err error) {
	switch mode {
	case wvExplicit:
		tx := g.BeginVersionedTx()
		err = g.ApplyInVersionedTx(context.Background(), tx, func(w WriteTx) error { return op.run(g.Writer(w)) })
		if err == nil {
			err = tx.Err()
		}
		g.EndVersionedTx(tx)
	case wvDirect:
		err = op.run(g.Writer(WriteTx{}))
	default:
		err = wvCommit(g, op)
	}
	return errors.Is(err, mvcc.ErrSerializationConflict) || errors.Is(err, ErrDirectWriteConflict), err
}

// wvCell runs B pending then aborted under A, and returns a non-empty verdict
// when the result differs from the serial history.
func wvCell(t *testing.T, setup func(WriteView[string, float64]) error, b, a wvOp, mode wvMode, wantDump string, wantErr bool) string {
	t.Helper()
	g := wvNew(t, setup)
	setupDump := wvDump(g)
	inB, releaseB, doneB := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		doneB <- g.ApplyDurable(context.Background(), func(wtx WriteTx) error { return b.run(g.Writer(wtx)) },
			func() error { close(inB); <-releaseB; return errWVInjected })
	}()
	select {
	case <-inB:
	case <-doneB:
		return "" // B refused on its own: nothing is in flight
	}
	go func() { time.Sleep(10 * time.Millisecond); close(releaseB) }()
	refused, errA := wvRunA(g, a, mode)
	<-doneB
	got := wvDump(g)
	switch {
	case refused && mode != wvExplicit:
		return fmt.Sprintf("A was refused (%v) instead of waiting for B", errA)
	case refused:
		if got != setupDump {
			return fmt.Sprintf("refused A changed the state\n--- setup ---\n%s--- got ---\n%s", setupDump, got)
		}
	case (errA != nil) != wantErr:
		return fmt.Sprintf("A returned %v, serial error=%v", errA, wantErr)
	case errA == nil && got != wantDump:
		return fmt.Sprintf("state != serial\n--- serial ---\n%s--- got ---\n%s", wantDump, got)
	case errA != nil && got != setupDump:
		return fmt.Sprintf("refused A changed the state\n--- setup ---\n%s--- got ---\n%s", setupDump, got)
	}
	return ""
}

func wvMatrix(t *testing.T, include func(b, a wvOp) bool) {
	bad := 0
	for _, s := range wvSetups {
		for _, a := range wvOps {
			ref := wvNew(t, s.run)
			errRef := wvCommit(ref, a)
			want := wvDump(ref)
			for _, b := range wvOps {
				if !include(b, a) {
					continue
				}
				for _, mode := range []wvMode{wvBounded, wvExplicit, wvDirect} {
					if v := wvCell(t, s.run, b, a, mode, want, errRef != nil); v != "" {
						bad++
						t.Errorf("setup %q, B=%s pending then aborted, A=%s committed (%v): %s", s.name, b.name, a.name, mode, v)
					}
				}
			}
		}
	}
	if bad > 0 {
		t.Logf("%d diverging cells", bad)
	}
}

// TestWriteViewNoOpOverInFlight_SameOp is every primitive over a pending write
// of the same primitive.
func TestWriteViewNoOpOverInFlight_SameOp(t *testing.T) {
	wvMatrix(t, func(b, a wvOp) bool { return b.name == a.name })
}

// TestWriteViewNoOpOverInFlight_Existence is every primitive over a pending
// creation, revival or removal of its node.
func TestWriteViewNoOpOverInFlight_Existence(t *testing.T) {
	wvMatrix(t, func(b, _ wvOp) bool {
		switch b.name {
		case "AddNode", "RemoveNode", "Revive", "AddEdgeHIfAbsent":
			return true
		}
		return false
	})
}

// TestWriteViewNoOpOverInFlight_Full is the whole matrix. Soak layer.
func TestWriteViewNoOpOverInFlight_Full(t *testing.T) {
	testlayers.RequireSoak(t)
	wvMatrix(t, func(wvOp, wvOp) bool { return true })
}
