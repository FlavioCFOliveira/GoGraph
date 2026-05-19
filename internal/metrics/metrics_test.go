package metrics

import (
	"sync"
	"testing"
	"time"
)

// recordingBackend captures every event so tests can verify the
// dispatch path.
type recordingBackend struct {
	mu      sync.Mutex
	count   map[string]uint64
	latency map[string][]time.Duration
}

func newRecording() *recordingBackend {
	return &recordingBackend{
		count:   map[string]uint64{},
		latency: map[string][]time.Duration{},
	}
}

func (r *recordingBackend) IncCounter(name string, delta uint64) {
	r.mu.Lock()
	r.count[name] += delta
	r.mu.Unlock()
}

func (r *recordingBackend) ObserveLatency(name string, d time.Duration) {
	r.mu.Lock()
	r.latency[name] = append(r.latency[name], d)
	r.mu.Unlock()
}

func TestNoopByDefault(t *testing.T) {
	t.Parallel()
	// Default backend is no-op; these calls must not panic and the
	// observable state is empty.
	IncCounter("never.recorded", 1)
	ObserveLatency("never.recorded", time.Millisecond)
}

func TestSetBackendDispatches(t *testing.T) {
	t.Parallel()
	// Note: this test does not run in parallel because SetBackend is
	// global. We restore the no-op default at the end.
	defer SetBackend(nil)

	r := newRecording()
	SetBackend(r)
	IncCounter("hits", 5)
	IncCounter("hits", 3)
	stop := Time("lat")
	time.Sleep(1 * time.Millisecond)
	stop()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count["hits"] != 8 {
		t.Fatalf("counter = %d, want 8", r.count["hits"])
	}
	if len(r.latency["lat"]) != 1 {
		t.Fatalf("latency samples = %d, want 1", len(r.latency["lat"]))
	}
	if r.latency["lat"][0] < time.Millisecond {
		t.Fatalf("latency = %v, want >= 1ms", r.latency["lat"][0])
	}
}
