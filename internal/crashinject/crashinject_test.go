package crashinject_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"gograph/internal/crashinject"
	"gograph/store/wal"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRoundTrip_WALMidFrame is the AC#1 / AC#2 test for task #528.
// It exercises the full crash-injection round-trip:
//
//  1. Parent calls Run(t, "wal.mid-frame", ...) which spawns
//     cmd/crashinject-helper with GOGRAPH_CRASH_AT=wal.mid-frame.
//  2. The helper writes one complete WAL frame, appends a torn
//     second-frame header, and is terminated by SIGKILL via
//     [crashinject.Breakpoint].
//  3. Parent asserts Out.Killed = true (SIGKILL).
//  4. Parent opens the WAL file from Out.Dir and confirms that the
//     reader decodes exactly one frame and reports a torn-frame error.
func TestRoundTrip_WALMidFrame(t *testing.T) {
	out, err := crashinject.Run(t, "wal.mid-frame", crashinject.Opts{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// AC#1: child must have been killed by SIGKILL.
	if !out.Killed {
		t.Errorf("Killed = false; want true (child must be SIGKILL'd at the breakpoint)\nstdout: %s\nstderr: %s",
			out.Stdout, out.Stderr)
	}

	// AC#2: WAL reader must detect the torn second frame.
	walPath := filepath.Join(out.Dir, "crash.wal")
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("crash.wal not found in %s: %v", out.Dir, err)
	}

	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	var frames []wal.Frame
	for f := range r.Frames() {
		frames = append(frames, f)
	}

	// Exactly one complete frame must be decoded.
	if len(frames) != 1 {
		t.Errorf("decoded %d frame(s), want 1", len(frames))
	}

	// TailError must be a torn-frame indicator.
	tailErr := r.TailError()
	if tailErr == nil {
		t.Error("TailError() = nil; want ErrTornFrame / ErrBadMagic / ErrCRCMismatch")
	} else if !errors.Is(tailErr, wal.ErrTornFrame) &&
		!errors.Is(tailErr, wal.ErrBadMagic) &&
		!errors.Is(tailErr, wal.ErrCRCMismatch) {
		t.Errorf("TailError() = %v; want one of ErrTornFrame/ErrBadMagic/ErrCRCMismatch", tailErr)
	}
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
