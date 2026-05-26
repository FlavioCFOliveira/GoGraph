//go:build soak

// Package stress provides soak-layer concurrency stress tests.
// Activated with -tags=soak.
//
// This file provides the TestMain for the soak build: it installs the
// goleak leak-check once for the entire test binary so that any goroutine
// spawned during a soak test that is not cleaned up before the binary exits
// is reported as a leak. The stress-build TestMain (cypher_writes_test.go)
// is compiled only under -tags=stress and is absent from soak builds, so
// this file is the sole TestMain under -tags=soak.
package stress

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// VerifyTestMain is the recommended goleak integration for packages that
	// rely on t.Parallel: it snapshots the goroutine set before and after the
	// full test binary, reporting leaks once rather than per-test.
	// VerifyTestMain never returns; it calls os.Exit internally.
	goleak.VerifyTestMain(m,
		// The test driver occasionally leaves a book-keeping goroutine
		// blocked on a select; it is owned by the Go runtime/testing
		// package and is not a stress-test leak.
		goleak.IgnoreTopFunction("testing.tRunner.func1"),
	)
}
