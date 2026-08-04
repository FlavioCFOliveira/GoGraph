package testlayers

// instrument.go — rmp #2319: the environment precondition a CONCURRENCY-EFFECT
// control depends on, and why coverage instrumentation removes it.
//
// # The class of test this serves
//
// Some tests do not measure the module. They measure whether the ENVIRONMENT can
// still demonstrate an effect that another test's verdict depends on — a negative
// or positive control. Two in this module:
//
//   - bench/mvccwrite's TestWriteScalingInstrument_SeesConcurrency forces
//     synthetic, genuinely parallel CPU-bound work through one mutex and requires
//     the cost to exceed the sprint's scaling target. If it cannot, the gates that
//     read the same measurement have no standing.
//   - store/wal's TestAppend_LoopInterleavesUnderConcurrency requires a loop of
//     individual appends to be observed interleaving against eight concurrent
//     appenders. If it cannot, AppendRun's contiguity test proves nothing.
//
// Both assert that a difference is VISIBLE. Neither can be satisfied by a machine
// that is not currently able to run the two arms differently.
//
// # Why coverage instrumentation specifically
//
// `make ci` runs the suite twice: once under `go test -race ./...`, and again under
// `make cover-gate`, which rebuilds every package with `-coverpkg`/`-coverprofile`.
// Coverage instrumentation adds a counter increment to every basic block, which
// makes each unit of work longer and more uniform. That COMPRESSES the free arm
// toward the serialised one while leaving available parallelism probing high, so
// bench/mvccwrite's existing requireAvailableParallelism guard — which measures
// only the free arm against one writer — never fires.
//
// Measured, same tree, same machine, `make cover-gate`:
//
//	available parallelism: 13.63x   (the existing guard sees no problem)
//	serialisation ratio:    2.432x   (below the 3.00x target -> FALSE NO-GO)
//
// and store/wal in the same run: "a loop of 24 individual Appends stayed contiguous
// across 5 attempts against 8 concurrent appenders".
//
// Both pass with `go test -count=1` on their own package. The red was the
// instrumentation, not the module.
//
// # Why SKIP rather than a lower threshold
//
// Lowering the target would let a real serialisation defect through, which is the
// only thing these controls exist to prevent. Skipping states the precondition
// honestly instead, exactly as [RequireSoak] states a layer's.
//
// # Why this cannot hide a defect
//
// It applies ONLY to controls — tests whose subject is the environment. The gates
// that measure the ENGINE (TestWriteScalingGate, TestWALWriteScalingGate,
// TestWriteConcurrencyGate) keep asserting under coverage, so a serialisation
// defect injected into the engine still fails the coverage run. And the controls
// themselves still assert under `go test -race ./...`, which is the same `make ci`
// invocation — so a control that genuinely broke is caught there, in the arm that
// can see it.
//
// The skip is LOUD by contract: every caller passes the measurement that motivated
// it, so a silently-skipped control can never be mistaken for a passing one.

import "testing"

// coverModeFn is [testing.CoverMode], indirected so the negative test can drive
// the instrumented branch without an instrumented build.
var coverModeFn = testing.CoverMode

// RequireUninstrumented skips tb when the binary was built with coverage
// instrumentation, which removes the environment precondition a
// concurrency-EFFECT control depends on. See the file comment for the measurements.
//
// detail must name the quantity the caller was about to assert on, so the skip
// reads as a stated precondition rather than as an absence. Callers that have not
// measured anything yet should say what they were going to measure.
//
// It is for CONTROLS ONLY — a test whose subject is whether the environment can
// still demonstrate an effect. A test whose subject is the module must keep
// asserting, or coverage runs stop gating the module.
func RequireUninstrumented(tb testing.TB, detail string) {
	tb.Helper()
	if mode := coverModeFn(); mode != "" {
		tb.Skipf("coverage instrumentation is active (CoverMode=%q), which compresses the arms "+
			"this control compares and removes the precondition it needs: %s. Nothing is "+
			"concluded about the module — the same control asserts under `go test -race ./...` "+
			"in this very `make ci`, and the ENGINE gates keep asserting here. See "+
			"internal/testlayers/instrument.go for the measured compression (rmp #2319).",
			mode, detail)
	}
}

// Instrumented reports whether coverage instrumentation is active, for a caller
// that wants to adjust rather than skip.
func Instrumented() bool { return coverModeFn() != "" }
