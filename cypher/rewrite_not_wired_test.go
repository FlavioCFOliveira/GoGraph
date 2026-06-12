package cypher_test

// rewrite_not_wired_test.go — guard test for task #1382.
//
// The cypher/ir/rewrite package is an experimental, not-yet-wired optimisation
// framework. This test asserts it is NOT in the transitive import graph of the
// production cypher package, so an accidental import would be caught by CI
// before it silently changes engine behaviour.
//
// Gate test semantics:
//   Before fix: no such test existed — accidental wiring was uncaught.
//   After fix:  this test fails if cypher/ir/rewrite is ever added to the
//               production import graph.

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCypherEngine_RewritePackageNotWired(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available; skipping import-graph guard")
	}

	// Resolve the module root relative to this test file.
	// This file lives at cypher/rewrite_not_wired_test.go, so
	// filepath.Dir(file) is the cypher/ directory and one level up is the
	// module root.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	moduleRoot := filepath.Join(filepath.Dir(file), "..")

	cmd := exec.Command("go", "list", "-f", `{{range .Deps}}{{println .}}{{end}}`,
		"github.com/FlavioCFOliveira/GoGraph/cypher")
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	const rewritePkg = "github.com/FlavioCFOliveira/GoGraph/cypher/ir/rewrite"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == rewritePkg {
			t.Errorf("cypher/ir/rewrite is in the production engine's import graph; " +
				"it must remain unwired (experimental API). See cypher/ir/rewrite/doc.go")
			return
		}
	}
}
