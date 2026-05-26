// Command crashinject-helper is the child process spawned by the
// crashinject harness during crash-injection tests. It should never
// be run directly; it is always invoked by [crashinject.Run] with the
// environment variables GOGRAPH_CRASH_AT and GOGRAPH_CRASH_DIR set.
//
// Each scenario writes a specific artefact (WAL file, snapshot, …)
// to GOGRAPH_CRASH_DIR and then calls [crashinject.Breakpoint] at
// the named execution point. [crashinject.Breakpoint] sends SIGKILL
// to the process when GOGRAPH_CRASH_AT matches the breakpoint name,
// leaving the artefact in a deterministically torn state.
//
// Registered scenarios:
//
//	wal.mid-frame  — writes one complete WAL frame, appends a partial
//	                 second-frame header, then crashes. The resulting
//	                 WAL file has a torn tail that wal.Reader must
//	                 detect as ErrTornFrame.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gograph/internal/crashinject"
	"gograph/store/wal"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("crashinject-helper: ")
	// run owns the deferred cleanup; main translates its return value
	// into an exit code via os.Exit only after run's defers have all
	// fired. This avoids the exitAfterDefer pitfall where os.Exit inside
	// run would silently skip the temp-dir RemoveAll.
	os.Exit(run())
}

// run executes the requested crash-injection scenario and returns a
// process exit code. Any deferred cleanup registered here runs before
// the caller invokes os.Exit.
func run() int {
	scenario := os.Getenv(crashinject.EnvCrashAt)
	if scenario == "" {
		fmt.Fprintln(os.Stderr, "crashinject-helper: GOGRAPH_CRASH_AT is not set; refusing to run")
		return 1
	}

	dir := os.Getenv(crashinject.EnvCrashDir)
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "crashinject-*")
		if err != nil {
			log.Printf("MkdirTemp: %v", err)
			return 1
		}
		// Clean up when the helper exits normally (non-crash path).
		// dir originates from os.MkdirTemp ("" prefix forces $TMPDIR), so
		// the path is process-local and not user-tainted; gosec G703
		// otherwise flags every os.RemoveAll(variable) call.
		defer func() { _ = os.RemoveAll(dir) }() //nolint:gosec // G703: dir is from MkdirTemp, not user input
	}

	switch scenario {
	case "wal.mid-frame":
		runWALMidFrame(dir)
	default:
		fmt.Fprintf(os.Stderr, "crashinject-helper: unknown scenario %q\n", scenario)
		return 1
	}
	return 0
}

// runWALMidFrame writes one complete WAL frame to a file in dir,
// then appends a 10-byte partial frame header (magic + version +
// length, without CRC or payload) to leave the WAL in a torn state,
// and finally calls [crashinject.Breakpoint]("wal.mid-frame") to
// self-kill via SIGKILL.
//
// The resulting file path is dir/crash.wal. A wal.Reader opened on
// that file must:
//   - Decode exactly one complete frame.
//   - Return ErrTornFrame (or ErrCRCMismatch) on the partial second frame.
func runWALMidFrame(dir string) {
	walPath := filepath.Join(dir, "crash.wal")

	// Write one complete frame via the WAL writer.
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("wal.Open: %v", err)
	}
	if err := w.Append(bytes.Repeat([]byte{0xAA}, 100)); err != nil {
		log.Fatalf("Append frame1: %v", err)
	}
	if err := w.Sync(); err != nil {
		log.Fatalf("Sync frame1: %v", err)
	}
	if err := w.Close(); err != nil {
		log.Fatalf("Close writer: %v", err)
	}

	// Append a partial second-frame header:
	//   magic (4B) + version (2B) + length (4B) = 10 bytes
	// The CRC field (4B) and the 100-byte payload are missing, so
	// the WAL reader will surface ErrTornFrame when it tries to read
	// the remaining 104 bytes.
	f, err := os.OpenFile(walPath, os.O_RDWR|os.O_APPEND, 0o644) //nolint:gosec
	if err != nil {
		log.Fatalf("open WAL for partial write: %v", err)
	}
	partial := make([]byte, 10)
	copy(partial[0:4], wal.Magic[:])                  // magic
	binary.LittleEndian.PutUint16(partial[4:6], 1)    // version = 1
	binary.LittleEndian.PutUint32(partial[6:10], 100) // payload length = 100
	if _, err := f.Write(partial); err != nil {
		_ = f.Close()
		log.Fatalf("write partial header: %v", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		log.Fatalf("sync partial header: %v", err)
	}
	_ = f.Close()

	// Crash here — SIGKILL will be delivered immediately.
	crashinject.Breakpoint("wal.mid-frame")

	// Reached only when GOGRAPH_CRASH_AT != "wal.mid-frame"
	// (non-crash self-test path).
	fmt.Println("runWALMidFrame: completed without crash (GOGRAPH_CRASH_AT != wal.mid-frame)")
}
