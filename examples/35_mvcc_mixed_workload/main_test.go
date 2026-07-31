package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test under a goroutine-leak check: the example spawns
// reader, analytics and writer goroutines and opens a WAL-backed engine, so a
// leaked goroutine or an unreleased WAL after run returns is a defect this
// catches. Under `go test -race` it also certifies the mixed phase is
// data-race-free, which matters here because the whole point of the example is
// three roles touching one engine at once.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun pins the facts that make the MEASUREMENT VALID, and deliberately does
// not pin the measurement itself.
//
// What is asserted is that the instrument is actually measuring: the index
// exists so the short read is a seek rather than a scan, the analytical query
// is genuinely long, and all four phases ran. Those are the three ways this
// example could silently stop being evidence.
//
// What is NOT asserted is `readers_starved`. It reads 1 today and must read 0
// once phase P4 of the MVCC programme retires the read barrier, so pinning it
// would either go red on success or have to be edited in the same commit that
// fixes the defect — and on a loaded machine a throughput ratio is not a sound
// gate anyway. The gate for the defect is the soak-layer test in
// bench/mtaudit/fairness_soak_test.go, which asserts against a calibrated
// multi-second read; this example is the instrument that shows the shape.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := parseFacts(t, buf.String())

	want := map[string]string{
		"config.nodes":      "3000",
		"config.readers":    "4",
		"index.created":     "1",
		"analytics.is_long": "1",
		"phases_measured":   "4",
	}
	for k, v := range want {
		got, ok := facts[k]
		if !ok {
			t.Errorf("missing fact %q", k)
			continue
		}
		if got != v {
			t.Errorf("fact %q = %q, want %q", k, got, v)
		}
	}

	// readers_starved must be PRESENT and well-formed even though its value is
	// not pinned: if the verdict stopped being emitted the example would still
	// pass every assertion above while reporting nothing.
	switch facts["readers_starved"] {
	case "0":
		t.Logf("readers_starved=0 — the point query kept its latency beside an analytical " +
			"query and a writer. If phase P4 has landed this is the expected result; " +
			"bench/mtaudit/fairness_soak_test.go is the gate that confirms it.")
	case "1":
		t.Logf("readers_starved=1 — the known defect (rmp #2274) is still present, as expected " +
			"before phase P4.")
	default:
		t.Fatalf("readers_starved is %q, want 0 or 1 — the example produced no verdict",
			facts["readers_starved"])
	}
}

// parseFacts collects the bare "key=value" lines, ignoring every "# " telemetry
// line, whose values vary per run and per machine.
func parseFacts(t *testing.T, out string) map[string]string {
	t.Helper()
	facts := make(map[string]string, 16)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparsable output line %q", line)
		}
		facts[k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}
	return facts
}
