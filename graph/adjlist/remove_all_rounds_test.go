package adjlist

// remove_all_rounds_test.go — rmp #2947, audit finding F8: an undirected
// RemoveAllEdgesFrom against concurrent appenders to the same node.
//
// The rejected form locked exactly the shards src's entry named and restarted
// whenever the entry changed before every lock was held, which nothing bounded:
// the audit measured 1985 restarts and 3.17 s for ONE call against 64
// appenders. The locked set now only grows, so a call takes at most shardCount
// rounds whatever the appenders do.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoveAllEdgesFrom_UndirectedRoundsAreBounded(t *testing.T) {
	for _, appenders := range []int{1, 16, 64} {
		t.Run(fmt.Sprintf("appenders=%d", appenders), func(t *testing.T) {
			a := New[string, float64](Config{Directed: false})
			resetRemoveAllRounds(a)
			var stop atomic.Bool
			var wg sync.WaitGroup
			for w := range appenders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; !stop.Load(); i++ {
						x := fmt.Sprintf("x%d_%d", w, i%16)
						_ = a.AddEdge("hub", x, 1)
						_ = a.RemoveEdge("hub", x)
					}
				}()
			}
			var calls int
			var slowest time.Duration
			for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); calls++ {
				start := time.Now()
				if err := a.RemoveAllEdgesFrom("hub"); err != nil {
					t.Errorf("RemoveAllEdgesFrom: %v", err)
				}
				slowest = max(slowest, time.Since(start))
			}
			stop.Store(true)
			wg.Wait()
			rounds := maxRemoveAllRounds(a)
			t.Logf("appenders=%d calls=%d maxRounds=%d slowest=%v", appenders, calls, rounds, slowest)
			if rounds > shardCount {
				t.Errorf("one call took %d rounds; the expanding locked set bounds it at %d", rounds, shardCount)
			}
			if slowest > time.Second {
				t.Errorf("one call took %v", slowest)
			}
		})
	}
}
