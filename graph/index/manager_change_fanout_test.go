package index_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"gograph/graph"
	"gograph/graph/index"
)

// countSubscriber counts every Change it receives. Apply is
// safe for concurrent use.
type countSubscriber struct {
	count atomic.Int64
}

func (s *countSubscriber) Kind() string         { return "counter" }
func (s *countSubscriber) Apply(_ index.Change) { s.count.Add(1) }

func TestManager_Fanout(t *testing.T) {
	t.Parallel()

	const (
		numSubs     = 8
		numWorker   = 8
		eventsEach  = 1000
		totalEvents = numWorker * eventsEach
	)

	m := index.NewManager()
	subs := make([]*countSubscriber, numSubs)
	for i := range subs {
		subs[i] = &countSubscriber{}
		if err := m.CreateIndex(fmt.Sprintf("sub_%d", i), subs[i]); err != nil {
			t.Fatalf("CreateIndex sub_%d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(numWorker)
	for w := 0; w < numWorker; w++ {
		w := w
		go func() {
			defer wg.Done()
			for j := 0; j < eventsEach; j++ {
				m.Apply(index.Change{
					Op:   index.OpAddNodeLabel,
					Node: graph.NodeID(uint64(w*eventsEach + j)),
				})
			}
		}()
	}
	wg.Wait()

	for i, sub := range subs {
		if got := sub.count.Load(); got != totalEvents {
			t.Errorf("sub_%d count = %d, want %d", i, got, totalEvents)
		}
	}
}
