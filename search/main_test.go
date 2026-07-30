package search

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies no goroutine leaks at the end of the test run.
//
// This package DOES spawn goroutines: the parallel algorithms in diameter.go,
// floyd_warshall_parallel.go, johnson_parallel.go, triangles_parallel.go and
// wcc_parallel.go each fan out workers. The comment here previously claimed the
// opposite — "the search package does not spawn goroutines itself" — which
// understated what this gate is actually load-bearing for (rmp #2261). It
// verifies that every one of those fan-outs joins its workers before returning.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
