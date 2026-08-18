// Package crashinject provides a subprocess-based crash-injection
// harness for deterministic crash-safety testing of WAL, snapshot,
// and checkpoint write paths.
//
// # Architecture
//
// Crash-injection tests use a parent–child model:
//
//  1. The test parent calls [Run] with a named scenario.
//  2. [Run] spawns cmd/crashinject-helper as a child process, passing
//     GOGRAPH_CRASH_AT=<scenario> and GOGRAPH_CRASH_DIR=<dir>.
//  3. The helper runs the scenario and calls [Breakpoint] at a
//     precisely chosen execution point.
//  4. [Breakpoint] sends SIGKILL to itself, terminating the child
//     abruptly at that exact state.
//  5. [Run] returns an [Out] value describing how the child exited,
//     and the caller inspects the artefacts left in dir.
//
// # Breakpoint registration
//
// Library code (e.g. store/wal, store/snapshot) calls [Breakpoint]
// at any point where a crash should be injected. A typical call site:
//
//	crashinject.Breakpoint("wal.mid-frame")
//
// This is a no-op in production (GOGRAPH_CRASH_AT is not set) and
// self-kills the process when running under the crash harness.
//
// # Concurrency
//
// [Breakpoint] reads an environment variable set once at process
// startup — it is safe to call concurrently with no locking.
// [Run] is safe to call from multiple goroutines (each invocation
// spawns an independent child process); the package-level binary
// cache is guarded by a [sync.Once].
package crashinject

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
)

// EnvCrashAt is the environment variable read by [Breakpoint] to
// decide which named point should trigger a crash. It is an alias for
// [crashpoint.EnvCrashAt]; the canonical definition lives in the
// dependency-light crashpoint package so production code can embed
// breakpoints without importing this test-harness package.
const EnvCrashAt = crashpoint.EnvCrashAt

// EnvCrashDir is the environment variable that tells the helper
// binary where to place its artefacts (WAL files, temp data). Alias
// for [crashpoint.EnvCrashDir].
const EnvCrashDir = crashpoint.EnvCrashDir

// Breakpoint is a thin re-export of [crashpoint.Breakpoint] so existing
// callers of crashinject.Breakpoint keep working. New production call
// sites should import internal/crashpoint directly to avoid pulling the
// testing package into their binaries.
func Breakpoint(name string) { crashpoint.Breakpoint(name) }

// Out captures the observable outcome of a helper child process
// spawned by [Run].
type Out struct {
	// Signal is the signal that terminated the child, or nil.
	Signal os.Signal

	// Dir is the crash artefact directory used by the child. Callers
	// inspect artefacts left there after Run returns.
	Dir string

	// Stdout and Stderr hold the child's captured output streams.
	Stdout []byte
	Stderr []byte

	// ExitCode is the numeric exit status. Meaningful only when
	// Killed is false and the child exited voluntarily.
	ExitCode int

	// Killed reports whether the child was terminated by SIGKILL at a
	// [Breakpoint] (i.e. a genuine crash-injection self-kill).
	// It is false when the child was killed by a context timeout or
	// cancellation; use [Out.TimedOut] to distinguish that case.
	Killed bool

	// TimedOut reports whether the context deadline elapsed before the
	// child exited. When true, Killed is false even if the child was
	// ultimately terminated by SIGKILL (the kill was issued by
	// exec.CommandContext, not by a crashpoint self-kill).
	TimedOut bool
}

// Opts configures a [Run] invocation.
type Opts struct {
	// Dir is the crash artefact directory forwarded to the helper via
	// GOGRAPH_CRASH_DIR. If empty, [Run] creates a fresh t.TempDir()
	// and the caller finds artefacts there after Run returns.
	Dir string

	// Env holds additional KEY=VALUE pairs appended to the child
	// environment (after GOGRAPH_CRASH_AT and GOGRAPH_CRASH_DIR).
	Env []string

	// Timeout caps the child execution. Zero defaults to 30 s.
	Timeout time.Duration
}

// helperBin caches the result of compiling cmd/crashinject-helper so
// every test in a single go test run reuses the same binary.
//
// helperBinDir records the temporary directory that holds the cached binary so
// [RemoveHelperBinary] can delete it at process exit. It is written exactly
// once, inside helperBinOnce.Do, and read only after every test has returned
// (see [RemoveHelperBinary]), so it needs no separate synchronisation.
var (
	helperBinOnce sync.Once
	helperBinPath string
	helperBinDir  string
	helperBinErr  error
)

