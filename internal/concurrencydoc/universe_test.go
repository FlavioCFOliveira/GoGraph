package concurrencydoc

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestScanUniverseMatchesGoList guards #1449: the set of packages the scanner
// walks must equal the public-package set computed from `go list ./...` (minus
// the same non-public exclusions). This is what makes the universe complete —
// the old hardcoded six-tree list silently excluded the public `metrics`
// package and the module root, and would miss any future top-level package.
//
// Equality is asserted both ways: a package in `go list` but not scanned means
// the universe has a blind spot (the original defect); a package scanned but
// not in `go list` means the scanner invented a non-package directory.
func TestScanUniverseMatchesGoList(t *testing.T) {
	res := scanRepo(t)
	scanned := make(map[string]struct{}, len(res.Packages))
	for _, p := range res.Packages {
		scanned[p] = struct{}{}
	}

	expected := publicPackagesFromGoList(t)

	var missing, extra []string
	for p := range expected {
		if _, ok := scanned[p]; !ok {
			missing = append(missing, p)
		}
	}
	for p := range scanned {
		if _, ok := expected[p]; !ok {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("public package(s) present in `go list` but NOT scanned (concurrency "+
			"mandate blind spot — extend the scan universe):\n  %s", strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("package(s) scanned but absent from `go list` (the scanner is walking a "+
			"non-package directory):\n  %s", strings.Join(extra, "\n  "))
	}
}

// publicPackagesFromGoList returns the repository-relative import paths of the
// public packages, computed authoritatively from `go list ./...` with the same
// exclusions the scanner applies (excludedTopLevel + gen/testdata components).
// The module root maps to ".".
func publicPackagesFromGoList(t *testing.T) map[string]struct{} {
	t.Helper()
	root := scanRepoRoot(t)

	modulePath := runGo(t, root, "list", "-m")
	// {{if .GoFiles}} emits a package only when it has non-test source files,
	// matching the scanner's rule (it records a package only when the dir has
	// at least one non-test .go file). This excludes test-only directories
	// such as graph/io (which holds a single package io_test cross-package
	// round-trip test and no library code).
	listOut := runGo(t, root, "list", "-f", "{{if .GoFiles}}{{.ImportPath}}{{end}}", "./...")

	out := make(map[string]struct{})
	for _, line := range strings.Split(listOut, "\n") {
		imp := strings.TrimSpace(line)
		if imp == "" {
			continue
		}
		rel := strings.TrimPrefix(imp, modulePath)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			rel = "." // module root (package gograph)
		}
		if excludedRel(rel) {
			continue
		}
		out[rel] = struct{}{}
	}
	return out
}

// excludedRel reports whether a repository-relative import path is excluded
// from the scan, mirroring excludedTopLevel and skipDirComponents in scan.go.
func excludedRel(rel string) bool {
	if rel == "." {
		return false
	}
	parts := strings.Split(rel, "/")
	if _, ok := excludedTopLevel[parts[0]]; ok {
		return true
	}
	for _, p := range parts {
		if _, ok := skipDirComponents[p]; ok {
			return true
		}
	}
	return false
}

// scanRepoRoot locates the repository root from the test's working directory.
func scanRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := RepoRoot(wd)
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return root
}

// runGo runs `go <args...>` in dir and returns trimmed stdout, failing the
// test on error.
func runGo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}
