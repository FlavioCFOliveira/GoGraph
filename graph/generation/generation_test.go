package generation

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func makeCSR(seed int) *csr.CSR[struct{}] {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	a.AddEdge(seed, seed+1, struct{}{})
	return csr.BuildFromAdjList(a)
}

func TestPublisher_AcquireRelease(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(1))
	g := p.Acquire()
	if g == nil {
		t.Fatalf("Acquire returned nil")
	}
	if g.Refcount() != 1 {
		t.Fatalf("Refcount = %d, want 1", g.Refcount())
	}
	p.Release(g)
	if g.Refcount() != 0 {
		t.Fatalf("Refcount after release = %d", g.Refcount())
	}
}

func TestPublisher_Publish(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(1))
	old := p.Current()
	next := p.Publish(makeCSR(2))
	if p.Current() != next {
		t.Fatalf("current did not swap")
	}
	if next == old {
		t.Fatalf("Publish returned same generation")
	}
}

func TestPublisher_PublishWithDrainNoReaders(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(1))
	next, err := p.PublishWithDrain(makeCSR(2), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("PublishWithDrain: %v", err)
	}
	if next != p.Current() {
		t.Fatalf("PublishWithDrain return != current")
	}
}

func TestPublisher_PublishWithDrainBlocksUntilRelease(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(1))
	g := p.Acquire()

	done := make(chan error, 1)
	go func() {
		_, err := p.PublishWithDrain(makeCSR(2), time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("PublishWithDrain returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	p.Release(g)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PublishWithDrain after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("PublishWithDrain did not return after release")
	}
}

func TestPublisher_PublishWithDrainTimeout(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(1))
	g := p.Acquire()
	defer p.Release(g)
	_, err := p.PublishWithDrain(makeCSR(2), 50*time.Millisecond)
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("expected ErrDrainTimeout, got %v", err)
	}
}

func TestPublisher_ConcurrentReadersDuringPublish(t *testing.T) {
	t.Parallel()
	p := New(makeCSR(1))
	const readers = 64
	var wg sync.WaitGroup
	var bad atomic.Int64
	stop := make(chan struct{})
	wg.Add(readers)
	for i := 0; i < readers; i++ {
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
					bad.Add(1)
					continue
				}
				if g.CSR() == nil {
					bad.Add(1)
				}
				p.Release(g)
			}
		}()
	}
	// Publish a few generations while readers are running.
	for i := 0; i < 50; i++ {
		_ = p.Publish(makeCSR(i + 2))
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("observed %d bad acquires", bad.Load())
	}
}
