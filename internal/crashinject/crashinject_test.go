package crashinject_test

import (
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestBreakpoint_NoOp verifies that Breakpoint is a no-op when
// GOGRAPH_CRASH_AT is not set (or does not match). This protects
// production code that calls Breakpoint inline.
func TestBreakpoint_NoOp(t *testing.T) {
	prev := os.Getenv(crashinject.EnvCrashAt)
	if err := os.Unsetenv(crashinject.EnvCrashAt); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv(crashinject.EnvCrashAt, prev) }()

	// Must return without killing the process.
	crashinject.Breakpoint("some.nonexistent.point")
	// Empty name must always be a no-op (never matches env value).
	crashinject.Breakpoint("") //nolint:staticcheck — intentional empty name test
}

// TestRun_UnknownScenario verifies that an unknown scenario name
// causes the helper to exit with a non-zero status code and that
// Run does not return a Go-level error (the exit code is surfaced
// in Out.ExitCode, not as an error).
func TestRun_UnknownScenario(t *testing.T) {
	out, err := crashinject.Run(t, "no.such.scenario", crashinject.Opts{})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if out.Killed {
		t.Error("Killed = true; unexpected for unknown scenario")
	}
	if out.ExitCode == 0 {
		t.Errorf("ExitCode = 0; want non-zero for unknown scenario\nstdout: %s\nstderr: %s",
			out.Stdout, out.Stderr)
	}
}
