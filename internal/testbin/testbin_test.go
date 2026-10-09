package testbin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGo_ChildTempDirsAreNotTheCallers proves that the `go` command run by [Go]
// sees TMPDIR and GOTMPDIR under [Root], not the caller's values (rmp #3028).
// A post-build walk of TMPDIR cannot see this: `go build` removes its work
// directory on success. Asking the child for its own view can. The test also
// asserts that the child's directory is gone once Go returns.
func TestGo_ChildTempDirsAreNotTheCallers(t *testing.T) {
	// Not parallel: t.Setenv changes the process environment.
	probe := t.TempDir()
	t.Setenv("TMPDIR", probe)
	t.Setenv("GOTMPDIR", probe)

	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	stdout, stderr, err := Go(context.Background(), ".", "env", "GOTMPDIR")
	if err != nil {
		t.Fatalf("Go: %v\n%s", err, stderr)
	}
	child := strings.TrimSpace(string(stdout))
	if filepath.Dir(child) != root {
		t.Errorf("child GOTMPDIR = %q, want a directory directly under %q (caller's value %q)", child, root, probe)
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Errorf("child GOTMPDIR %q still present after Go returned (stat err: %v)", child, err)
	}
}
