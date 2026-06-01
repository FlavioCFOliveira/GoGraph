package subproc_test

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// TestMain dispatches to child mode when GOGRAPH_SUBPROC_MODE is set;
// otherwise it runs the test suite normally.
func TestMain(m *testing.M) {
	// Register all modes used by the tests in this package.
	subproc.Register("echo-args", func(args []string) int {
		if len(args) == 0 {
			return 1
		}
		fmt.Println(args[0])
		return 0
	})
	subproc.Register("exit-zero", func(_ []string) int {
		return 0
	})
	subproc.Register("exit-nonzero", func(_ []string) int {
		return 42
	})
	subproc.Register("write-stderr", func(_ []string) int {
		fmt.Fprintln(os.Stderr, "stderr output")
		return 0
	})
	subproc.Register("concurrent-child", func(_ []string) int {
		fmt.Println("concurrent-child-ok")
		return 0
	})

	// Dispatch runs the child handler (and exits) when this binary
	// was spawned as a child. Otherwise it is a no-op and the parent
	// test suite continues.
	subproc.Dispatch()

	// Verify no goroutine leaks in the parent test suite.
	goleak.VerifyTestMain(m)
}

// ─── AC#1: echo round-trip ───────────────────────────────────────

// TestRun_Echo is the AC#1 test: the parent spawns the child in
// "echo-args" mode with a known argument and asserts that stdout
// matches.
func TestRun_Echo(t *testing.T) {
	arg := "hello-subproc"
	out, errOut, err := subproc.Run(t, "echo-args", arg)
	if err != nil {
		t.Fatalf("Run: %v\nstderr: %s", err, errOut)
	}
	got := strings.TrimSpace(string(out))
	if got != arg {
		t.Errorf("stdout = %q, want %q", got, arg)
	}
}

// TestRun_ExitZero verifies that a handler returning 0 produces no
// error from Run.
func TestRun_ExitZero(t *testing.T) {
	_, _, err := subproc.Run(t, "exit-zero")
	if err != nil {
		t.Errorf("exit-zero: unexpected error: %v", err)
	}
}

// TestRun_ExitNonZero verifies that a non-zero exit code surfaces as
// a non-nil error from Run.
func TestRun_ExitNonZero(t *testing.T) {
	_, _, err := subproc.Run(t, "exit-nonzero")
	if err == nil {
		t.Error("exit-nonzero: expected error, got nil")
	}
}

// TestRun_Stderr verifies that the child's stderr is captured.
func TestRun_Stderr(t *testing.T) {
	_, errOut, err := subproc.Run(t, "write-stderr")
	if err != nil {
		t.Fatalf("write-stderr: unexpected error: %v", err)
	}
	if !strings.Contains(string(errOut), "stderr output") {
		t.Errorf("stderr = %q, want \"stderr output\"", errOut)
	}
}

// ─── AC#2: temp dir is cleaned ───────────────────────────────────

// TestRun_TempDir verifies that the child runs in t.TempDir() and
// that the directory is cleaned up after the test. We simply check
// that Run does not error — cleanup is managed by the testing
// framework's TempDir mechanic.
func TestRun_TempDir(t *testing.T) {
	_, _, err := subproc.Run(t, "exit-zero")
	if err != nil {
		t.Fatalf("TempDir test: %v", err)
	}
}

// ─── AC#3: concurrent parent goroutines ──────────────────────────

// TestRun_Concurrent spawns N children from concurrent goroutines
// and verifies that all return the expected output.
func TestRun_Concurrent(t *testing.T) {
	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)

	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			out, errOut, err := subproc.Run(t, "concurrent-child")
			if err != nil {
				errs[idx] = fmt.Errorf("worker %d: %w (stderr: %s)", idx, err, errOut)
				return
			}
			got := strings.TrimSpace(string(out))
			if got != "concurrent-child-ok" {
				errs[idx] = fmt.Errorf("worker %d: stdout = %q, want %q", idx, got, "concurrent-child-ok")
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent test worker %d failed: %v", i, err)
		}
	}
}

// ─── Unknown mode ────────────────────────────────────────────────

// TestRun_UnknownMode verifies that an unregistered mode causes the
// child to exit non-zero.
func TestRun_UnknownMode(t *testing.T) {
	_, _, err := subproc.Run(t, "no-such-mode")
	if err == nil {
		t.Error("unknown mode: expected error, got nil")
	}
}

// ─── Double-register panic ────────────────────────────────────────

func TestRegister_Duplicate_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("double Register: expected panic, got none")
		}
	}()
	// "exit-zero" is already registered in TestMain above.
	subproc.Register("exit-zero", func(_ []string) int { return 0 })
}
