package crashinject_test

// helper_cleanup_wiring_test.go — rmp #2527, the standing regression gate.
//
// The mechanism gates (helper_cleanup_internal_test.go,
// helper_cleanup_state_test.go) prove the removal removes, and that it is aimed
// at the directory the build created. Neither notices the way this leak actually
// comes back: a NEW package starts calling crashinject.Run, builds its own
// helper in its own test process, and never installs the process-exit hook.
// Nothing in that package fails, so the leak returns silently — which is exactly
// how it went unnoticed for 697 directories.
//
// This gate reads the module's own source and asserts the invariant directly:
// every package directory that calls crashinject.Run also installs
// crashinject.RemoveHelperBinary.

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	// runMarker is the call that causes a helper binary to be built in the
	// calling package's test process.
	runMarker = "crashinject.Run("
	// cleanupMarker is the process-exit hook that removes it. Matching the
	// identifier rather than a full call expression keeps the gate indifferent
	// to how the caller wires it (goleak.Cleanup, a bare TestMain, a helper).
	cleanupMarker = "crashinject.RemoveHelperBinary"
)

// crashinjectWiring reports which package directories under fsys call
// crashinject.Run, and which of those omit the RemoveHelperBinary hook.
//
// Comment lines are ignored, so a package that merely DOCUMENTS the harness is
// not dragged into importing it. Directories are keyed by their parent path
// because the hook is installed in a package's TestMain, i.e. per directory, not
// per file.
func crashinjectWiring(fsys fs.FS) (callers, missing []string, err error) {
	// A directory is a caller if any of its .go files calls Run, and is hooked
	// if any of its .go files names RemoveHelperBinary.
	isCaller := map[string]bool{}
	isHooked := map[string]bool{}

	walkErr := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS/editor/build output: they hold no module source and
			// walking them is pure cost.
			switch base := path.Base(p); {
			case p == ".":
				return nil
			case strings.HasPrefix(base, "."),
				base == "dist", base == "soak-artefacts", base == "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, readErr := fs.ReadFile(fsys, p)
		if readErr != nil {
			return readErr
		}
		dir := path.Dir(p)
		for line := range strings.Lines(string(b)) {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, runMarker) {
				isCaller[dir] = true
			}
			if strings.Contains(line, cleanupMarker) {
				isHooked[dir] = true
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}

	for dir := range isCaller {
		callers = append(callers, dir)
		if !isHooked[dir] {
			missing = append(missing, dir)
		}
	}
	slices.Sort(callers)
	slices.Sort(missing)
	return callers, missing, nil
}

// TestHelperCleanup_WiredInEveryCallerPackage is the live gate over this module.
func TestHelperCleanup_WiredInEveryCallerPackage(t *testing.T) {
	t.Parallel()

	root := moduleRootForTest(t)
	callers, missing, err := crashinjectWiring(os.DirFS(root))
	if err != nil {
		t.Fatalf("scan module source: %v", err)
	}

	// NON-VACUITY, asserted separately from the verdict. A scan that found no
	// caller at all would report "nothing missing" and look green while proving
	// nothing — the failure mode this project calls an oracle that cannot fail.
	// internal/crashinject itself always qualifies, so the floor is 1.
	if len(callers) == 0 {
		t.Fatal("the scan found no package calling crashinject.Run; the markers or " +
			"the walk are broken, so the verdict below is vacuous")
	}
	t.Logf("scanned %d package(s) calling %s: %v", len(callers), runMarker, callers)

	if len(missing) != 0 {
		t.Errorf("these packages call %s but never install %s, so each of their test "+
			"processes strands a ~15 MB helper directory in the system temp area "+
			"(rmp #2527): %v\n\nAdd the hook to the package's TestMain; see the "+
			"RemoveHelperBinary godoc for the goleak.Cleanup form.",
			runMarker, cleanupMarker, missing)
	}
}

// TestCrashinjectWiring_DetectsAnUnhookedCaller is the sensitivity proof: the
// gate above is only worth having if it fails when the invariant is broken. The
// fixtures are synthetic filesystems, so the proof needs no file planted in the
// real tree.
func TestCrashinjectWiring_DetectsAnUnhookedCaller(t *testing.T) {
	t.Parallel()

	const callFile = "func TestX(t *testing.T) { crashinject.Run(t, \"s\", crashinject.Opts{}) }\n"
	const hookFile = "func TestMain(m *testing.M) { crashinject.RemoveHelperBinary() }\n"

	tests := []struct {
		name        string
		files       fstest.MapFS
		wantCallers []string
		wantMissing []string
	}{
		{
			name: "caller with the hook is accepted",
			files: fstest.MapFS{
				"pkg/a/a_test.go":    {Data: []byte(callFile)},
				"pkg/a/main_test.go": {Data: []byte(hookFile)},
			},
			wantCallers: []string{"pkg/a"},
			wantMissing: nil,
		},
		{
			name: "caller without the hook is REPORTED",
			files: fstest.MapFS{
				"pkg/b/b_test.go": {Data: []byte(callFile)},
			},
			wantCallers: []string{"pkg/b"},
			wantMissing: []string{"pkg/b"},
		},
		{
			name: "the hook in a SIBLING directory does not count",
			files: fstest.MapFS{
				"pkg/c/c_test.go":    {Data: []byte(callFile)},
				"pkg/d/main_test.go": {Data: []byte(hookFile)},
			},
			wantCallers: []string{"pkg/c"},
			wantMissing: []string{"pkg/c"},
		},
		{
			name: "a comment mentioning Run is not a call",
			files: fstest.MapFS{
				"pkg/e/doc.go": {Data: []byte("// crashinject.Run( is described here.\npackage e\n")},
			},
			wantCallers: nil,
			wantMissing: nil,
		},
		{
			name: "non-Go files are ignored",
			files: fstest.MapFS{
				"pkg/f/README.md": {Data: []byte(callFile)},
			},
			wantCallers: nil,
			wantMissing: nil,
		},
		{
			name: "testdata is skipped",
			files: fstest.MapFS{
				"pkg/g/testdata/x.go": {Data: []byte(callFile)},
			},
			wantCallers: nil,
			wantMissing: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			callers, missing, err := crashinjectWiring(tc.files)
			if err != nil {
				t.Fatalf("crashinjectWiring: %v", err)
			}
			if !slices.Equal(callers, tc.wantCallers) {
				t.Errorf("callers = %v, want %v", callers, tc.wantCallers)
			}
			if !slices.Equal(missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", missing, tc.wantMissing)
			}
		})
	}
}

// moduleRootForTest walks up from the working directory to the directory holding
// go.mod, matching the helper used by the other repo-level gates in this module
// (internal/docscheck, internal/scriptgate).
func moduleRootForTest(t *testing.T) string {
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
