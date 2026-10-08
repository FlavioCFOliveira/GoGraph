package crashinject_test

// helper_cleanup_state_test.go — rmp #2527, the link between "the removal works"
// and "the removal is aimed at the right path".
//
// helper_cleanup_internal_test.go proves removeHelperDir removes a real
// populated directory. That is worthless if the path the process-exit hook is
// given is not the path the build actually created, so this file asserts the
// build RECORDS its directory, that the recorded path is the directory the build
// created under the testbin root, and that the cached binary really lives inside it.
//
// Why the post-exit state itself is not asserted here: a test cannot observe its
// own process after os.Exit. The end-to-end evidence is the before/after count
// of gograph-crashinject-* directories across a full `make ci`, and the standing
// regression gate is the wiring scan in helper_cleanup_wiring_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/internal/testbin"
)

// TestHelperBinaryDir_RecordsTheDirectoryTheBuildCreated forces a helper build
// (via a Run that exits immediately on an unknown scenario) and then asserts the
// recorded directory is a real, testbin-rooted, correctly-prefixed directory that
// contains the cached binary.
func TestHelperBinaryDir_RecordsTheDirectoryTheBuildCreated(t *testing.T) {
	// Not parallel: reads package-level state that RemoveHelperBinary mutates.
	if _, err := crashinject.Run(t, "no.such.scenario", crashinject.Opts{}); err != nil {
		t.Fatalf("Run (to force the helper build): %v", err)
	}

	dir := crashinject.HelperBinaryDir()
	if dir == "" {
		t.Fatal("HelperBinaryDir() is empty after a Run; the process-exit hook " +
			"would have nothing to remove and the leak is back")
	}

	// The recorded path must sit directly under the testbin root, which is on
	// disk and outside TMPDIR and GOTMPDIR (rmp #3028), and carry the prefix the
	// guard in internal/tmphygiene lists.
	root, err := testbin.Root()
	if err != nil {
		t.Fatalf("testbin.Root: %v", err)
	}
	binRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	if parent := filepath.Dir(resolved); parent != binRoot {
		t.Errorf("helper dir %q sits in %q, want the testbin root %q", dir, parent, binRoot)
	}
	if base := filepath.Base(dir); !strings.HasPrefix(base, "gograph-crashinject-") {
		t.Errorf("helper dir base = %q, want the gograph-crashinject- prefix "+
			"(internal/tmphygiene counts that prefix; a rename here makes the guard blind)", base)
	}

	// The directory must exist and hold the binary, i.e. it is genuinely the
	// thing worth removing rather than a stale string.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q): %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	var found string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "crashinject-helper") {
			found = e.Name()
		}
	}
	if found == "" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no crashinject-helper binary in %q; entries: %v", dir, names)
	}
	t.Logf("recorded helper dir %q holds %q — this is the path RemoveHelperBinary deletes at process exit", dir, found)
}
