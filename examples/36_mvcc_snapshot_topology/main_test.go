package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test under a goroutine-leak check: the example spawns a
// reader pool bounded by a context, so a reader still running after run returns
// is a defect this catches. Together with `go test -race` it certifies the
// concurrent phase is leak-free and data-race-free as well as isolated.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestRun pins the deterministic facts of the default configuration. Only bare
// "fact" lines are asserted; every "# " telemetry line (latency, throughput,
// version counts, heap) varies per run and per machine and is ignored.
//
// The five zeros are the point of the example. Each was NON-zero on some engine
// this example was run against, and each names a distinct defect: an acknowledged
// commit a reader could not see; an edge a reader saw before it was acknowledged;
// a per-arc structure desynchronised from the topology it indexes; an observation
// query that failed outright because the CSR pair build read a moving adjacency
// twice (rmp #2293); and two clauses of ONE query resolving the same arc at two
// different instants, which the bracket is structurally unable to see and only
// the self-contradiction query catches (rmp #2294).
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, defaultConfig()); err != nil {
		t.Fatalf("run: %v\n\noutput:\n%s", err, buf.String())
	}

	facts := parseFacts(t, buf.String())

	want := map[string]string{
		"config.spokes":                     "80",
		"config.readers":                    "4",
		"config.new_node_every":             "2",
		"config.churn":                      "60",
		"config.min_checks":                 "5",
		"contradiction_checks_met":          "1",
		"links.committed":                   "80",
		"links.final":                       "80",
		"final_read_sees_every_commit":      "1",
		"final_far_endpoints_align":         "1",
		"invisible_commits":                 "0",
		"future_reads":                      "0",
		"misaligned_far_endpoints":          "0",
		"read_errors":                       "0",
		"intra_query_contradictions":        "0",
		"snapshot_topology_invariant_holds": "1",
	}
	for k, wantV := range want {
		got, ok := facts[k]
		if !ok {
			t.Errorf("fact %q missing from output", k)
			continue
		}
		if got != wantV {
			t.Errorf("fact %q = %q, want %q", k, got, wantV)
		}
	}
}

// TestRun_BothEndpointsPreExisting pins the same invariants with
// -new-node-every 0, where every LINK joins two nodes that already existed when
// any reader's snapshot began.
//
// It is a separate case because node liveness cannot mask anything here: with
// both endpoints pre-existing there is no tombstone or unborn-node filter to
// remove a wrongly-visible arc, so this configuration isolates EDGE visibility
// on its own. The default mixes the two dimensions, which is realistic but means
// a failure does not say which one broke.
func TestRun_BothEndpointsPreExisting(t *testing.T) {
	cfg := defaultConfig()
	cfg.newNodeEvery = 0
	cfg.spokes = 50

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v\n\noutput:\n%s", err, buf.String())
	}
	facts := parseFacts(t, buf.String())
	for _, k := range []string{
		"invisible_commits", "future_reads", "misaligned_far_endpoints", "read_errors",
		"intra_query_contradictions",
	} {
		if got := facts[k]; got != "0" {
			t.Errorf("fact %q = %q, want 0 — an edge between two pre-existing nodes "+
				"was observed outside this read's snapshot", k, got)
		}
	}
	if got := facts["snapshot_topology_invariant_holds"]; got != "1" {
		t.Errorf("snapshot_topology_invariant_holds = %q, want 1", got)
	}
	if got := facts["links.final"]; got != "50" {
		t.Errorf("links.final = %q, want 50", got)
	}
}

// TestRun_RejectsInvalidConfig pins that the tunable surface validates rather
// than running a meaningless workload — a zero reader pool would report a green
// invariant it never checked.
func TestRun_RejectsInvalidConfig(t *testing.T) {
	for name, mutate := range map[string]func(*config){
		"no spokes":      func(c *config) { c.spokes = 0 },
		"no readers":     func(c *config) { c.readers = 0 },
		"no duration":    func(c *config) { c.duration = 0 },
		"negative N":     func(c *config) { c.newNodeEvery = -1 },
		"negative churn": func(c *config) { c.churn = -1 },
		"no min checks":  func(c *config) { c.minChecks = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaultConfig()
			mutate(&cfg)
			var buf bytes.Buffer
			if err := run(context.Background(), &buf, cfg); err == nil {
				t.Fatal("run accepted an invalid configuration")
			}
		})
	}
}

// parseFacts collects the bare key=value lines, ignoring "# " telemetry.
func parseFacts(t *testing.T, out string) map[string]string {
	t.Helper()
	facts := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparseable output line %q", line)
		}
		facts[k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}
	return facts
}