// Run builds (lazily) and spawns the cmd/crashinject-helper binary
// with GOGRAPH_CRASH_AT=scenario. It waits for the child to exit and
// returns the captured output and exit status.
//
// The caller should inspect Out.Killed to confirm the child was
// terminated by SIGKILL, and then examine the artefacts in Out.Dir.
func Run(t testing.TB, scenario string, opts Opts) (Out, error) {
	t.Helper()

	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}

	helperPath, err := buildHelperOnce(t)
	if err != nil {
		return Out{}, fmt.Errorf("crashinject.Run: build helper: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	// helperPath is the locally-built crashinject-helper binary produced
	// by buildHelperOnce (which runs `go build` in the project tree); the
	// path is process-local and not user-supplied. gosec G204 otherwise
	// flags every exec.Command with a variable argument.
	cmd := exec.CommandContext(ctx, helperPath) //nolint:gosec // G204: helperPath is buildHelperOnce output, not user input
	cmd.Env = append(os.Environ(),
		EnvCrashAt+"="+scenario,
		EnvCrashDir+"="+opts.Dir,
	)
	cmd.Env = append(cmd.Env, opts.Env...)
	cmd.Dir = opts.Dir

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()

	// Capture whether the context expired before inspecting the exit
	// status. exec.CommandContext kills with SIGKILL on timeout, which
	// is byte-identical to a crashpoint self-kill at the OS level. We
	// must distinguish the two using the context's final state.
	ctxTimedOut := ctx.Err() != nil

	out := Out{
		Stdout: stdoutBuf.Bytes(),
		Stderr: stderrBuf.Bytes(),
		Dir:    opts.Dir,
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if !isExitError(runErr, &exitErr) {
			// A non-ExitError with the context already expired means the
			// deadline fired before the child produced a wait status — e.g.
			// exec.Start/Wait returning "context deadline exceeded" when a very
			// short timeout elapses around process startup on a heavily loaded
			// machine. That is still a timeout, not a harness failure, so report
			// it consistently with the post-start SIGKILL-on-deadline path
			// (TimedOut=true) rather than surfacing the raw error. This keeps the
			// deadline classification stable however early the deadline fires
			// (fixes a load-induced flake in the full -race suite).
			if ctxTimedOut {
				out.TimedOut = true
				return out, nil
			}
			return out, fmt.Errorf("crashinject.Run: exec: %w", runErr)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				out.Signal = ws.Signal()
				if ws.Signal() == syscall.SIGKILL && ctxTimedOut {
					// The kill was issued by exec.CommandContext due to
					// the deadline, not by a crashpoint self-kill.
					out.TimedOut = true
				} else {
					out.Killed = ws.Signal() == syscall.SIGKILL
				}
				return out, nil
			}
			out.ExitCode = ws.ExitStatus()
		}
		return out, nil
	}
	return out, nil
}

// isExitError unwraps err with errors.As to detect an *exec.ExitError,
// optionally writing the discovered pointer into *target. Using errors.As
// (rather than a direct type assertion) preserves correctness when the
// caller wraps the os/exec error via fmt.Errorf("...: %w", err) — a
// pattern that some recovery harness call sites use and that would
// otherwise cause a silent fallback to the wrong code path.
func isExitError(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	if target != nil {
		*target = ee
	}
	return true
}

// buildHelperOnce compiles cmd/crashinject-helper exactly once per
// test process and caches the binary path. The binary is placed in a
// process-unique temporary directory (created via os.MkdirTemp) so
// that concurrent test processes never share a file path and cannot
// race on the same binary (ETXTBSY on Linux, partial-write on others).
//
// The directory outlives every individual test in the process, by design, and
// is removed by [RemoveHelperBinary] when the process exits. It must NOT be
// removed with t.Cleanup: the path is cached in helperBinOnce, so deleting it
// when the triggering test ends would leave every later test in the same
// process holding a path to a binary that no longer exists.
func buildHelperOnce(t testing.TB) (string, error) {
	t.Helper()
	helperBinOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			helperBinErr = fmt.Errorf("locate module root: %w", err)
			return
		}
		// Per-process unique directory eliminates cross-process file-path
		// collisions that cause ETXTBSY on Linux or partially-written
		// binaries on other platforms. The directory is intentionally not
		// cleaned up DURING the test run: removing it after the first test
		// that triggered the build would invalidate the cached path for all
		// subsequent tests in the same process.
		//
		// It is removed at PROCESS exit instead, by [RemoveHelperBinary].
		// This used to rely on "OS temp-dir cleanup (reboot / tmpwatch /
		// launchd) reclaims the directory between runs", which is false on
		// macOS: launchd prunes /var/folders only on a very long idle
		// schedule, so each `make ci` stranded two 15 MB directories (one per
		// suite run: `go test -race`, then the coverage pass) and 697 of them
		// had accumulated by the time the volume filled. A full temp volume
		// then makes every WAL append fail with ENOSPC, which the WAL reports
		// as a poisoned writer that discarded its un-synced suffix — the exact
		// signature of a genuine durability defect. The leak was therefore not
		// merely untidy: it manufactured false evidence of data loss
		// (rmp #2527).
		dir, err := os.MkdirTemp("", "gograph-crashinject-*")
		if err != nil {
			helperBinErr = fmt.Errorf("crashinject helper tmpdir: %w", err)
			return
		}
		helperBinDir = dir

		binPath := filepath.Join(dir, "crashinject-helper"+helperBinSuffix)
		// helperBuildTags carries -tags gograph_crashinject only when this
		// package was itself compiled with that tag, so the helper's embedded
		// crashpoint.Breakpoint matches the parent's expectation (active hook
		// under the tag, production no-op without it). It is empty otherwise.
		// Capacity: "build" + the tag flags + "-o" + binPath + the package.
		args := make([]string, 0, 1+len(helperBuildTags)+3)
		args = append(args, "build")
		args = append(args, helperBuildTags...)
		args = append(args, "-o", binPath, "./cmd/crashinject-helper")
		// args is a hard-coded build invocation; binPath is inside a
		// process-local os.MkdirTemp directory. Not user-tainted.
		cmd := exec.Command("go", args...) //nolint:gosec // G204: hard-coded `go build` against project path
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			helperBinErr = fmt.Errorf("go build crashinject-helper: %w\n%s", err, out)
			return
		}
		helperBinPath = binPath
	})
	return helperBinPath, helperBinErr
}

