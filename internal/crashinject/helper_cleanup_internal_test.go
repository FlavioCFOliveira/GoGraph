package crashinject

// helper_cleanup_internal_test.go — rmp #2527.
//
// buildHelperOnce places a ~15 MB crashinject-helper binary in a fresh
// os.MkdirTemp directory once per test process, and nothing ever removed it.
// Two directories were stranded by every `make ci` (one per suite run: the
// -race pass and the coverage pass) and 697 had accumulated by the time the
// temp volume filled. The consequence was not untidiness: with the volume full,
// every WAL append fails with ENOSPC, which the WAL correctly reports as a
// poisoned writer that discarded its un-synced suffix — indistinguishable, to
// anyone reading the log, from a real durability defect.
//
// This file gates the removal MECHANISM. The wiring of that mechanism into every
// caller package's TestMain is gated separately, in
// helper_cleanup_wiring_test.go.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveHelperDir asserts the removal actually removes, and that it is
// idempotent. Idempotence is load-bearing rather than cosmetic: the hook runs
// from TestMain, where a second `make ci` in the same checkout, a manual prune,
// or a repeated call must not turn tidy-up into a test failure.
func TestRemoveHelperDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup returns the path handed to removeHelperDir.
		setup func(t *testing.T) string
	}{
		{
			name: "populated directory",
			setup: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "helper")
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				// A binary-shaped payload plus a nested directory, so the
				// removal is exercised recursively rather than on an empty dir.
				if err := os.WriteFile(filepath.Join(dir, "crashinject-helper"), []byte("\x7fELF stub"), 0o600); err != nil {
					t.Fatalf("write helper stub: %v", err)
				}
				nested := filepath.Join(dir, "nested")
				if err := os.Mkdir(nested, 0o750); err != nil {
					t.Fatalf("mkdir nested: %v", err)
				}
				if err := os.WriteFile(filepath.Join(nested, "x"), []byte("x"), 0o600); err != nil {
					t.Fatalf("write nested: %v", err)
				}
				return dir
			},
		},
		{
			name: "absent directory is not an error",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "never-created")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := tc.setup(t)

			if err := removeHelperDir(dir); err != nil {
				t.Fatalf("removeHelperDir: %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("after removeHelperDir, Stat(%q) err = %v; want IsNotExist", dir, err)
			}
			// Second call on the same path: the hook must stay idempotent.
			if err := removeHelperDir(dir); err != nil {
				t.Fatalf("removeHelperDir (second call): %v", err)
			}
		})
	}
}

// TestRemoveHelperBinary_NoOpWithoutABuild asserts the exported hook is safe to
// install unconditionally. store/recovery calls it from TestMain in the DEFAULT
// build, where its crash-injection tests are compiled out by build tag and no
// helper is ever built; if the hook were not a no-op there it would either panic
// or delete something it does not own.
//
// It runs on a saved-and-restored copy of the package state rather than the live
// value, so it cannot destroy the cached binary that the other tests in this
// process rely on.
func TestRemoveHelperBinary_NoOpWithoutABuild(t *testing.T) {
	// Not parallel: it mutates package state that RemoveHelperBinary reads.
	savedDir, savedPath := helperBinDir, helperBinPath
	t.Cleanup(func() { helperBinDir, helperBinPath = savedDir, savedPath })

	helperBinDir, helperBinPath = "", ""
	RemoveHelperBinary() // must not panic and must leave the empty state alone
	if got := HelperBinaryDir(); got != "" {
		t.Fatalf("HelperBinaryDir() = %q after a no-op removal; want \"\"", got)
	}
}
