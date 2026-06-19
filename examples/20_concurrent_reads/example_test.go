package main

// example_test.go — assertion-based regression test for the concurrent
// reads example. The example spawns a sweep of worker goroutines, so
// completion order and per-level throughput are non-deterministic and a
// // Output: block would be flaky. Instead the test asserts the
// concurrency-invariant facts the readers compute over the shared
// immutable CSR — the Dijkstra src->dst distance, the BFS reach count,
// the PageRank top-k set, and that every concurrent read agreed with the
// single-threaded reference — and never the "# " telemetry or any
// per-goroutine ordering. TestMain runs the suite under go.uber.org/goleak
// so the example doubles as a goroutine-leak check. This is the key
// example for the lock-free read contract, so run it under the race
// detector:
//
//	go test -race ./examples/20_concurrent_reads/...

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under go.uber.org/goleak so
// the concurrent example doubles as a goroutine-leak check.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun drives run into a buffer and asserts the deterministic
// invariants regardless of goroutine scheduling: that BFS reaches every
// node (connected-by-construction), that the Dijkstra distance and
// PageRank top-k facts are present and well-formed, and — the headline
// correctness claim — that every concurrent read agreed with the
// single-threaded reference. It never asserts on "# " telemetry or on
// per-goroutine ordering.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := defaultConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	// Node count is exact and independent of the RNG.
	if got, want := facts["nodes.count"], int64(cfg.nodes); got != want {
		t.Errorf("nodes.count = %d, want %d", got, want)
	}

	// The BA network is connected by construction, so BFS reaches every
	// node. This is the constant the concurrency invariant pins.
	if got, want := facts["ref.bfs_reached"], int64(cfg.nodes); got != want {
		t.Errorf("ref.bfs_reached = %d, want %d (graph must be connected)", got, want)
	}

	// The Dijkstra reference distance is a positive finite cost (weights
	// are >= 1 and the target is reachable).
	if got := facts["ref.dijkstra_dist"]; got <= 0 {
		t.Errorf("ref.dijkstra_dist = %d, want > 0", got)
	}

	// The PageRank top-k fact is present and holds exactly top_k entries.
	topK := mustField(t, out, "ref.pagerank_topk=")
	if n := countTopK(topK); n != cfg.topK {
		t.Errorf("ref.pagerank_topk has %d entries (%q), want %d", n, topK, cfg.topK)
	}

	// The headline correctness fact: every concurrent read agreed with
	// the single-threaded reference at every worker count.
	if got := mustField(t, out, "reads.agree="); got != "true" {
		t.Errorf("reads.agree = %q, want \"true\"", got)
	}
}

// TestDeterministic confirms the dataset shape and the read answers are
// reproducible: two runs with the same config produce identical
// deterministic fact lines (the volatile "# " telemetry is dropped).
func TestDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	cfg := defaultConfig()
	if err := run(context.Background(), &a, cfg); err != nil {
		t.Fatalf("run a: %v", err)
	}
	if err := run(context.Background(), &b, cfg); err != nil {
		t.Fatalf("run b: %v", err)
	}
	if factLines(a.String()) != factLines(b.String()) {
		t.Errorf("deterministic fact lines differ between runs:\n--- a ---\n%s\n--- b ---\n%s",
			factLines(a.String()), factLines(b.String()))
	}
}

// TestConcurrencyInvariant confirms the central contract: the read
// answers do not depend on the worker count. Running the same seed at a
// single worker and at the full sweep must yield identical reference
// facts, because every reader is a pure function of the same immutable
// snapshot.
func TestConcurrencyInvariant(t *testing.T) {
	serial := defaultConfig()
	serial.workers = 1
	parallel := defaultConfig()
	parallel.workers = 8

	var sb, pb bytes.Buffer
	if err := run(context.Background(), &sb, serial); err != nil {
		t.Fatalf("run serial: %v", err)
	}
	if err := run(context.Background(), &pb, parallel); err != nil {
		t.Fatalf("run parallel: %v", err)
	}
	sf, pf := parseFacts(t, sb.String()), parseFacts(t, pb.String())
	for _, k := range []string{"nodes.count", "ref.bfs_reached", "ref.dijkstra_dist"} {
		if sf[k] != pf[k] {
			t.Errorf("%s: serial=%d parallel=%d (must be equal)", k, sf[k], pf[k])
		}
	}
	if s, p := mustField(t, sb.String(), "ref.pagerank_topk="), mustField(t, pb.String(), "ref.pagerank_topk="); s != p {
		t.Errorf("ref.pagerank_topk: serial=%q parallel=%q (must be equal)", s, p)
	}
	for _, out := range []string{sb.String(), pb.String()} {
		if got := mustField(t, out, "reads.agree="); got != "true" {
			t.Errorf("reads.agree = %q, want \"true\"", got)
		}
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for an
// attachment degree the seed core cannot satisfy is rejected before any
// work.
func TestRunRejectsBadConfig(t *testing.T) {
	bad := defaultConfig()
	bad.seedCore = 3
	bad.attach = 5 // seedCore must exceed attach
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with seedCore <= attach; want error")
	}
}

// TestRunHonoursCancellation confirms run aborts promptly when the
// context is already cancelled, returning the context error.
func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, &bytes.Buffer{}, defaultConfig())
	if err == nil {
		t.Fatal("run ignored a cancelled context; want error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("run error = %v, want context canceled", err)
	}
}

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. the top-k list or the
// boolean reads.agree) are skipped.
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
		if n, err := parseInt(v); err == nil {
			facts[k] = n
		}
	}
	return facts
}

// parseInt parses a base-10 signed integer, returning an error for any
// non-integer value (used to skip the non-integer fact lines).
func parseInt(s string) (int64, error) {
	var n int64
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	if s == "" {
		return 0, errNotInt
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errNotInt
		}
		n = n*10 + int64(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// errNotInt is returned by parseInt for a non-integer value.
var errNotInt = stringError("not an integer")

type stringError string

func (e stringError) Error() string { return string(e) }

// factLines returns only the deterministic lines of out (dropping the
// volatile "# " telemetry), joined back into a single string for
// equality comparison.
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

// mustField returns the value of the first bare fact line beginning with
// prefix (e.g. "reads.agree="), failing the test if it is absent.
func mustField(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return v
		}
	}
	t.Fatalf("output missing fact line %q, got:\n%s", prefix, out)
	return ""
}

// countTopK counts the comma-separated entries in a "[a,b,c]" top-k
// rendering, or 0 for the empty list "[]".
func countTopK(s string) int {
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return 0
	}
	return strings.Count(s, ",") + 1
}
