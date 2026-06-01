package exec_test

// index_stress_test.go — concurrent stress test for IndexBuffer + index.Manager
// (task-276).
//
// 100 writers each enqueue a single OpAddNodeLabel for a unique NodeID and
// commit concurrently. 1000 reader goroutines run simultaneously, reading the
// label bitmap throughout. After all writers have finished, exactly 100 bits
// must be set in the label bitmap for label ID 1.

import (
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

func TestIndexBuffer_ConcurrentStress(t *testing.T) {
	defer goleak.VerifyNone(t)

	mgr := index.NewManager()
	lblIdx := label.NewNodeIndex()
	if err := mgr.CreateIndex("nodes", lblIdx); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	const (
		writers = 100
		readers = 1000
		labelID = uint32(1)
	)

	// Start writers.
	var writerWg sync.WaitGroup
	writerWg.Add(writers)
	for i := 0; i < writers; i++ {
		nodeID := graph.NodeID(i + 1)
		go func() {
			defer writerWg.Done()
			buf := &exec.IndexBuffer{}
			buf.Enqueue(index.Change{
				Op:    index.OpAddNodeLabel,
				Node:  nodeID,
				Label: labelID,
			})
			buf.Commit(mgr)
		}()
	}

	// Start readers that observe the bitmap concurrently with writers.
	// They run until writers are done to maximise contention.
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = lblIdx.Count(labelID)
				}
			}
		}()
	}

	writerWg.Wait()
	close(stop)
	readerWg.Wait()

	// All 100 unique NodeIDs must be present in the bitmap.
	bm := lblIdx.Intersect(labelID)
	got := bm.GetCardinality()
	if got != writers {
		t.Errorf("bitmap cardinality = %d, want %d", got, writers)
	}
	for i := 0; i < writers; i++ {
		nodeID := uint64(i + 1)
		if !bm.Contains(nodeID) {
			t.Errorf("NodeID %d missing from bitmap", nodeID)
		}
	}
}
