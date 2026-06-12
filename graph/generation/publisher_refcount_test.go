//go:build soak || nightly

package generation

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublisher_Refcount_RotateUnderLoad rotates the publisher 1000 times
// while 50 readers each hold a generation reference, asserting that
// no reference is invalidated prematurely.
func TestPublisher_Refcount_RotateUnderLoad(t *testing.T) {
	p := New(makeCSR(t, 0))

	const readers = 50
	const rotations = 1000

	var wg sync.WaitGroup
	wg.Add(readers)

	stop := make(chan struct{})
	var violations atomic.Int64

	// Start readers; each reader Acquires, checks the CSR is non-nil,
	// then Releases.
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g := p.Acquire()
				if g == nil {
					continue
				}
				// The CSR must never be nil — that would indicate use-after-free.
				if g.CSR() == nil {
					violations.Add(1)
				}
				// Refcount must be at least 1 while we hold the reference.
				if g.Refcount() < 1 {
					violations.Add(1)
				}
				time.Sleep(time.Microsecond)
				p.Release(g)
			}
		}()
	}

	// Rotate 1000 times.
	for i := 0; i < rotations; i++ {
		gen, err := p.Publish(makeCSR(t, i+1))
		if err != nil {
			t.Fatalf("Publish(%d): %v", i+1, err)
		}
		_ = gen
	}

	close(stop)
	wg.Wait()

	if n := violations.Load(); n > 0 {
		t.Errorf("observed %d refcount/CSR violations during rotation", n)
	}
	t.Logf("Completed %d rotations with %d concurrent readers", rotations, readers)
}
