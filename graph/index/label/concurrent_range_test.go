package label_test

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// TestConcurrentRange verifies that concurrent AddRange and RemoveRange
// operations on the same label complete without data races, panics, or
// goroutine leaks, and that the resulting Count is in the valid range
// [0, rangeLen] regardless of the linearisation order.
// Goroutine-leak detection is handled by TestMain via goleak.VerifyTestMain.
func TestConcurrentRange(t *testing.T) {
	t.Parallel()

	const (
		adders   = 8
		removers = 8
		rangeLen = 1000
		lbl      = uint32(0)
	)

	idx := label.NewIndex()
	startCh := make(chan struct{})
	var wg sync.WaitGroup

	for range adders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			idx.AddRange(lbl, 0, graph.NodeID(rangeLen-1))
		}()
	}
	for range removers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			idx.RemoveRange(lbl, 0, graph.NodeID(rangeLen-1))
		}()
	}

	close(startCh)
	wg.Wait()

	c := idx.Count(lbl)
	if c > rangeLen {
		t.Fatalf("Count(%d) = %d after concurrent add/remove, want <= %d", lbl, c, rangeLen)
	}
	// Whatever the final count, Has must be consistent with Scan.
	got := idx.Scan(lbl)
	if uint64(len(got)) != c {
		t.Fatalf("Scan(%d) length = %d, inconsistent with Count = %d", lbl, len(got), c)
	}
}
