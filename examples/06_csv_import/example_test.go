package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is the small, deterministic default: same model and code
// path as a scaled run, sized to build and round-trip well under the
// short-layer 60 s package budget. The shape is deterministic for the
// fixed seed, so the invariants asserted below are stable across
// machines.
func testConfig() config {
	return defaultConfig()
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — the ingested row count, the written row/record counts, and
// the round-trip edge count. The volatile telemetry lines (prefixed "# ")
// are ignored, as required by the examples standard for non-deterministic
// output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, testConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)
	cfg := testConfig()

	// The generator emits a simple graph, so every generated row is a
	// distinct edge: ingested rows equal generated rows equal the edge
	// count.
	gen := facts["generated.rows"]
	if lo, hi := int64(cfg.nodes*cfg.followsMin), int64(cfg.nodes*cfg.followsMax); gen < lo || gen > hi {
		t.Errorf("generated.rows = %d, want within [%d,%d]", gen, lo, hi)
	}
	if got := facts["ingested.rows"]; got != gen {
		t.Errorf("ingested.rows = %d, want %d (== generated.rows)", got, gen)
	}
	if got := facts["graph.edges"]; got != gen {
		t.Errorf("graph.edges = %d, want %d (== generated.rows)", got, gen)
	}

	// Node count is exact and independent of the RNG.
	if got := facts["graph.nodes"]; got != int64(cfg.nodes) {
		t.Errorf("graph.nodes = %d, want %d", got, cfg.nodes)
	}

	// CSV serialises one row per edge, so rows out equals the edge count.
	if got := facts["csv.rows_out"]; got != gen {
		t.Errorf("csv.rows_out = %d, want %d (== edges)", got, gen)
	}

	// JSON Lines emits one record per node then one per edge, so the
	// record count equals nodes + edges.
	wantRecords := int64(cfg.nodes) + gen
	if got := facts["jsonl.records_out"]; got != wantRecords {
		t.Errorf("jsonl.records_out = %d, want %d (== nodes+edges)", got, wantRecords)
	}
	if got := facts["jsonl.expected_records"]; got != wantRecords {
		t.Errorf("jsonl.expected_records = %d, want %d (== nodes+edges)", got, wantRecords)
	}

	// Round-trip invariant: re-reading the written CSV yields the same
	// edge count.
	if got := facts["roundtrip.csv_reread_rows"]; got != gen {
		t.Errorf("roundtrip.csv_reread_rows = %d, want %d", got, gen)
	}
	if got := facts["roundtrip.edges"]; got != gen {
		t.Errorf("roundtrip.edges = %d, want %d (round-trip must preserve the edge count)", got, gen)
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more follows than there are other nodes is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 10, followsMin: 0, followsMax: 20, weightMax: 1, sampleN: 0, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with followsMax > nodes-1; want error")
	}
}

// TestRunHonoursCancellation confirms the pipeline aborts promptly when
// the context is already cancelled, returning the context error.
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

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines.
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

// BenchmarkRun runs the whole generate→parse→serialise→round-trip
// pipeline at the default scale, so `go test -bench` produces the
// throughput evidence mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	cfg.sampleN = 0
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := run(ctx, &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. the config range line)
// are skipped.
func parseFacts(t *testing.T, out string) map[string]int64 {
	t.Helper()
	facts := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed fact line: %q", line)
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			facts[k] = n
		}
	}
	return facts
}

// factLines returns only the deterministic lines of out (dropping the
// volatile "# " telemetry), joined back into a single string for equality
// comparison.
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
