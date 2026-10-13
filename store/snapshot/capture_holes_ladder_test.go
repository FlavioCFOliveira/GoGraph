package snapshot

// capture_holes_ladder_test.go — WAL v2 step 1 (docs/design-wal-v2.md §9): a
// capture at an MVCC instant succeeds and loads under concurrent interning,
// commits and aborts at every published concurrency level.
//
// Layer: short.

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestCapture_ConcurrentInternCommitAbort_Ladder runs goroutines that create
// nodes, committing "c-" keys and rolling back "a-" keys, while captures are
// taken through a capture read. Every capture must succeed; its mapper.bin must
// load with its nodeids.bin; it must carry every key committed before the capture
// began and no rolled-back key, alive or as a tombstone.
func TestCapture_ConcurrentInternCommitAbort_Ladder(t *testing.T) {
	t.Parallel()
	const totalOps, captures = 2048, 4
	for _, level := range []int{1, 8, 64, 256, 1024} {
		t.Run(fmt.Sprintf("goroutines=%d", level), func(t *testing.T) {
			t.Parallel()
			g := lpg.New[string, float64](adjlist.Config{})
			defer func() { _ = g.Close() }()
			perWorker := max(totalOps/level, 4)
			var committed sync.Map // key -> struct{}, stored after the commit returns
			var wg sync.WaitGroup
			var started atomic.Int64
			for w := range level {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range perWorker {
						started.Add(1)
						if (w+i)%3 == 0 {
							tx := g.BeginVersionedTx()
							if err := g.Writer(tx).AddNode(fmt.Sprintf("a-%d-%d", w, i)); err != nil {
								t.Error(err)
							}
							tx.Abandon()
							g.EndVersionedTx(tx)
							continue
						}
						k := fmt.Sprintf("c-%d-%d", w, i)
						if err := g.ApplyVersioned(func(tx lpg.WriteTx) error { return g.Writer(tx).AddNode(k) }); err != nil {
							t.Error(err)
							continue
						}
						committed.Store(k, struct{}{})
					}
				}()
			}
			for c := range captures {
				// The keys committed before the capture's instant: read before it.
				var before []string
				committed.Range(func(k, _ any) bool { before = append(before, k.(string)); return true })
				at := g.BeginCaptureRead()
				cs := csr.BuildFromAdjListAsOf(g.AdjList(),
					func(id graph.NodeID) bool { return g.NodeExistsAsOf(id, at) }, at.StartTS(), at.TxID())
				capt, err := CaptureGraph[string, float64](g, cs, nil, at)
				g.EndRead(at)
				if err != nil {
					t.Fatalf("capture %d: %v", c, err)
				}
				rb, err := ReadMapperString(bytes.NewReader(capt.mapper.bytes))
				if err != nil {
					t.Fatalf("capture %d: ReadMapperString: %v", c, err)
				}
				next, err := ReadNodeIDs(bytes.NewReader(capt.nodeIDs.bytes))
				if err != nil {
					t.Fatalf("capture %d: ReadNodeIDs: %v", c, err)
				}
				entries := make([]graph.MapperEntry[string], 0, len(rb.Pairs))
				carried := make(map[string]bool, len(rb.Pairs))
				for _, p := range rb.Pairs {
					if strings.HasPrefix(p.Key, "a-") {
						t.Errorf("capture %d carries rolled-back key %q (node %d)", c, p.Key, uint64(p.ID))
					}
					carried[p.Key] = true
					entries = append(entries, graph.MapperEntry[string]{ID: p.ID, Key: p.Key})
				}
				if err := graph.NewMapper[string]().LoadFrom(entries, next); err != nil {
					t.Fatalf("capture %d does not load: %v", c, err)
				}
				for _, k := range before {
					if !carried[k] {
						t.Errorf("capture %d lacks %q, committed before its instant", c, k)
					}
				}
			}
			wg.Wait()
			if started.Load() == 0 {
				t.Fatal("no writer ran")
			}
		})
	}
}
