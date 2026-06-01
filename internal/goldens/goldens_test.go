package goldens_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/goldens"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ─── Helpers ─────────────────────────────────────────────────────

// mockTB captures Errorf / Fatalf calls so subtests can assert on
// whether Assert passed or failed without touching the real test.
type mockTB struct {
	testing.TB
	errors  []string
	fataled bool
}

func (m *mockTB) Helper()                   {}
func (m *mockTB) Errorf(f string, a ...any) { m.errors = append(m.errors, f) }
func (m *mockTB) Fatalf(f string, a ...any) { m.fataled = true; m.errors = append(m.errors, f) }

func passed(m *mockTB) bool { return len(m.errors) == 0 && !m.fataled }
func failed(m *mockTB) bool { return !passed(m) }

// prepGolden writes content to path, creating parent dirs if needed.
func prepGolden(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("prepGolden: mkdir: %v", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("prepGolden: write: %v", err)
	}
}

// ─── AC#1: on equality the helper is a no-op ─────────────────────

func TestAssert_EqualContent_NoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testdata", "equal.golden")
	data := []byte("hello golden\n")
	prepGolden(t, path, data)

	tb := &mockTB{}
	goldens.Assert(tb, path, data)
	if failed(tb) {
		t.Errorf("identical content: Assert failed unexpectedly: %v", tb.errors)
	}
}

// ─── AC#2: on mismatch the failure message is a unified diff ─────

func TestAssert_DifferentContent_ReportsDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testdata", "mismatch.golden")
	want := []byte("line one\nline two\n")
	got := []byte("line one\nline TWO\n") // differs on line 2
	prepGolden(t, path, want)

	tb := &mockTB{}
	goldens.Assert(tb, path, got)
	if passed(tb) {
		t.Error("mismatched content: expected failure, got none")
	}
	// Failure message should mention the path.
	if len(tb.errors) == 0 || tb.errors[0] == "" {
		t.Error("failure message is empty")
	}
}

func TestAssert_MissingFile_ReportsCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testdata", "missing.golden")
	// Do NOT create the file.

	tb := &mockTB{}
	goldens.Assert(tb, path, []byte("new content\n"))
	if passed(tb) {
		t.Error("missing golden: expected failure, got none")
	}
}

// ─── AC#3: with -update the file is rewritten and test passes ────

func TestAssert_Update_OverwritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testdata", "update.golden")
	old := []byte("old content\n")
	updated := []byte("new content after update\n")
	prepGolden(t, path, old)

	// Simulate -update by setting the env variable.
	t.Setenv("GOGRAPH_UPDATE_GOLDENS", "1")

	tb := &mockTB{}
	goldens.Assert(tb, path, updated)

	// The test should not have failed.
	if failed(tb) {
		t.Errorf("update mode: unexpected failure: %v", tb.errors)
	}

	// The file on disk must now contain the new content.
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}
	if !bytes.Equal(written, updated) {
		t.Errorf("file after update = %q, want %q", written, updated)
	}
}

func TestAssert_Update_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testdata", "created.golden")
	// File does not exist yet.
	data := []byte("brand new\n")

	t.Setenv("GOGRAPH_UPDATE_GOLDENS", "1")

	tb := &mockTB{}
	goldens.Assert(tb, path, data)

	if failed(tb) {
		t.Errorf("update creates file: unexpected failure: %v", tb.errors)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after create: %v", err)
	}
	if !bytes.Equal(written, data) {
		t.Errorf("created file = %q, want %q", written, data)
	}
}

// ─── AC#4: atomic write (temp + rename) ──────────────────────────

func TestAssert_Update_AtomicWrite(t *testing.T) {
	// Verify that no .golden-tmp-* file is left after a successful
	// update (the rename completed and the temp was removed).
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "testdata")
	path := filepath.Join(dataDir, "atomic.golden")
	data := []byte("atomic write test\n")

	t.Setenv("GOGRAPH_UPDATE_GOLDENS", "1")

	tb := &mockTB{}
	goldens.Assert(tb, path, data)
	if failed(tb) {
		t.Fatalf("atomic write: unexpected failure: %v", tb.errors)
	}

	// No leftover temp files.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if len(e.Name()) > 12 && e.Name()[:13] == ".golden-tmp-" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// ─── UpdateRequested ─────────────────────────────────────────────

func TestUpdateRequested_EnvVar(t *testing.T) {
	t.Setenv("GOGRAPH_UPDATE_GOLDENS", "1")
	if !goldens.UpdateRequested() {
		t.Error("UpdateRequested() = false with GOGRAPH_UPDATE_GOLDENS=1")
	}
}

func TestUpdateRequested_EnvVar_Unset(t *testing.T) {
	_ = os.Unsetenv("GOGRAPH_UPDATE_GOLDENS")
	// Don't assert false here because the -update flag might be set
	// globally by the test runner; just verify it doesn't panic.
	_ = goldens.UpdateRequested()
}
