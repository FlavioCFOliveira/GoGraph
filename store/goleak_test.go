package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
// Per CLAUDE.md: every package that spawns goroutines (here, the
// background checkpointer the composed DB owns) must integrate
// go.uber.org/goleak. The whole point of store.DB.Close is that the
// checkpoint goroutine is stopped before the WAL is closed; this gate
// turns "the goroutine leaked past Close" into a hard test failure.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
