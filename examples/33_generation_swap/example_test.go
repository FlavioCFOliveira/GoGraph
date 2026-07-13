package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain doubles the package as a goroutine-leak check: the example spawns a
// publisher and a pool of readers, all of which must exit before run returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun drives the default configuration and asserts the MVCC guarantees:
// every read observed a whole, consistent generation; the refcount is
// accounted correctly; and the current generation is the last one published.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	cfg := defaultConfig()
	mustContain(t, out, "generations.published="+itoa(cfg.versions))
	mustContain(t, out, "reads.total="+itoa(cfg.readers*cfg.readsPerReader))
	mustContain(t, out, "reads.all_consistent=true")
	mustContain(t, out, "refcount.accounted=true")

	// The current generation must be the last version published, and every read
	// observed a valid version (never a torn mix), so final.order and
	// final.current_order agree.
	final := "final.order=" + itoa(cfg.baseNodes+(cfg.versions-1)*cfg.growth)
	mustContain(t, out, final)
	mustContain(t, out, strings.Replace(final, "final.order=", "final.current_order=", 1))
}

// TestDeterministic verifies the deterministic fact lines are stable across
// runs (the "# " telemetry, incl. the schedule-dependent set of generations
// observed, is stripped first).
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

// TestRunRejectsBadConfig checks the validation boundary rejects impossible
// shapes.
func TestRunRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*config)
	}{
		{"zero versions", func(c *config) { c.versions = 0 }},
		{"zero readers", func(c *config) { c.readers = 0 }},
		{"zero reads", func(c *config) { c.readsPerReader = 0 }},
		{"one node", func(c *config) { c.baseNodes = 1 }},
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

// factLines returns only the deterministic fact lines: non-blank lines that do
// not start with the "# " telemetry prefix.
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

// mustContain fails the test when out has no line exactly equal to want.
func mustContain(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return
		}
	}
	t.Errorf("output missing fact line %q\n--- facts ---\n%s", want, factLines(out))
}

// itoa is a tiny non-allocating-in-spirit int formatter kept local so the test
// has no dependency beyond the standard library and goleak.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
