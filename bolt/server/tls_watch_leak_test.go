package server

import (
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestCertReloader_Watch_NoGoroutineLeak verifies that a Watch goroutine
// exits cleanly when its stop channel is closed, leaving no leaked goroutine.
//
// AC3 (T712): no goroutine leak after Watch is stopped.
func TestCertReloader_Watch_NoGoroutineLeak(t *testing.T) {
	// Not parallel: goleak.IgnoreCurrent captures a snapshot at call time;
	// running parallel tests in flight would pollute that snapshot.
	dir := t.TempDir()
	certPath, keyPath := writeTestPair(t, dir, "leak")
	r, err := NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	// Snapshot goroutines that are already live so goleak ignores them.
	opts := []goleak.Option{goleak.IgnoreCurrent()}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(10*time.Millisecond, stop)
	}()

	// Let Watch spin at least one tick.
	time.Sleep(30 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch goroutine did not exit after stop channel was closed")
	}

	// Verify the Watch goroutine is the only additional goroutine started and
	// that it has now exited.
	if err := goleak.Find(opts...); err != nil {
		t.Errorf("goroutine leak after Watch stopped: %v", err)
	}
}
