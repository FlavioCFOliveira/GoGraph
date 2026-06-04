//go:build gograph_crashinject

package crashinject_test

// This file holds the crash-injection round-trip that depends on the
// helper actually self-killing. It is compiled only under the
// gograph_crashinject build tag, because without that tag the helper's
// embedded crashpoint.Breakpoint is the production no-op and the child
// would run to completion instead of being SIGKILL'd. The inert
// (no-tag) behaviour is asserted separately by the untagged guard test
// in breakpoint_inert_test.go.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRoundTrip_WALMidFrame exercises the full crash-injection
// round-trip:
//
//  1. Parent calls Run(t, "wal.mid-frame", ...) which spawns
//     cmd/crashinject-helper (built with -tags gograph_crashinject)
//     with GOGRAPH_CRASH_AT=wal.mid-frame.
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

	// Child must have been killed by SIGKILL.
	if !out.Killed {
		t.Errorf("Killed = false; want true (child must be SIGKILL'd at the breakpoint)\nstdout: %s\nstderr: %s",
			out.Stdout, out.Stderr)
	}

	// WAL reader must detect the torn second frame.
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
