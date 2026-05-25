package lpg_test

import (
	"fmt"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestLPG_Concurrent fans out 16 goroutines, each writing 10 000
// SetNodeProperty calls to a key range that maps to a distinct mapper
// shard. After all writers finish, the oracle verifies every stored
// value equals Int64Value(int64(key)).
//
// Must pass under -race.
func TestLPG_Concurrent(t *testing.T) {
	defer goleak.VerifyNone(t)

	const (
		numGoroutines = 16
		perGoroutine  = 10_000
	)

	g := lpg.New[int, int64](adjlist.Config{Directed: true})

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := range numGoroutines {
		i := i
		go func() {
			defer wg.Done()
			base := i * perGoroutine
			for j := range perGoroutine {
				key := base + j
				if err := g.AddNode(key); err != nil {
					// AddNode on an unbounded graph never fails; surface
					// any unexpected error via panic so -race output is
					// preserved.
					panic(fmt.Sprintf("AddNode(%d): %v", key, err))
				}
				if err := g.SetNodeProperty(key, "val", lpg.Int64Value(int64(key))); err != nil {
					panic(fmt.Sprintf("SetNodeProperty(%d): %v", key, err))
				}
			}
		}()
	}

	wg.Wait()

	// Oracle: every key must carry "val" == Int64Value(int64(key)).
	for i := range numGoroutines {
		base := i * perGoroutine
		for j := range perGoroutine {
			key := base + j
			v, ok := g.GetNodeProperty(key, "val")
			if !ok {
				t.Errorf("key %d: property \"val\" missing", key)
				continue
			}
			got, ok2 := v.Int64()
			if !ok2 {
				t.Errorf("key %d: expected PropInt64, got kind %v", key, v.Kind())
				continue
			}
			if got != int64(key) {
				t.Errorf("key %d: got %d, want %d", key, got, key)
			}
		}
	}
}
