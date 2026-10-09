package lpg

// mvcc_born_asof_test.go — WAL v2 step 1 (docs/design-wal-v2.md §3.2, risk 6):
// [Graph.NodeBornAsOf] must never report an aborted first creation as born.
//
// The abort withdraws the creation's birth record under the life-shard lock and
// used to mark the id unborn only afterwards, outside the lock: in between, a
// reader found no record and no unborn mark — the shape of a node whose birth
// was reclaimed long ago — and answered "born". A capture then wrote the id as a
// tombstone instead of a hole. The window is a few instructions wide, so this is
// a targeted stress: writers create and roll back fresh keys while readers,
// pinned before every creation, poll them.
//
// Layer: short.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestNodeBornAsOf_AbortedCreationNeverReadsBorn(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()
	if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode("seed") }); err != nil {
		t.Fatal(err)
	}
	at := g.BeginRead()
	defer g.EndRead(at)

	const writers, readers = 8, 4
	var (
		recent [256]atomic.Uint64 // ids of in-flight aborts, published before the abort
		stop   atomic.Bool
		wrong  atomic.Int64
		checks atomic.Int64
		wg     sync.WaitGroup
	)
	deadline := time.Now().Add(300 * time.Millisecond)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				tx := g.BeginVersionedTx()
				key := fmt.Sprintf("w%d-%d", w, i)
				if err := g.Writer(tx).AddNode(key); err != nil {
					t.Error(err)
				}
				id, _ := g.AdjList().Mapper().Lookup(key)
				recent[(w*31+i)%len(recent)].Store(uint64(id) + 1)
				tx.Abandon()
				g.EndVersionedTx(tx)
			}
		}()
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for i := range recent {
					v := recent[i].Load()
					if v == 0 {
						continue
					}
					checks.Add(1)
					if g.NodeBornAsOf(graph.NodeID(v-1), at) {
						wrong.Add(1)
					}
				}
			}
		}()
	}
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	if checks.Load() == 0 {
		t.Fatal("no NodeBornAsOf check ran: the stress exercised nothing")
	}
	if n := wrong.Load(); n != 0 {
		t.Fatalf("%d of %d reads reported an aborted, never-committed creation as born at an instant before it", n, checks.Load())
	}
}

// TestNodeBornAsOf_Cases pins the predicate's branches: a committed creation is
// born, a creation committed after the instant is not, an aborted one is not, and
// a node removed before the instant is still born (dead, not a hole).
func TestNodeBornAsOf_Cases(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{})
	defer func() { _ = g.Close() }()
	add := func(k string) {
		t.Helper()
		if err := g.ApplyVersioned(func(tx WriteTx) error { return g.Writer(tx).AddNode(k) }); err != nil {
			t.Fatal(err)
		}
	}
	id := func(k string) graph.NodeID {
		t.Helper()
		v, ok := g.AdjList().Mapper().Lookup(k)
		if !ok {
			t.Fatalf("%q not interned", k)
		}
		return v
	}
	add("old")
	add("removed")
	if err := g.ApplyVersioned(func(tx WriteTx) error { _, err := g.Writer(tx).RemoveNode("removed"); return err }); err != nil {
		t.Fatal(err)
	}
	at := g.BeginRead()
	defer g.EndRead(at)
	add("later")
	tx := g.BeginVersionedTx()
	if err := g.Writer(tx).AddNode("aborted"); err != nil {
		t.Fatal(err)
	}
	tx.Abandon()
	g.EndVersionedTx(tx)
	for _, c := range []struct {
		key  string
		want bool
	}{{"old", true}, {"removed", true}, {"later", false}, {"aborted", false}} {
		if got := g.NodeBornAsOf(id(c.key), at); got != c.want {
			t.Errorf("NodeBornAsOf(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}
