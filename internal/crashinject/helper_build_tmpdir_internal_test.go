package crashinject

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildHelper_LeavesNothingUnderTMPDIR is the regression gate for rmp #3028.
// Tests may run with TMPDIR and GOTMPDIR on a RAM drive that must hold graph
// data files only. The helper build must therefore put neither its binary nor
// the child `go build` work files there. The test points both variables at an
// empty probe directory, builds the helper, and walks the probe: any entry is a
// leak. The binary must exist outside the probe, so a build that produced
// nothing cannot pass.
func TestBuildHelper_LeavesNothingUnderTMPDIR(t *testing.T) {
	// Not parallel: t.Setenv changes the process environment.
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}
	probe := t.TempDir()
	t.Setenv("TMPDIR", probe)
	t.Setenv("GOTMPDIR", probe)

	dir, bin, err := buildHelper(root)
	if dir != "" {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if err != nil {
		t.Fatalf("buildHelper: %v", err)
	}

	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("helper binary %q: %v", bin, err)
	}
	if strings.HasPrefix(bin, probe+string(filepath.Separator)) {
		t.Errorf("helper binary %q is under TMPDIR %q", bin, probe)
	}

	var leaked []string
	walkErr := filepath.WalkDir(probe, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != probe {
			leaked = append(leaked, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %q: %v", probe, walkErr)
	}
	if len(leaked) > 0 {
		t.Errorf("helper build left %d entries under TMPDIR/GOTMPDIR %q: %v", len(leaked), probe, leaked)
	}
}