// RemoveHelperBinary deletes the temporary directory holding this process's
// cached crashinject-helper binary. It is a no-op when no helper was ever
// built, so every package may call it unconditionally.
//
// # Why the hook is process-scoped, and not t.Cleanup
//
// [buildHelperOnce] caches the binary path in a sync.Once for the whole test
// process, precisely so that N crash scenarios pay for one 15 MB `go build`
// instead of N. A t.Cleanup registered by the test that happened to trigger
// that build would delete the binary out from under every later test in the
// same process, turning the cache into a use-after-free. The correct scope is
// therefore the process, and TestMain is the only process-scoped hook the
// testing package offers.
//
// # How to wire it
//
// Every package whose tests call [Run] must install this in its TestMain.
// A bare `defer RemoveHelperBinary()` does NOT work alongside
// goleak.VerifyTestMain, because goleak calls os.Exit and deferred functions do
// not run through os.Exit. Use goleak's own cleanup hook, which replaces that
// os.Exit call and is therefore responsible for exiting:
//
//	func TestMain(m *testing.M) {
//		goleak.VerifyTestMain(m, goleak.Cleanup(func(exitCode int) {
//			crashinject.RemoveHelperBinary()
//			os.Exit(exitCode)
//		}))
//	}
//
// TestHelperCleanup_WiredInEveryCallerPackage enforces this wiring across the
// module, so a new package that calls [Run] cannot silently reintroduce the
// leak.
//
// # Concurrency
//
// RemoveHelperBinary is NOT safe for concurrent use with [Run]. It reads the
// cached directory without synchronisation and deletes the binary that [Run]
// executes, so it must be called only after every test in the process has
// returned — which is exactly what the TestMain hook above guarantees.
func RemoveHelperBinary() {
	if helperBinDir == "" {
		return
	}
	_ = removeHelperDir(helperBinDir)
	helperBinDir = ""
	helperBinPath = ""
}

// removeHelperDir removes dir and everything under it. A directory that has
// already gone is not an error — os.RemoveAll returns nil for an absent path —
// which is what makes the hook idempotent: a second call, a manual prune, or a
// concurrent `make ci` in the same checkout must all be tolerated.
//
// It is a separate function so the removal itself is unit-testable against a
// real populated directory, independently of the process-exit wiring.
func removeHelperDir(dir string) error {
	// dir originates from os.MkdirTemp inside this package; it is never
	// caller-supplied, so gosec's G703 warning about os.RemoveAll on a
	// variable path does not apply.
	if err := os.RemoveAll(dir); err != nil { //nolint:gosec // G703: dir is this package's own os.MkdirTemp result, not user input
		return fmt.Errorf("remove crashinject helper dir %q: %w", dir, err)
	}
	return nil
}

// HelperBinaryDir reports the temporary directory holding this process's cached
// crashinject-helper binary, or "" when no helper has been built (or after
// [RemoveHelperBinary] has run).
//
// It exists so a test can assert that the directory the process-exit hook will
// delete is the directory the build actually created — the link between "the
// removal works" and "the removal is aimed at the right path". It carries the
// same concurrency restriction as [RemoveHelperBinary].
func HelperBinaryDir() string {
	return helperBinDir
}

// moduleRoot returns the absolute path of the Go module root by
// running "go env GOMOD" and taking its directory.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	modFile := strings.TrimSpace(string(out))
	if modFile == "" || modFile == os.DevNull {
		return "", fmt.Errorf("go env GOMOD: module root not found (running outside a module?)")
	}
	return filepath.Dir(modFile), nil
}
