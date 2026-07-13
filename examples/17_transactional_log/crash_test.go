package main

// crash_test.go drives the real cross-process kill -9 crash + recovery
// demonstration end to end: it builds the example binary, has runRealCrashDemo
// re-exec it as a crash-child that commits N fsynced transfers and SIGKILLs
// itself with a torn WAL frame in flight, then reopens the data directory with
// recovery.OpenCtx and asserts the deterministic durability facts — exactly N
// transfers replayed, the torn tail detected and treated as benign, and the
// ledger conserved. The volatile "# " telemetry and the temp path are ignored.
//
// It is skipped in -short mode (it spawns a subprocess) and on platforms that
// cannot deliver SIGKILL. The parent runs under -race, so the recovery path is
// race-checked:
//
//	go test -race ./examples/17_transactional_log/...
//
// TestMain (in example_test.go) wraps the suite in go.uber.org/goleak, so this
// test also proves the subprocess plumbing leaks no goroutine in the parent.

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRealCrashRecovery proves the module's durability contract across a real
// process death: after a SIGKILL mid-write, recovery reproduces exactly the
// durably-committed prefix — no committed transfer lost, no uncommitted or torn
// frame accepted.
func TestRealCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("real cross-process crash test builds and spawns a subprocess; skipped in -short")
	}
	if !crashDemoSupported {
		t.Skipf("real crash demo needs a self-directed SIGKILL; unsupported on %s", runtime.GOOS)
	}

	bin := buildExampleBinary(t)

	const crashCommitted = 40 // durable transfers before the kill
	cfg := defaultConfig()
	cfg.accounts = 40 // 40*39 = 1560 distinct ordered pairs >> crashCommitted+1

	var buf bytes.Buffer
	if err := runRealCrashDemo(context.Background(), &buf, cfg, crashCommitted, bin); err != nil {
		t.Fatalf("real crash demo: %v\n--- output ---\n%s", err, buf.String())
	}
	out := buf.String()
	facts := parseFacts(t, out)

	for _, c := range []struct {
		col  string
		want int64
	}{
		{"crash.committed_before_kill", crashCommitted},
		{"recovery.transfers_replayed", crashCommitted},
		{"recovery.torn_tail_detected", 1},
		{"recovery.balance_conserved", 1},
		{"recovery.is_clean", 1},
	} {
		if got := facts[c.col]; got != c.want {
			t.Errorf("%s = %d, want %d\n--- output ---\n%s", c.col, got, c.want, out)
		}
	}
}

// buildExampleBinary compiles this example into a temp binary and returns its
// path. It skips (rather than fails) when `go build` cannot run — e.g. an
// offline sandbox — mirroring the cross-process pattern in example 25.
func buildExampleBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ex17")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("go build skipped: %v\n%s", err, out)
	}
	return bin
}
