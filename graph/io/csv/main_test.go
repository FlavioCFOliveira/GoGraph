package csv_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wraps the test binary's entry point so goleak can verify
// that no goroutines are leaked across the whole test suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
