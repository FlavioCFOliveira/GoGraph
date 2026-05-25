package generation

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPublisher_Close_DrainsPendingReaders(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(t, 1))

	const readers = 100
	started := make(chan struct{}, readers)
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			g := p.Acquire()
			if g == nil {
				return
			}
			started <- struct{}{}
			// Simulate doing some work with the snapshot.
			time.Sleep(10 * time.Millisecond)
			p.Release(g)
		}()
	}

	// Wait for all readers to acquire before closing.
	for i := 0; i < readers; i++ {
		<-started
	}

	// Close must drain all readers within the deadline.
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()

	select {
	case <-done:
		// Good: Close returned.
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s after readers released")
	}
	wg.Wait()
}

func TestPublisher_Close_AcquireReturnsNil(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(t, 1))
	p.Close()
	if g := p.Acquire(); g != nil {
		t.Errorf("Acquire after Close returned non-nil generation")
		p.Release(g)
	}
}

func TestPublisher_Close_PublishReturnsErrClosed(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(t, 1))
	p.Close()
	_, err := p.Publish(makeCSR(t, 2))
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close: got %v, want ErrClosed", err)
	}
}

func TestPublisher_Close_Idempotent(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(t, 1))
	p.Close()
	p.Close() // second call must not panic
}
