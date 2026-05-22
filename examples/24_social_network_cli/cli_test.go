package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain wraps every test with go.uber.org/goleak so the package's
// integration tests verify the CLI does not leak goroutines past the
// last subcommand close. Per-package leak checks catch silent
// regressions in the WAL writer, recovery, and the Cypher engine
// without needing a goleak call inside every test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestCLIRoundTrip walks the data directory through the full subcommand
// cycle exactly as a user would (init → seed → query → snapshot →
// reopen → stats), captures stdout for each step, and compares the
// byte stream against the golden fixtures under testdata/. The test
// runs entirely in one process so the maphash-based mapper produces
// stable NodeIDs across the in-process recovery cycles — see doc.go's
// "Persistence Contract" section for the rationale.
func TestCLIRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. init ───────────────────────────────────────────────────────────
	gotInit := captureStdout(t, func() {
		if err := dispatch([]string{"init", "-d", dir}); err != nil {
			t.Fatalf("init: %v", err)
		}
	})
	wantInit := readGolden(t, "init_output.json")
	assertEqualWithDir(t, gotInit, wantInit, dir, "init")

	// 2. seed ───────────────────────────────────────────────────────────
	gotSeed := captureStdout(t, func() {
		if err := dispatch([]string{"seed", "-d", dir}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})
	wantSeed := readGolden(t, "seed_output.json")
	assertEqualWithDir(t, gotSeed, wantSeed, dir, "seed")

	// 3. query — representative MATCH against the seeded graph ──────────
	var qbuf bytes.Buffer
	qbuf.Reset()
	q := `MATCH (u:User) RETURN u.username AS username, u.display_name AS display_name ORDER BY username`
	if err := runQuery(ctx, dir, q, &qbuf); err != nil {
		t.Fatalf("query: %v", err)
	}
	wantQuery := readGolden(t, "query_users.jsonl")
	if qbuf.String() != wantQuery {
		t.Fatalf("query mismatch:\n  got:\n%s\n  want:\n%s", qbuf.String(), wantQuery)
	}

	// 4. snapshot ───────────────────────────────────────────────────────
	gotSnap := captureStdout(t, func() {
		if err := dispatch([]string{"snapshot", "-d", dir}); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
	})
	wantSnap := readGolden(t, "snapshot_output.json")
	assertEqualWithDir(t, gotSnap, wantSnap, dir, "snapshot")

	// 5. stats — exercised after the snapshot to demonstrate that the
	//    counts survive a recovery cycle within the same process. The
	//    single-process maphash seed keeps NodeIDs stable, so the byte
	//    stream matches the golden exactly.
	gotStats := captureStdout(t, func() {
		if err := dispatch([]string{"stats", "-d", dir}); err != nil {
			t.Fatalf("stats: %v", err)
		}
	})
	wantStats := readGolden(t, "seed_stats.json")
	if gotStats != wantStats {
		t.Fatalf("stats mismatch:\n  got:  %s\n  want: %s", gotStats, wantStats)
	}
}

// TestCLI_DispatchHelp confirms the help-by-default path emits the
// help text to stdout when the "help" subcommand is used explicitly,
// and the exit-code mapping treats unknown subcommands as usage errors.
func TestCLI_DispatchHelp(t *testing.T) {
	out := captureStdout(t, func() {
		if err := dispatch([]string{"help"}); err != nil {
			t.Fatalf("help: %v", err)
		}
	})
	for _, sub := range []string{"init", "seed", "query", "snapshot", "stats"} {
		if !strings.Contains(out, sub) {
			t.Fatalf("help output missing subcommand %q\n%s", sub, out)
		}
	}
	if err := dispatch([]string{"bogus-subcommand"}); err == nil {
		t.Fatalf("expected usage error for unknown subcommand, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything fn wrote to it as a string. Used by the round-trip test
// because the subcommand entry points emit through os.Stdout rather
// than an injected writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = originalStdout
	return <-done
}

// readGolden loads a fixture file from testdata. Path components are
// joined relative to the test's working directory.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %q: %v", name, err)
	}
	return string(b)
}

// assertEqualWithDir substitutes the temporary directory's absolute
// path in got with the literal <DATA_DIR> placeholder used in the
// golden fixtures, then compares byte-for-byte. The placeholder
// approach keeps the goldens stable across machines while still
// exercising the absolute-path resolution in cmdInit/cmdSnapshot.
func assertEqualWithDir(t *testing.T, got, want, dir, step string) {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dir, err)
	}
	canonical := strings.ReplaceAll(got, abs, "<DATA_DIR>")
	if canonical != want {
		t.Fatalf("%s output mismatch:\n  got:  %q\n  want: %q", step, canonical, want)
	}
}
