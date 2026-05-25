package search

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// The search package does not spawn goroutines itself; goleak guards
// against accidental leaks in any future change.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
