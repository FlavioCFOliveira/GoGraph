package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain doubles the package as a goroutine-leak check: the example starts
// Bolt servers (plain and TLS) and driver connections, all of which must be
// torn down before run returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun drives the default configuration and asserts every wire-level
// guarantee: bad credentials rejected, good accepted, a committed write
// observed, a rolled-back write discarded, a failure followed by recovery, and
// a successful query over TLS.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"auth.bad_rejected=true",
		"auth.good_accepted=true",
		"tx.write_committed=true",
		"tx.rollback_discarded=true",
		"error.failure_then_recovered=true",
		"tls.query_succeeded=true",
	} {
		mustContain(t, out, want)
	}
}

// TestDeterministic verifies the deterministic fact lines are stable across
// runs (the "# " telemetry is stripped first).
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, defaultConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, defaultConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s", factLines(a.String()), factLines(b.String()))
	}
}

// TestRunRejectsBadConfig checks the validation boundary.
func TestRunRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*config)
	}{
		{"zero persons", func(c *config) { c.persons = 0 }},
		{"empty password", func(c *config) { c.password = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			tc.mut(&cfg)
			if err := run(context.Background(), &bytes.Buffer{}, cfg); err == nil {
				t.Errorf("expected an error for %s, got nil", tc.name)
			}
		})
	}
}

func factLines(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func mustContain(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return
		}
	}
	t.Errorf("output missing fact line %q\n--- facts ---\n%s", want, factLines(out))
}
