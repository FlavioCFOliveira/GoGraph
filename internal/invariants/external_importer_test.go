package invariants

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInvariantsHasExternalImporter guards #1446: the invariant checkers must
// have at least one consumer OUTSIDE this package. Before the shapegen wiring
// they had none — the package was "paper coverage", documented as a battery
// component but never run on the library's actual output. If a future refactor
// removes the last external importer, this test fails so the regression is
// visible immediately rather than silently re-stranding the checkers.
func TestInvariantsHasExternalImporter(t *testing.T) {
	const importPath = "github.com/FlavioCFOliveira/GoGraph/internal/invariants"

	root := repoRoot(t)
	selfDir := filepath.Join(root, "internal", "invariants")

	skip := map[string]struct{}{
		".git": {}, "testdata": {}, "gen": {}, "vendor": {}, "dist": {}, "bin": {},
	}

	var importers []string
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a per-entry walk error must not abort the scan
		}
		if d.IsDir() {
			if _, ok := skip[d.Name()]; ok {
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		// Files in this package are not "external".
		if filepath.Dir(path) == selfDir {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil //nolint:nilerr // unparseable file is not this test's concern
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == importPath {
				rel, _ := filepath.Rel(root, path)
				importers = append(importers, rel)
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking repo: %v", walkErr)
	}

	if len(importers) == 0 {
		t.Errorf("internal/invariants has no external importer; the checkers are "+
			"paper coverage again (#1446). Wire them into a property/round-trip "+
			"test (see internal/shapegen/invariants_battery_test.go).\nimport path: %s",
			importPath)
	} else {
		t.Logf("internal/invariants is imported by %d external file(s): %s",
			len(importers), strings.Join(importers, ", "))
	}
}

// repoRoot walks up from the test's working directory until it finds go.mod.
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
