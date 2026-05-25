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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// EnvCrashAt is the environment variable read by [Breakpoint] to
// decide which named point should trigger a crash.
const EnvCrashAt = "GOGRAPH_CRASH_AT"

// EnvCrashDir is the environment variable that tells the helper
// binary where to place its artefacts (WAL files, temp data).
const EnvCrashDir = "GOGRAPH_CRASH_DIR"

// Breakpoint checks whether GOGRAPH_CRASH_AT equals name; if so,
// it sends SIGKILL to the current process to simulate an abrupt crash
// at this exact execution point.
//
// name must be non-empty; an empty name is silently ignored so that
// callers cannot accidentally crash when the environment variable is
// unset (where os.Getenv returns "").
//
// In production (GOGRAPH_CRASH_AT unset or empty) this function is
// a no-op with no measurable overhead (one string comparison).
//
// Breakpoint is safe for concurrent use.
func Breakpoint(name string) {
	if name == "" {
		return // guard: never match the empty env value
	}
	if at := os.Getenv(EnvCrashAt); at != "" && at == name {
		// Self-kill via SIGKILL; cannot be caught or deferred.
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		// Block until the signal is delivered (should be instant).
		select {} //nolint:staticcheck
	}
}

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

	cmd := exec.CommandContext(ctx, helperPath)
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

// isExitError is a type-assertion helper that avoids an import of
// "errors" at the call site.
func isExitError(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	ee, ok := err.(*exec.ExitError)
	if ok && target != nil {
		*target = ee
	}
	return ok
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
		binPath := filepath.Join(os.TempDir(), "gograph-crashinject-helper")
		args := []string{"build", "-o", binPath, "./cmd/crashinject-helper"}
		cmd := exec.Command("go", args...)
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
