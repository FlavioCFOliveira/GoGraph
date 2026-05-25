// Package subproc provides a deterministic subprocess helper for
// cross-process tests. It re-execs the current test binary in a
// named "mode", dispatching to a registered handler function in the
// child.
//
// # Architecture
//
// The typical usage pattern mirrors the "TestMain subprocess" idiom
// used by the Go standard library and several test-heavy packages:
//
//  1. Each package that needs a child operation registers a handler
//     via [Register] at init time (usually via an init function or a
//     package-level var in a _test.go file).
//  2. In the test's TestMain, call [Dispatch] before running tests.
//     When the binary is running as a child, Dispatch calls the
//     registered handler and exits; when it is the parent, Dispatch
//     is a no-op.
//  3. The parent test calls [Run] to spawn the child with the chosen
//     mode, captures stdout/stderr, and inspects results.
//
// # Example
//
//	func init() {
//	    subproc.Register("echo", func(args []string) int {
//	        fmt.Println(args[0])
//	        return 0
//	    })
//	}
//
//	func TestMain(m *testing.M) {
//	    subproc.Dispatch()
//	    os.Exit(m.Run())
//	}
//
//	func TestEcho(t *testing.T) {
//	    out, _, err := subproc.Run(t, "echo", "hello")
//	    if string(out) != "hello\n" { t.Errorf(...) }
//	}
//
// # Concurrency
//
// [Register] is not safe for concurrent use; it must be called from
// init functions or TestMain before any test runs. [Dispatch] is
// called at most once per process. [Run] is safe for concurrent use
// from multiple goroutines.
package subproc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// EnvMode is the environment variable read by [Dispatch] to determine
// which registered handler to invoke.
const EnvMode = "GOGRAPH_SUBPROC_MODE"

// Handler is a function invoked in the child process for a given mode.
// args are the extra arguments passed by the parent via [Run].
// The return value is used as the child's exit code.
type Handler func(args []string) int

var (
	registryMu sync.RWMutex
	registry   = map[string]Handler{}
)

// Register associates name with handler. It must be called before
// [Dispatch] runs (i.e. from init or TestMain). Registering the same
// name twice panics.
func Register(name string, handler Handler) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[name]; ok {
		panic(fmt.Sprintf("subproc.Register: mode %q already registered", name))
	}
	registry[name] = handler
}

// Dispatch checks GOGRAPH_SUBPROC_MODE. If the variable is set, it
// looks up the registered handler, calls it with os.Args[1:], and
// exits with the handler's return code. If the variable is not set,
// Dispatch returns immediately (parent path).
//
// Call Dispatch at the top of TestMain, before m.Run().
func Dispatch() {
	mode := os.Getenv(EnvMode)
	if mode == "" {
		return // parent path
	}

	registryMu.RLock()
	handler, ok := registry[mode]
	registryMu.RUnlock()

	if !ok {
		fmt.Fprintf(os.Stderr, "subproc: unknown mode %q\n", mode)
		os.Exit(1)
	}
	os.Exit(handler(os.Args[1:]))
}

// Run spawns the current test binary (os.Args[0]) as a child process
// in the named mode, passing extra as additional argv. The child's
// working directory is set to t.TempDir().
//
// Run blocks until the child exits and returns its captured stdout,
// stderr, and any execution error. A non-zero exit code from the child
// is surfaced as a non-nil error.
//
// Run is safe for concurrent use from multiple test goroutines.
func Run(t testing.TB, mode string, extra ...string) (stdout, stderr []byte, err error) {
	t.Helper()
	return RunCtx(context.Background(), t, mode, extra...)
}

// RunCtx is the context-aware variant of [Run]. ctx is forwarded to
// exec.CommandContext; cancellation terminates the child.
func RunCtx(ctx context.Context, t testing.TB, mode string, extra ...string) (stdout, stderr []byte, err error) {
	t.Helper()

	args := append([]string{}, extra...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), EnvMode+"="+mode)
	cmd.Dir = t.TempDir()

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), runErr
}

// RunWithTimeout is a convenience wrapper that caps child execution
// at timeout.
func RunWithTimeout(t testing.TB, timeout time.Duration, mode string, extra ...string) (stdout, stderr []byte, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return RunCtx(ctx, t, mode, extra...)
}
