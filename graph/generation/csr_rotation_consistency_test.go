//go:build soak

package generation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublisher_CSR_RotationConsistency runs 32 readers for 5s while
// 1 rotator publishes a new CSR every 10ms, asserting per-iteration
// internal consistency.
//
// makeCSR produces a directed graph with exactly 1 edge, so CSR.Size()
// must always equal 1. A value greater than 1 indicates the reader
// observed a torn write across snapshots.
func TestPublisher_CSR_RotationConsistency(t *testing.T) {
	p := New(makeCSR(t, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Rotator: publish a new CSR every 10ms.
	go func() {
		i := 1
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				if _, err := p.Publish(makeCSR(t, i)); err != nil {
					return
				}
				i++
			}
		}
	}()

	// 32 readers: each iteration must observe a self-consistent snapshot.
	var wg sync.WaitGroup
	wg.Add(32)
	var violations atomic.Int64
	for r := 0; r < 32; r++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				g := p.Acquire()
				if g == nil {
					return
				}
				c := g.CSR()
				if c != nil {
					// makeCSR builds a directed graph with exactly 1 edge;
					// CSR.Size() must equal 1 for any valid snapshot.
					if s := c.Size(); s != 1 {
						violations.Add(1)
					}
				}
				p.Release(g)
			}
		}()
	}

	wg.Wait()
	if n := violations.Load(); n > 0 {
		t.Errorf("observed %d consistency violations across snapshot rotations", n)
	}
}
