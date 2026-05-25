//go:build soak

package generation

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublisher_LoadTest_1000Readers100Writers runs 1000 reader goroutines
// and 100 writer goroutines for 5s, verifying race-detector cleanliness.
func TestPublisher_LoadTest_1000Readers100Writers(t *testing.T) {
	p := New(makeCSR(t, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var publishCount atomic.Int64

	// 100 writers each publishing rebuilt snapshots.
	for w := 0; w < 100; w++ {
		go func(seed int) {
			i := 0
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				c := makeCSR(t, seed+i)
				gen, err := p.Publish(c)
				_ = gen
				if err != nil {
					return
				}
				publishCount.Add(1)
				i++
				runtime.Gosched()
			}
		}(w * 10000)
	}

	// 1000 readers each traversing the published CSR.
	var readerWg sync.WaitGroup
	readerWg.Add(1000)
	for r := 0; r < 1000; r++ {
		go func() {
			defer readerWg.Done()
			var sum int64
			for {
				select {
				case <-ctx.Done():
					_ = sum // prevent optimisation of the accumulation
					return
				default:
				}
				g := p.Acquire()
				if g == nil {
					return
				}
				c := g.CSR()
				if c != nil {
					sum += int64(c.Order())
				}
				p.Release(g)
				runtime.Gosched()
			}
		}()
	}

	readerWg.Wait()
	t.Logf("Total publishes: %d", publishCount.Load())
}
