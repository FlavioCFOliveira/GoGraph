package index_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"gograph/graph"
	"gograph/graph/index"
)

// latchSubscriber counts received events via an atomic so it is safe
// for concurrent Apply calls. snapCount captures the count at a
// specific point in time for post-unregistration verification.
type latchSubscriber struct {
	count atomic.Int64
}

func (s *latchSubscriber) Kind() string         { return "latch" }
func (s *latchSubscriber) Apply(_ index.Change) { s.count.Add(1) }

func TestManager_SubscriberRace(t *testing.T) {
	t.Parallel()

	const (
		writerIter   = 5000
		raceRoutines = 32
	)

	m := index.NewManager()

	var wg sync.WaitGroup

	// Writer goroutine: streams writerIter events.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writerIter; i++ {
			m.Apply(index.Change{
				Op:   index.OpAddNodeLabel,
				Node: graph.NodeID(uint64(i)),
			})
		}
	}()

	// Register/apply/unregister goroutines: each uses a unique name to
	// avoid ErrIndexExists on concurrent CreateIndex calls.
	wg.Add(raceRoutines)
	for g := 0; g < raceRoutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("sub_%d", g)
			sub := &latchSubscriber{}

			if err := m.CreateIndex(name, sub); err != nil {
				// Name collision should never happen because each
				// goroutine uses a unique index.
				t.Errorf("CreateIndex %s: %v", name, err)
				return
			}

			// Apply exactly one event through the manager so this
			// subscriber is definitely reached while registered.
			m.Apply(index.Change{
				Op:   index.OpAddNodeLabel,
				Node: graph.NodeID(uint64(g + 1_000_000)),
			})

			// Capture count before unregistration.
			countBeforeDrop := sub.count.Load()

			if err := m.DropIndex(name); err != nil {
				t.Errorf("DropIndex %s: %v", name, err)
				return
			}

			// After DropIndex returns, the Manager holds no reference
			// to sub. Any Apply that started before DropIndex and has
			// not yet delivered will have completed under the Manager's
			// read-lock, which DropIndex's write-lock cannot have
			// acquired until all such deliveries finished. Therefore
			// sub.count must not grow after this point.
			countAfterDrop := sub.count.Load()
			if countAfterDrop < countBeforeDrop {
				// Sanity: count must be monotonically non-decreasing.
				t.Errorf("sub_%d count went backwards: before=%d after=%d",
					g, countBeforeDrop, countAfterDrop)
			}

			// Drain any in-flight Apply calls that acquired the read
			// lock before DropIndex's write lock: they will have
			// completed by the time DropIndex returned (write lock
			// excluded all readers). After this, count is stable.
			finalCount := sub.count.Load()
			if finalCount != countAfterDrop {
				// countAfterDrop was read after DropIndex returned, so
				// no further Apply can reach sub. If this fires the
				// Manager's locking invariant is broken.
				t.Errorf("sub_%d count changed after DropIndex: %d → %d",
					g, countAfterDrop, finalCount)
			}
		}()
	}

	wg.Wait()
}
