package isolationtest_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain asserts the harness leaks no goroutines.
//
// It matters more here than in most packages: this harness starts one goroutine
// per session per permutation — 20 permutations × 2 sessions for each short-layer
// spec, and 4200 × 3 for the soak one — and it deliberately parks some of them
// on a channel to exercise blocking detection. A missed close would therefore
// not leak one goroutine but thousands, and it would leak them in the package
// whose entire claim is that it is a trustworthy instrument.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
