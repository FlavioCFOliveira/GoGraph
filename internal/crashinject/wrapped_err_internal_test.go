package crashinject

// wrapped_err_internal_test.go — T944: isExitError must detect an
// *exec.ExitError even when callers wrap it via fmt.Errorf("...: %w", err).

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// TestIsExitError_NilError reports false on nil input. Documenting the
// nil-safe contract makes the helper safe to call unconditionally.
func TestIsExitError_NilError(t *testing.T) {
	var target *exec.ExitError
	if isExitError(nil, &target) {
		t.Fatal("nil error should not match")
	}
	if target != nil {
		t.Fatal("nil error must not write *target")
	}
}

// TestIsExitError_UnrelatedError reports false on a non-exec error.
func TestIsExitError_UnrelatedError(t *testing.T) {
	var target *exec.ExitError
	if isExitError(errors.New("plain"), &target) {
		t.Fatal("plain error should not match")
	}
}

// TestIsExitError_Wrapped exercises the regression that motivated this
// task: a real *exec.ExitError wrapped with fmt.Errorf("%w", ...) must
// still be detected by isExitError. The previous implementation used a
// direct type assertion and would silently fail here.
func TestIsExitError_Wrapped(t *testing.T) {
	cmd := exec.Command("false") // exits with code 1
	runErr := cmd.Run()
	if runErr == nil {
		t.Skip("'false' command not available or did not exit non-zero")
	}
	wrapped := fmt.Errorf("subprocess died: %w", runErr)

	var target *exec.ExitError
	if !isExitError(wrapped, &target) {
		t.Fatalf("wrapped *exec.ExitError must be detected via errors.As; got false (err=%v)", wrapped)
	}
	if target == nil {
		t.Fatal("wrapped match must populate *target")
	}
	if target.ExitCode() != 1 {
		t.Errorf("wrapped match must yield the right exit code; got %d, want 1", target.ExitCode())
	}
}
