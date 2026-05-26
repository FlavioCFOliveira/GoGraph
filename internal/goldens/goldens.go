// Package goldens provides a uniform golden-file assertion helper for
// tests that compare byte-for-byte output against stored fixtures.
//
// # Usage
//
//	func TestFoo(t *testing.T) {
//	    got := render(...)
//	    goldens.Assert(t, "testdata/foo.golden", got)
//	}
//
// On the first run (no fixture file) the test fails with a clear
// message instructing the developer to run with -update:
//
//	go test -run TestFoo -update
//
// The -update flag (or GOGRAPH_UPDATE_GOLDENS=1) causes Assert to
// overwrite the fixture file with the current output and mark the
// test as passed. The write is atomic (temp file + rename) so an
// interrupted run never corrupts an existing golden.
//
// # Path resolution
//
// path is resolved relative to the caller's package directory
// (using runtime/debug to locate the file). Absolute paths are
// used as-is; relative paths are resolved against the directory
// that contains the calling test file (i.e. the package root from
// the test's perspective).
//
// # Concurrency
//
// Assert and UpdateRequested are safe for concurrent use; the
// flag is read once at package init time and never modified.
package goldens

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// updateFlag is the -update command-line flag registered at init.
// We do not use flag.Bool because the package may be imported by
// many test binaries; registering once is sufficient.
var updateFlag = func() *bool {
	// flag.Lookup guards against double-registration when multiple
	// test packages import goldens in the same binary.
	if f := flag.Lookup("update"); f != nil {
		v, _ := f.Value.(interface{ Get() any }).Get().(bool)
		return &v
	}
	return flag.Bool("update", false, "overwrite golden files with current output")
}()

// UpdateRequested reports whether the test binary was started with
// -update or the environment variable GOGRAPH_UPDATE_GOLDENS=1.
// It is safe to call before flag.Parse (e.g. from TestMain); the
// environment variable path is always available.
func UpdateRequested() bool {
	if os.Getenv("GOGRAPH_UPDATE_GOLDENS") == "1" {
		return true
	}
	// updateFlag is nil only if the flag package was not yet
	// initialised, which cannot happen after package init.
	if updateFlag != nil && *updateFlag {
		return true
	}
	return false
}

// Assert compares got against the golden file at path. If the file
// does not exist or its content differs from got, the test fails
// with a unified diff. When -update or GOGRAPH_UPDATE_GOLDENS=1 is
// set the file is overwritten with got and the test continues.
//
// path may be absolute or relative. Relative paths are resolved
// relative to the directory of the calling test's source file, which
// is equivalent to the package root under which the test runs.
func Assert(t testing.TB, path string, got []byte) {
	t.Helper()

	if !filepath.IsAbs(path) {
		// Resolve relative to the caller's source directory.
		_, callerFile, _, ok := runtime.Caller(1)
		if ok {
			path = filepath.Join(filepath.Dir(callerFile), path)
		}
	}

	if UpdateRequested() {
		if err := writeAtomic(path, got); err != nil {
			t.Fatalf("goldens.Assert: update %q: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path) // #nosec G304 — caller-supplied test fixture path.
	if os.IsNotExist(err) {
		t.Errorf("goldens.Assert: golden file %q does not exist; run with -update to create it\ngot:\n%s",
			path, got)
		return
	}
	if err != nil {
		t.Fatalf("goldens.Assert: read %q: %v", path, err)
	}

	if bytes.Equal(want, got) {
		return // exact match — pass
	}

	// Produce a unified diff for readable failure output.
	t.Errorf("goldens.Assert: %q mismatch:\n%s", path, unifiedDiff(want, got))
}

// writeAtomic writes data to path atomically: it writes to a
// temporary file in the same directory and then renames it over
// the destination so an interrupted write cannot leave a corrupt
// golden file on disk.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".golden-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// unifiedDiff produces a simple line-level unified diff between want
// and got. It is intentionally minimal — suitable for test output
// — and does not require an external diff binary.
func unifiedDiff(want, got []byte) string {
	wantLines := splitLines(want)
	gotLines := splitLines(got)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "--- want\n+++ got\n")

	// Naive line-by-line comparison; no LCS heuristic.
	maxLen := max(len(wantLines), len(gotLines))
	for i := range maxLen {
		w := lineAt(wantLines, i)
		g := lineAt(gotLines, i)
		if w == g {
			fmt.Fprintf(&buf, " %s\n", w)
		} else {
			if w != "" {
				fmt.Fprintf(&buf, "-%s\n", w)
			}
			if g != "" {
				fmt.Fprintf(&buf, "+%s\n", g)
			}
		}
	}
	return buf.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	// Trim trailing newline to avoid a spurious empty last line.
	if s != "" && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
