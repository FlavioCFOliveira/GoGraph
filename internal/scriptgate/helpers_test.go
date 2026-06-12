// Package scriptgate runs the self-contained shell gate tests under
// `go test`, so the release/CI shell gates (check_doc_freshness.sh,
// release_soak_gate.sh) are continuously exercised by the normal test
// suite rather than living as orphaned, run-by-hand scripts.
//
// The package is test-only by design.
package scriptgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the test's working directory until it finds the
// directory containing go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}

// runShellGate runs the given repo-relative bash script and returns its
// combined output. It skips the test when bash is unavailable (a
// sanctioned environment-precondition skip).
func runShellGate(t *testing.T, relPath string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	script := filepath.Join(repoRoot(t), relPath)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("gate script %s not found: %v", relPath, err)
	}
	cmd := exec.Command("bash", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
