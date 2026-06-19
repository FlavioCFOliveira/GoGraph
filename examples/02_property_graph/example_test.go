package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// testConfig is the small, deterministic default the regression test pins:
// same model and code path as a scaled run, small enough to build and query
// well under the short-layer 60 s package budget. The shape is deterministic
// for the fixed seed, so the exact facts asserted below are stable across
// machines.
func testConfig() config {
	return defaultConfig()
}

// TestRun drives run into a buffer and asserts only the deterministic facts —
// node/edge counts, the index-backed match counts, and the read-back typed
// property values of the sample person. The volatile telemetry lines (prefixed
// "# ") are ignored, as the examples standard requires for non-deterministic
// output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(out)

	// Integer facts: node/edge counts and the index-backed match counts.
	// Every value is exact and reproducible for the fixed seed.
	wantInt := map[string]int64{
		"nodes.persons":      2000,
		"nodes.orgs":         50,
		"edges.works_at":     2000,
		"q.persons":          2000,
		"q.managers":         417,
		"q.dept_engineering": 348,
		"q.active":           1496,
		"q.manager_orgs":     50,
		"sample.age":         58,
	}
	for k, want := range wantInt {
		got, ok := facts[k]
		if !ok {
			t.Errorf("missing fact %q", k)
			continue
		}
		if n, err := strconv.ParseInt(got, 10, 64); err != nil {
			t.Errorf("fact %q = %q, not an integer", k, got)
		} else if n != want {
			t.Errorf("%s = %d, want %d", k, n, want)
		}
	}

	// String / typed-value read-back facts: the sample person's properties,
	// fetched back through GetNodeProperty.
	wantStr := map[string]string{
		"sample.key":    "p0",
		"sample.name":   "Charlotte Anderson",
		"sample.salary": "71360.53",
		"sample.active": "true",
		"sample.dept":   "Operations",
	}
	for k, want := range wantStr {
		if got := facts[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// Conservation: the indexed label scan must agree with the materialised
	// node count, and there is one WORKS_AT edge per person.
	if facts["q.persons"] != facts["nodes.persons"] {
		t.Errorf("q.persons (%s) != nodes.persons (%s)", facts["q.persons"], facts["nodes.persons"])
	}
	if facts["edges.works_at"] != facts["nodes.persons"] {
		t.Errorf("edges.works_at (%s) != nodes.persons (%s)", facts["edges.works_at"], facts["nodes.persons"])
	}
}

// TestRunWithoutSchema confirms the schema is genuinely optional: building the
// same dataset with -schema=false (no validator installed) yields byte-for-byte
// identical deterministic facts, since the schema only validates writes and
// never alters the data.
func TestRunWithoutSchema(t *testing.T) {
	withSchema := testConfig()
	withoutSchema := testConfig()
	withoutSchema.schemaOn = false

	var sb, nb bytes.Buffer
	if err := run(context.Background(), &sb, withSchema); err != nil {
		t.Fatalf("run with schema: %v", err)
	}
	if err := run(context.Background(), &nb, withoutSchema); err != nil {
		t.Fatalf("run without schema: %v", err)
	}
	// The config.schema line differs by design; every other fact must match.
	if dropConfigSchema(factLines(sb.String())) != dropConfigSchema(factLines(nb.String())) {
		t.Error("schema on/off produced different facts; the schema must not alter the data")
	}
}

// TestSchemaRejectsBadType confirms the installed schema is a real runtime
// validator, not advisory: a property write whose kind disagrees with the
// declaration is rejected before it lands. This is the enforcement half of the
// optional-schema demonstration.
func TestSchemaRejectsBadType(t *testing.T) {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := installSchema(g); err != nil {
		t.Fatalf("installSchema: %v", err)
	}
	if err := g.SetNodeLabel("x", labelPerson); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	// age is declared PropInt64; writing a string must be rejected.
	if err := g.SetNodeProperty("x", propAge, lpg.StringValue("not-an-int")); err == nil {
		t.Fatal("schema accepted a string for the int64-typed 'age' property; want a type-mismatch error")
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: an impossible
// configuration (here, zero orgs to work at) is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{persons: 100, orgs: 0, managerPct: 10, activePct: 50, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with orgs=0; want error")
	}
}

// TestRunHonoursCancellation confirms the build aborts promptly when the
// context is already cancelled, returning the context error.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, &bytes.Buffer{}, testConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// TestDeterministic confirms the dataset shape is reproducible: two runs with
// the same config produce identical deterministic fact lines.
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := run(context.Background(), &a, testConfig()); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, testConfig()); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s",
			factLines(a.String()), factLines(b.String()))
	}
}

// BenchmarkRun benchmarks an end-to-end build-and-query cycle at the default
// scale, so `go test -bench` produces the build/query evidence mechanically
// alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=value" lines (everything not
// prefixed with "# ") into a map of raw string values, so the caller can assert
// integer, float, and string facts uniformly.
func parseFacts(out string) map[string]string {
	facts := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			facts[k] = v
		}
	}
	return facts
}

// factLines returns only the deterministic lines of out (dropping the volatile
// "# " telemetry), joined back into a single string for equality comparison.
func factLines(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// dropConfigSchema removes the single "config.schema=…" line so two runs that
// differ only in the schema flag can be compared on every other fact.
func dropConfigSchema(facts string) string {
	var keep []string
	for _, line := range strings.Split(facts, "\n") {
		if strings.HasPrefix(line, "config.schema=") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
