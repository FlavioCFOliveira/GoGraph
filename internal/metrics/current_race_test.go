package metrics

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestCurrent_NilSafeUnderConcurrentReset hammers current() from many
// goroutines while another goroutine repeatedly resets backendPtr to
// nil and back to a real backend. A naive current() that dereferences
// backendPtr.Load() after a CompareAndSwap can nil-panic when a
// concurrent Store(nil) lands between the CAS and the final Load. The
// fix re-loads and falls back to a no-op backend instead of
// dereferencing a possibly-nil pointer.
//
// The test asserts current() never panics and always returns a non-nil
// backend. It must run under -race. It is intentionally not parallel:
// it mutates the package-global backendPtr directly and restores the
// no-op default on exit.
func TestCurrent_NilSafeUnderConcurrentReset(t *testing.T) {
	// Restore the default no-op backend for any later test in this
	// package, regardless of how this test ends.
	defer SetBackend(nil)

	const readers = 64
	const iterations = 20000

	var stopResetter atomic.Bool
	var resetterWG sync.WaitGroup
	var readerWG sync.WaitGroup
	var violations atomic.Int64

	// Resetter alternates backendPtr between nil and a real backend as
	// fast as it can, maximising the window the buggy code mis-handles.
	real := newRecording()
	resetterWG.Add(1)
	go func() {
		defer resetterWG.Done()
		toggle := false
		for !stopResetter.Load() {
			if toggle {
				backendPtr.Store(nil)
			} else {
				SetBackend(real)
			}
			toggle = !toggle
		}
	}()

	// Readers call current() in a tight loop and require a non-nil
	// backend on every call. A nil return (or a deref panic in the buggy
	// implementation) fails the test.
	readerWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readerWG.Done()
			for i := 0; i < iterations; i++ {
				if current() == nil {
					violations.Add(1)
				}
				// Exercise the public entry points too; they route
				// through current() and must never panic.
				IncCounter("metrics.test.current_race", 1)
			}
		}()
	}

	readerWG.Wait()
	stopResetter.Store(true)
	resetterWG.Wait()

	if v := violations.Load(); v != 0 {
		t.Fatalf("current() returned nil %d times under concurrent reset", v)
	}
}
