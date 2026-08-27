package testlayers

// quiesce.go — rmp #2517: the environment precondition a WALL-CLOCK assertion
// depends on, and why a parallel whole-suite run removes it.
//
// # The class of test this serves
//
// A test that asserts on elapsed time, on a ratio of elapsed times, on a
// throughput, or on CPU time is measuring the MACHINE as much as the code. That
// is fine on a quiet machine and meaningless on a busy one.
//
// `make ci` runs `test-short`, which is `go test -race -count=1 ./...`, and
// `go test` builds and runs PACKAGES IN PARALLEL (`-p` defaults to GOMAXPROCS).
// So every such assertion in the short layer is evaluated while an arbitrary
// number of other packages compete for the same cores. Three measured
// consequences, all on green trees:
//
//   - cypher's TestDetachDeleteDoesNotDegradeAcrossCycles reported a CPU growth
//     ratio of 2.90x against a 2.5x limit, with per-cycle times of 852ms rising
//     to 2.48s. In isolation the SAME test measures 19.3, 17.8, 18.5, 18.9, 18.2
//     and 18.1 milliseconds — a last-over-first ratio of 0.94x, perfectly flat.
//     The cycles ran 45 to 130 times slower under the gate; the failing run had
//     the cypher package at 297s and cypher/exec at 234s executing concurrently.
//   - bench/mvccwrite's TestWriteScalingInstrument_SeesConcurrency measured
//     2.984x against a 3.00x target, after its own probe reported 12.91x
//     available parallelism. Passes 3/3 solo (rmp #2499).
//   - bench/cyclicjoin's TestCyclicJoin_FittedExponents tripped a 1.50x
//     constant-factor tolerance at its SMALLEST data point, while its real claim
//     — allocation ratios of 2.7x to 27.4x against a 1.5x floor — and its fitted
//     exponents were healthy. Passes 3/3 uninstrumented (rmp #2506).
//
// # Why CPU time is not the answer
//
// It was tried. cypher/delete_scaling_test.go moved its absolute wall-clock
// budget to the soak layer (which worked — its godoc records 40.61s in the short
// layer for work that takes 375.6ms alone) and kept the regression claim in the
// short layer by switching to a CPU-time ratio, on the reasoning that CPU time is
// load-independent. It is not: contention inflates it through scheduler overhead,
// cache and TLB pressure, and spinning. The first bullet above IS that CPU ratio
// failing, and its sibling power control TestDeleteCycleGateDetectsDegradation
// fails the mirror way, because contention COMPRESSES the ratio it must exceed
// (rmp #2589).
//
// # Why SKIP rather than a wider tolerance
//
// Exactly the reason [RequireUninstrumented] gives. Widening the tolerance until
// it survives the worst observed load would let a real regression through, which
// is the only thing these gates exist to prevent — and the numbers show how far
// it would have to go: 45x to 130x on the delete cycles. Stating the precondition
// is honest; inflating the threshold is not.
//
// # Why this cannot hide a defect
//
// The default is to ASSERT. [RequireQuietMachine] skips only when
// GOGRAPH_PARALLEL_SUITE is set, which happens in exactly one place — the
// parallel whole-suite targets, where the measurement is invalid anyway. So:
//
//   - `go test ./cypher/` on one package, or a single `-run` filter, still
//     asserts. A developer investigating one gate is not silently skipped.
//   - `make ci` runs `test-timing` as its own phase, serially (`-p 1`) and
//     WITHOUT the variable, so every gate guarded here still gates every push.
//     Nothing moves out of the pre-push gate; it moves to the phase of that gate
//     where the measurement means something.
//   - A gate that genuinely regressed fails in `test-timing`.
//
// The skip is LOUD by contract: every caller passes the measurement it was about
// to assert on, so a skipped gate can never be mistaken for a passing one.
//
// # Scope
//
// This is for an assertion whose SUBJECT is a duration, a rate, or a ratio of
// them. It is not a general-purpose skip. A functional assertion — a result, an
// error, an invariant, an allocation count, an operation count, a fitted
// complexity exponent — is load-independent and must keep asserting in the short
// layer. Where a test has both kinds of arm, guard only the timing arm and leave
// the rest asserting; bench/cyclicjoin is the worked example.
//
// The full inventory of affected assertions is docs/short-layer-wallclock-audit.md.

import (
	"os"
	"testing"
)

// parallelSuiteEnv names the environment variable that declares "this process is
// one of many packages being tested in parallel, so wall-clock measurement here
// is meaningless". The parallel whole-suite Make targets set it; the serial
// timing target deliberately does not.
const parallelSuiteEnv = "GOGRAPH_PARALLEL_SUITE"

// lookupEnvFn is [os.LookupEnv], indirected so the negative test can drive both
// branches without mutating the real process environment.
var lookupEnvFn = os.LookupEnv

// RequireQuietMachine skips tb when the test binary is running as part of a
// parallel whole-suite run, which removes the quiet-machine precondition that any
// wall-clock, throughput, or CPU-time assertion depends on. See the file comment
// for the measurements.
//
// detail must name the quantity the caller was about to assert on, so the skip
// reads as a stated precondition rather than as an absence. A caller that has not
// measured anything yet should say what it was going to measure.
//
// It is for TIMING assertions only. A functional assertion is load-independent
// and must keep asserting; a test with both kinds of arm guards only the timing
// arm.
//
// Concurrency: safe for concurrent use; it reads an environment variable and
// nothing else.
func RequireQuietMachine(tb testing.TB, detail string) {
	tb.Helper()
	if _, set := lookupEnvFn(parallelSuiteEnv); set {
		tb.Skipf("%s is set, so this process is one of many packages being tested in "+
			"parallel and the quiet-machine precondition this timing assertion needs is "+
			"absent: %s. Nothing is concluded about the module — the same assertion runs "+
			"and ASSERTS in `make test-timing`, which `make ci` invokes serially (-p 1) "+
			"for exactly this reason, and it asserts on any single-package run too. See "+
			"internal/testlayers/quiesce.go for the measured distortion (rmp #2517) and "+
			"docs/short-layer-wallclock-audit.md for the full inventory.",
			parallelSuiteEnv, detail)
	}
}

// InParallelSuite reports whether the test binary is running as part of a
// parallel whole-suite run, for a caller that wants to adjust or report rather
// than skip.
func InParallelSuite() bool {
	_, set := lookupEnvFn(parallelSuiteEnv)
	return set
}
