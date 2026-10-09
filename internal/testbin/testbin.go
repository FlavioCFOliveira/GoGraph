// Package testbin builds the executables that test harnesses run as child
// processes, and keeps every such executable and every `go` build temporary on
// disk, outside the test process's TMPDIR and GOTMPDIR.
//
// # Why
//
// Tests may run with TMPDIR and GOTMPDIR pointed at a RAM drive that must hold
// graph data files only (see CLAUDE.md, "Tests and validation"). A harness that
// builds a child binary with os.MkdirTemp("", …) and an inherited environment
// writes the binary, and the child `go` command's work directory, to that RAM
// drive. This package is the single seam that prevents it:
//
//   - [MkdirTemp] and [TempDir] create directories under [Root], a fixed
//     location in the user cache directory, never under TMPDIR or GOTMPDIR.
//   - [Go] runs the `go` command with TMPDIR and GOTMPDIR reset, in the
//     child's environment only, to a fresh directory under [Root] that it
//     removes before returning.
//
// # Ownership
//
// The caller owns every directory [MkdirTemp] returns and must remove it.
// [TempDir] registers that removal with the test's Cleanup. A process killed
// before its cleanup runs leaves its directory under [Root].
//
// # Concurrency
//
// Every function is safe for concurrent use by multiple goroutines and by
// multiple processes: each call creates its own uniquely named directory and
// shares no mutable state.
package testbin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// rootName is the directory under os.UserCacheDir that holds every testbin
// directory.
const rootName = "gograph-testbin"

// Root returns the disk directory under which every testbin directory is
// created, creating it when absent: <os.UserCacheDir()>/gograph-testbin.
func Root() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("testbin: user cache dir: %w", err)
	}
	root := filepath.Join(cache, rootName)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("testbin: create root %q: %w", root, err)
	}
	return root, nil
}

// MkdirTemp creates a new, uniquely named directory under [Root], with pattern
// interpreted as by os.MkdirTemp, and returns its path. The caller owns the
// directory and must remove it.
func MkdirTemp(pattern string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, pattern)
	if err != nil {
		return "", fmt.Errorf("testbin: mkdir temp: %w", err)
	}
	return dir, nil
}

// TB is the subset of testing.TB that [TempDir] uses. It is declared here so
// that non-test packages may import testbin without linking package testing.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(f func())
}

// TempDir creates a directory with [MkdirTemp] and registers its removal with
// tb.Cleanup. It fails the test when the directory cannot be created.
func TempDir(tb TB) string {
	tb.Helper()
	dir, err := MkdirTemp("bin-*")
	if err != nil {
		tb.Fatalf("%v", err)
	}
	tb.Cleanup(func() {
		// Best-effort: a leftover directory under Root wastes disk but cannot
		// affect any test result.
		_ = os.RemoveAll(dir)
	})
	return dir
}

// Go runs `go args...` with working directory dir and returns its standard
// output and standard error. The child inherits the current environment except
// TMPDIR and GOTMPDIR, which point to a fresh directory under [Root] that Go
// removes before returning, so the `go` command's work files never reach the
// caller's TMPDIR or GOTMPDIR. The command is killed when ctx is done.
func Go(ctx context.Context, dir string, args ...string) (stdout, stderr []byte, err error) {
	tmp, err := MkdirTemp("gotmp-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil && err == nil {
			err = fmt.Errorf("testbin: remove go temp dir %q: %w", tmp, rmErr)
		}
	}()

	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // G204: callers pass fixed go-tool argv assembled from literals.
	cmd.Dir = dir
	cmd.Env = childEnv(os.Environ(), tmp)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if runErr := cmd.Run(); runErr != nil {
		return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("go %s: %w", strings.Join(args, " "), runErr)
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}

// childEnv returns env without any TMPDIR or GOTMPDIR entry, followed by both
// variables set to tmp.
func childEnv(env []string, tmp string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMPDIR=") || strings.HasPrefix(kv, "GOTMPDIR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TMPDIR="+tmp, "GOTMPDIR="+tmp)
}
