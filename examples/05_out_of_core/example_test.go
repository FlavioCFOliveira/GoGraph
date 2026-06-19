package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// testConfig is the small, deterministic default the example reports at,
// reused by the assertion-based tests. The shape is reproducible for the
// fixed seed, so the invariants asserted below are stable across machines.
func testConfig() config { return defaultConfig() }

// TestRun drives run into a buffer and asserts only the deterministic
// facts — the node/edge counts and the top-k PageRank node ids. The
// volatile telemetry lines (prefixed "# ", carrying durations and the
// disk/heap footprint) and the temp directory path are never asserted, as
// required by the examples standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// The temp path must never reach pinned output (it differs every run).
	if strings.Contains(out, "gograph-ex05-") || strings.Contains(out, "graph.csr") {
		t.Errorf("output leaked a temp path:\n%s", out)
	}

	// csr.order is the live vertex count, exactly cfg.nodes. csr.size is the
	// edge count: the seed core contributes m*(m-1)/2 edges and every node
	// from m onward contributes exactly outDegree, so the total is exact.
	if got, want := facts["csr.order"], int64(cfg.nodes); got != want {
		t.Errorf("csr.order = %d, want %d", got, want)
	}
	m := int64(cfg.outDegree)
	wantSize := m*(m-1)/2 + (int64(cfg.nodes)-m)*m
	if got := facts["csr.size"]; got != wantSize {
		t.Errorf("csr.size = %d, want %d", got, wantSize)
	}

	// The top-k PageRank node ids are deterministic for the fixed seed:
	// Price's model makes early-arriving nodes the high-PageRank authorities
	// and the ascending-id tie-break makes the ordering total. This is the
	// headline fact the example exists to pin.
	wantTop := []int64{197, 164, 7, 230, 131, 32, 98, 65, 172, 56}
	if cfg.topK != len(wantTop) {
		t.Fatalf("test expects top-k = %d, defaultConfig has %d", len(wantTop), cfg.topK)
	}
	for i, want := range wantTop {
		key := "pagerank.top" + strconv.Itoa(i)
		if got, ok := facts[key]; !ok {
			t.Errorf("%s missing from output", key)
		} else if got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}

	// Every reported authority id is a live vertex (in [0, nodes)).
	for i := 0; i < cfg.topK; i++ {
		id := facts["pagerank.top"+strconv.Itoa(i)]
		if id < 0 || id >= int64(cfg.nodes) {
			t.Errorf("pagerank.top%d = %d, want within [0,%d)", i, id, cfg.nodes)
		}
	}
}

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines (the
// "# " telemetry, which varies, is dropped before comparison).
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

// TestSeedChangesShape confirms the seed actually drives the shape: a
// different seed yields a different top-k authority ranking (the counts
// stay the same because they do not depend on the RNG draw).
func TestSeedChangesShape(t *testing.T) {
	base := testConfig()
	other := testConfig()
	other.seed = base.seed + 1

	var ba, oa bytes.Buffer
	if err := run(context.Background(), &ba, base); err != nil {
		t.Fatalf("run base: %v", err)
	}
	if err := run(context.Background(), &oa, other); err != nil {
		t.Fatalf("run other: %v", err)
	}
	if factLines(ba.String()) == factLines(oa.String()) {
		t.Error("different seeds produced identical fact lines; seed has no effect on shape")
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for more
// out-links than there are nodes to target is rejected before any work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{nodes: 4, outDegree: 8, attractive: 1, topK: 4, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with outDegree >= nodes; want error")
	}
}

// TestRunHonoursCancellation confirms the build aborts promptly when the
// context is already cancelled, returning the context error. The node
// count is raised above checkEvery so the generation loop reaches its
// cancellation poll.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := testConfig()
	cfg.nodes = 2 * checkEvery
	err := run(ctx, &bytes.Buffer{}, cfg)
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// BenchmarkRun exercises the whole build → persist → mmap → PageRank path
// at the default scale, so `go test -bench` produces the timing evidence
// mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	for i := 0; i < b.N; i++ {
		if err := run(context.Background(), &bytes.Buffer{}, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as a
// map. Lines whose value is not an integer are skipped.
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
