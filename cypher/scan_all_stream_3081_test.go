package cypher

// scan_all_stream_3081_test.go — rmp #3081: on the read path at a pinned
// snapshot AllNodesScan streams its ids instead of materialising them in Init.
// These tests pin that the streamed scan emits EXACTLY the snapshot's node set,
// in the materialising scan's order, while nodes are inserted and deleted
// concurrently mid-drain. Layer: short.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func drainScanIDs(t *testing.T, op *exec.AllNodesScan, limit int) []graph.NodeID {
	t.Helper()
	var out []graph.NodeID
	var row exec.Row
	for limit < 0 || len(out) < limit {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		out = append(out, graph.NodeID(row[0].(expr.IntegerValue)))
	}
	return out
}

func TestAllNodesScan_StreamsExactSnapshotUnderConcurrentWrites_3081(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	const base = 3000
	for i := 0; i < base; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	snap := g.BeginRead()
	if snap == nil {
		t.Fatal("BeginRead returned nil: MVCC is not armed, the streamed path is unreachable")
	}
	defer g.EndRead(snap)
	rv := g.ReadAt(snap)

	// The reference: the materialising scan at the same snapshot, before any
	// concurrent write.
	ref := exec.NewAllNodesScan(&lpgNodeWalker{g: rv})
	if err := ref.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := drainScanIDs(t, ref, -1)
	if len(want) != base {
		t.Fatalf("reference scan = %d ids, want %d", len(want), base)
	}

	w := &lpgNodeWalker{g: rv, stream: true}
	if !w.StreamsNodeIDs() {
		t.Fatal("a read walker at a pinned snapshot does not stream")
	}
	if (&lpgNodeWalker{g: rv}).StreamsNodeIDs() || (&lpgNodeWalker{g: g.ReadAt(nil), stream: true}).StreamsNodeIDs() {
		t.Fatal("a non-read or snapshot-less walker streams")
	}
	op := exec.NewAllNodesScan(w)
	if err := op.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := drainScanIDs(t, op, 700)

	// Concurrent writers: delete nodes the cursor has not reached and nodes it
	// has passed, and insert new nodes into every shard, while the scan drains.
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < base; i += 3 {
			_ = g.RemoveNode(fmt.Sprintf("n%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2000; i++ {
			_ = g.AddNode(fmt.Sprintf("new%d", i))
		}
	}()
	close(start)
	got = append(got, drainScanIDs(t, op, 500)...)
	wg.Wait()
	got = append(got, drainScanIDs(t, op, -1)...)
	_ = op.Close()

	if len(got) != len(want) {
		t.Fatalf("streamed scan = %d ids, want the snapshot's %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("streamed id[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	// The writes did land: a fresh snapshot sees a different node set.
	snap2 := g.BeginRead()
	defer g.EndRead(snap2)
	fresh := exec.NewAllNodesScan(&lpgNodeWalker{g: g.ReadAt(snap2), stream: true})
	if err := fresh.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, wantN := len(drainScanIDs(t, fresh, -1)), base-base/3+2000; n != wantN {
		t.Fatalf("fresh snapshot scan = %d ids, want %d (the concurrent writes did not land)", n, wantN)
	}
}

// TestAllNodesScan_EngineReadTxStableUnderWrites_3081 drives the same property
// end to end: every statement of an explicit read transaction scans the
// transaction's snapshot, while concurrent writers churn nodes.
func TestAllNodesScan_EngineReadTxStableUnderWrites_3081(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := NewEngine(g)
	ctx := context.Background()
	for i := 0; i < 1500; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	rtx, err := eng.BeginReadTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rtx.Rollback() }()
	ids := func() []string {
		res, err := rtx.Exec("MATCH (n) RETURN id(n) AS i", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Close() }()
		var out []string
		for res.Next() {
			out = append(out, fmt.Sprint(res.Record()["i"]))
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := ids()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1500; i += 2 {
			_ = g.RemoveNode(fmt.Sprintf("n%d", i))
			_ = g.AddNode(fmt.Sprintf("m%d", i))
		}
	}()
	for r := 0; r < 4; r++ {
		got := ids()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("round %d: read transaction saw %d ids, want its snapshot's %d", r, len(got), len(want))
		}
	}
	wg.Wait()
	if got := ids(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after the writers: read transaction saw %d ids, want its snapshot's %d", len(got), len(want))
	}
}
