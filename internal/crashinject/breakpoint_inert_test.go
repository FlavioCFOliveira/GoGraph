//go:build !gograph_crashinject

package crashinject_test

// This file is the production-build guard for task #1313: it proves that
// WITHOUT the gograph_crashinject tag the crash breakpoint is inert. It
// is compiled only in the default (no-tag) build — the same build every
// released binary uses — and is excluded when the crash battery runs
// with -tags gograph_crashinject (where the breakpoint is expected to
// fire and is covered by roundtrip_selfkill_test.go instead).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// TestBreakpoint_InertWithoutTag is the core acceptance test for the
// build-tag gating. In the default build, crashinject.Run compiles
// cmd/crashinject-helper WITHOUT the gograph_crashinject tag (helperBuildTags
// is empty), so the helper embeds the production no-op crashpoint.Breakpoint.
//
// It then runs the "wal.mid-frame" scenario with GOGRAPH_CRASH_AT set to
// that exact name — the value that, under the tag, would SIGKILL the
// helper. The assertion is that the helper is NOT killed: it runs the
// scenario to completion and exits cleanly. This is the compile-time
// guarantee that an inherited GOGRAPH_CRASH_AT cannot make a released
// binary self-kill on a durability path.
//
// Before the fix, Breakpoint was unconditionally compiled with the
// SIGKILL body, so the same helper run would have died from SIGKILL and
// out.Killed would have been true — i.e. this test would have failed.
func TestBreakpoint_InertWithoutTag(t *testing.T) {
	// GOGRAPH_CRASH_AT is forwarded to the helper child by Run; we set the
	// matching scenario name to prove that even an exact match has no
	// effect when the helper is built without the crash-injection tag.
	out, err := crashinject.Run(t, "wal.mid-frame", crashinject.Opts{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.Killed {
		t.Fatalf("helper was SIGKILL'd without the gograph_crashinject tag; "+
			"the breakpoint must be inert in the production build\nstdout: %s\nstderr: %s",
			out.Stdout, out.Stderr)
	}
	if out.ExitCode != 0 {
		t.Fatalf("helper exited with code %d, want 0 (it should complete the "+
			"scenario cleanly when the breakpoint is a no-op)\nstdout: %s\nstderr: %s",
			out.ExitCode, out.Stdout, out.Stderr)
	}
}
