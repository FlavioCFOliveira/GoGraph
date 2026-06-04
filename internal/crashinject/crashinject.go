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
	// Stdout and Stderr hold the child's captured output streams.
	Stdout []byte
	Stderr []byte

	// ExitCode is the numeric exit status. Meaningful only when
	// Killed is false and the child exited voluntarily.
	ExitCode int

	// Signal is the signal that terminated the child, or nil.
	Signal os.Signal

	// Killed reports whether the child was terminated by SIGKILL.
	Killed bool

	// Dir is the crash artefact directory used by the child. Callers
	// inspect artefacts left there after Run returns.
	Dir string
}

// Opts configures a [Run] invocation.
type Opts struct {
	// Dir is the crash artefact directory forwarded to the helper via
	// GOGRAPH_CRASH_DIR. If empty, [Run] creates a fresh t.TempDir()
	// and the caller finds artefacts there after Run returns.
	Dir string

	// Timeout caps the child execution. Zero defaults to 30 s.
	Timeout time.Duration

	// Env holds additional KEY=VALUE pairs appended to the child
	// environment (after GOGRAPH_CRASH_AT and GOGRAPH_CRASH_DIR).
	Env []string
}

// helperBin caches the result of compiling cmd/crashinject-helper so
// every test in a single go test run reuses the same binary.
var (
	helperBinOnce sync.Once
	helperBinPath string
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

	out := Out{
		Stdout: stdoutBuf.Bytes(),
		Stderr: stderrBuf.Bytes(),
		Dir:    opts.Dir,
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if !isExitError(runErr, &exitErr) {
			return out, fmt.Errorf("crashinject.Run: exec: %w", runErr)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				out.Signal = ws.Signal()
				out.Killed = ws.Signal() == syscall.SIGKILL
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
// test run and caches the binary path.
func buildHelperOnce(t testing.TB) (string, error) {
	t.Helper()
	helperBinOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			helperBinErr = fmt.Errorf("locate module root: %w", err)
			return
		}
		binPath := filepath.Join(os.TempDir(), "gograph-crashinject-helper"+helperBinSuffix)
		// helperBuildTags carries -tags gograph_crashinject only when this
		// package was itself compiled with that tag, so the helper's embedded
		// crashpoint.Breakpoint matches the parent's expectation (active hook
		// under the tag, production no-op without it). It is empty otherwise.
		// Capacity: "build" + the tag flags + "-o" + binPath + the package.
		args := make([]string, 0, 1+len(helperBuildTags)+3)
		args = append(args, "build")
		args = append(args, helperBuildTags...)
		args = append(args, "-o", binPath, "./cmd/crashinject-helper")
		// args is a hard-coded build invocation; only binPath comes from
		// os.TempDir which is process-local. Not user-tainted.
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
