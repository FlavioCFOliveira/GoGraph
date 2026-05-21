package metrics

import "testing"

// TestCurrent_InitializesFromNil covers the CompareAndSwap branch in current()
// that fires when backendPtr has been reset to nil.
func TestCurrent_InitializesFromNil(t *testing.T) {
	// Reset the global to nil to trigger the lazy-init branch.
	backendPtr.Store(nil)
	b := current()
	if b == nil {
		t.Fatal("expected non-nil backend after current() initializes from nil")
	}
	// Restore the default noop so other tests are not affected.
	SetBackend(nil)
}
