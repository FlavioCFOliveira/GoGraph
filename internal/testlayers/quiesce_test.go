package testlayers

import (
	"strings"
	"testing"
)

// This file reuses the fakeTB defined in instrument_test.go: both guards use the
// same sliver of testing.TB (Helper + Skipf), so a second fake would only be a
// second thing to keep in step.

// TestRequireQuietMachine_SkipsOnlyInAParallelSuite is the whole contract: the
// guard must default to ASSERTING, and skip only when the parallel-suite variable
// is set.
//
// The default-assert direction is the load-bearing one. A guard that defaulted to
// skipping would make every gate it touches vanish from every ad-hoc run, and a
// vanished gate reads exactly like a passing one — which is the failure mode
// internal/testlayers/instrument.go was written to avoid.
func TestRequireQuietMachine_SkipsOnlyInAParallelSuite(t *testing.T) {
	orig := lookupEnvFn
	t.Cleanup(func() { lookupEnvFn = orig })

	t.Run("variable_absent_does_not_skip", func(t *testing.T) {
		lookupEnvFn = func(string) (string, bool) { return "", false }
		f := &fakeTB{}
		RequireQuietMachine(f, "a 2.5x CPU growth ratio")
		if f.skipped {
			t.Fatalf("guard skipped with %s unset; it must ASSERT by default, got skip %q",
				parallelSuiteEnv, f.msg)
		}
		if InParallelSuite() {
			t.Error("InParallelSuite() = true with the variable unset")
		}
	})

	t.Run("variable_set_skips_loudly", func(t *testing.T) {
		lookupEnvFn = func(name string) (string, bool) {
			if name == parallelSuiteEnv {
				return "1", true
			}
			return "", false
		}
		f := &fakeTB{}
		RequireQuietMachine(f, "a 2.5x CPU growth ratio")
		if !f.skipped {
			t.Fatalf("guard did not skip with %s set", parallelSuiteEnv)
		}
		// The skip must carry the caller's measurement, or a skipped gate reads
		// as a passing one. This is the "LOUD by contract" clause.
		if !strings.Contains(f.msg, "a 2.5x CPU growth ratio") {
			t.Errorf("skip message does not carry the caller's detail, so the skip is not self-explaining:\n%s", f.msg)
		}
		// It must also say where the assertion DOES run, so the reader is not
		// left thinking the check was simply dropped.
		if !strings.Contains(f.msg, "test-timing") {
			t.Errorf("skip message does not name the target where the gate still asserts:\n%s", f.msg)
		}
		if !InParallelSuite() {
			t.Error("InParallelSuite() = false with the variable set")
		}
	})

	t.Run("empty_value_still_counts_as_set", func(t *testing.T) {
		// `GOGRAPH_PARALLEL_SUITE=` is a deliberate declaration, not an absence.
		// Testing presence rather than truthiness avoids a class of Makefile bug
		// where an empty expansion silently re-enables a gate under load.
		lookupEnvFn = func(name string) (string, bool) {
			if name == parallelSuiteEnv {
				return "", true
			}
			return "", false
		}
		f := &fakeTB{}
		RequireQuietMachine(f, "an elapsed-time budget")
		if !f.skipped {
			t.Fatalf("guard did not skip with %s set to the empty string", parallelSuiteEnv)
		}
	})
}
