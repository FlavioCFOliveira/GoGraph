//go:build !gograph_crashinject

package crashpoint_test

// Production-build (no-tag) guarantees for task #1313: the disabled
// Breakpoint is inert even on an exact GOGRAPH_CRASH_AT match, and the
// crashpoint package — as it is compiled into released binaries — links
// neither os nor syscall, so the SIGKILL machinery is provably absent.
//
// These assertions are gated to the default build because under
// -tags gograph_crashinject an exact match would SIGKILL the test binary
// and the package legitimately imports os and syscall.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
)

// crashpointImportPath is the import path of the package under test; its
// non-test dependency set is what a released binary actually links.
const crashpointImportPath = "github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"

// TestBreakpoint_InertOnExactMatch proves the load-bearing property of
// task #1313: in the untagged build, calling Breakpoint with
// GOGRAPH_CRASH_AT set to the EXACT matching name must NOT kill the
// process. Before the build-tag gating, the unconditional SIGKILL body
// would have terminated this test binary here; reaching the assertion
// after the call demonstrates the no-op held.
func TestBreakpoint_InertOnExactMatch(t *testing.T) {
	const name = "checkpoint.mid-truncate"

	prev, had := os.LookupEnv(crashpoint.EnvCrashAt)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(crashpoint.EnvCrashAt, prev)
		} else {
			_ = os.Unsetenv(crashpoint.EnvCrashAt)
		}
	})

	if err := os.Setenv(crashpoint.EnvCrashAt, name); err != nil {
		t.Fatal(err)
	}

	// In the enabled build this would SIGKILL; in the disabled build it
	// must return normally.
	crashpoint.Breakpoint(name)

	// Reached only because the disabled Breakpoint ignored the matching
	// env entirely. A sentinel write makes the survival explicit.
	survived := true
	if !survived {
		t.Fatal("unreachable")
	}
}

// TestBreakpoint_NoCrashMachineryLinked is the build-elision assertion.
// It asks the Go toolchain for the transitive non-test imports of the
// crashpoint package in this (default, no-tag) build and fails if either
// "os" or "syscall" appears. The disabled Breakpoint compiles to an
// empty body that references neither, so the SIGKILL machinery (and the
// per-commit os.Getenv) is provably absent from released binaries.
//
// This is the compile-time counterpart to the runtime no-op tests: it
// guarantees the elision at the linker level, not merely that the body
// did nothing at runtime.
func TestBreakpoint_NoCrashMachineryLinked(t *testing.T) {
	// `go list -deps -f {{.ImportPath}}` over the package prints the
	// package itself plus every transitive (non-test) dependency, one per
	// line. The argument is a compile-time constant import path, not user
	// input, so gosec G204 does not apply.
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", crashpointImportPath) //nolint:gosec // G204: fixed import path constant, not user input
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, outBytes)
	}

	forbidden := map[string]struct{}{
		"os":      {},
		"syscall": {},
	}
	for _, line := range strings.Split(strings.TrimSpace(string(outBytes)), "\n") {
		dep := strings.TrimSpace(line)
		if _, bad := forbidden[dep]; bad {
			t.Errorf("crashpoint links %q in the untagged build; the disabled "+
				"Breakpoint must reference neither os nor syscall so the SIGKILL "+
				"machinery is absent from released binaries", dep)
		}
	}
}
