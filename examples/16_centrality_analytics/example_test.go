package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// testConfig returns the deterministic configuration the regression test
// pins. It mirrors defaultConfig (six clusters of fifty nodes at intra-density
// 0.30) so the asserted invariants describe the same shape the example prints
// by default, while staying small enough that the O(V*E) Brandes pass is
// instant.
func testConfig() config {
	return defaultConfig()
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants of the chain-of-clusters topology — never the volatile "# "
// telemetry. The invariants are, in order of strength:
//
//   - the fixed counts (nodes, gateways) the generator produces;
//   - the betweenness winners: the set of top-2*(K-1) betweenness node ids
//     equals the set of bridge gateways. This is a theorem of the topology —
//     each bridge is the unique edge across its cut — so it holds for every
//     seed. Only the ORDER within that set varies per seed (gateways tie on
//     score), so the test compares the set, not the positions;
//   - the community partition invariants: membership is conserved (sum of
//     sizes == V), and the count never exceeds the planted K and never drops
//     below K/2 (label propagation may merge whole adjacent clusters across a
//     single bridge, but cannot fabricate or split communities here);
//   - every community size is a whole multiple of the cluster size — i.e. no
//     dense cluster is ever split. This last one is an OBSERVED property of
//     the dense (p_in >= ~0.3) regime, documented as empirical rather than a
//     theorem (see the comment on the assertion).
func TestRun(t *testing.T) {
	cfg := testConfig()

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// Fixed counts the generator produces by construction.
	mustContain(t, out, "nodes.total=300")
	mustContain(t, out, "nodes.gateways=10")
	mustContain(t, out, "config.communities=6")

	// Betweenness winners == gateway set. The order varies per seed (gateways
	// tie on betweenness score, and the chain tapers values from centre to
	// ends), so compare the SET of reported ids against the gateway set, not
	// the positional ranking.
	wantGateways := gatewaySet(cfg)
	gotTop := betweennessTopSet(t, out, cfg.topK)
	if !equalIntSet(gotTop, wantGateways) {
		t.Errorf("betweenness top-%d set = %v, want gateway set %v",
			cfg.topK, sortedKeys(gotTop), sortedKeys(wantGateways))
	}

	// Community partition invariants.
	count := mustFactInt(t, out, "communities.count=")
	if count < cfg.communities/2 || count > cfg.communities {
		t.Errorf("communities.count=%d, want in [%d,%d] (K/2..K)",
			count, cfg.communities/2, cfg.communities)
	}

	sizes := mustFactIntSlice(t, out, "communities.sizes=")
	total := 0
	for _, s := range sizes {
		total += s
		// Empirical property of the dense regime (p_in >= ~0.3): label
		// propagation only ever MERGES whole clusters across a bridge, never
		// SPLITS one, so every community size is a whole multiple of the
		// cluster size. This is documented as observed (it held across the
		// tested seeds), not as a graph-theoretic guarantee.
		if s%cfg.nodesPerCommunity != 0 {
			t.Errorf("community size %d is not a multiple of cluster size %d (a cluster was split)",
				s, cfg.nodesPerCommunity)
		}
	}
	if want := cfg.communities * cfg.nodesPerCommunity; total != want {
		t.Errorf("community sizes sum to %d, want %d (membership not conserved)", total, want)
	}
	if len(sizes) != count {
		t.Errorf("communities.sizes has %d entries, but communities.count=%d", len(sizes), count)
	}
}

// TestRunDeterministic verifies that two runs of the same config produce
// byte-identical fact lines (the "# " telemetry, which varies, is stripped
// first). Reproducibility for a fixed seed is the whole point of the seeded
// generator.
func TestRunDeterministic(t *testing.T) {
	cfg := testConfig()

	facts := func() string {
		var buf bytes.Buffer
		if err := run(context.Background(), &buf, cfg); err != nil {
			t.Fatalf("run: %v", err)
		}
		return factLines(buf.String())
	}

	if a, b := facts(), facts(); a != b {
		t.Errorf("fact lines differ between two runs of the same config:\nfirst:\n%s\nsecond:\n%s", a, b)
	}
}

// TestRunCancellation verifies that run honours a cancelled context: a context
// cancelled before run starts must abort the build with the context error
// rather than producing a full report.
func TestRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front

	// A scale large enough that the build's coarse cancellation check is
	// reached, so cancellation is observable promptly.
	cfg := defaultConfig()
	cfg.communities = 50
	cfg.nodesPerCommunity = 500

	var buf bytes.Buffer
	err := run(ctx, &buf, cfg)
	if err == nil {
		t.Fatal("run with a cancelled context returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("run error = %v, want a context.Canceled wrap", err)
	}
}

// TestValidateRejectsBadConfig checks that validate rejects impossible
// configurations at the boundary, before any work.
func TestValidateRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
	}{
		{"zero communities", config{communities: 0, nodesPerCommunity: 10, intraDensity: 0.3, topK: 2, seed: 1}},
		{"one node per community", config{communities: 3, nodesPerCommunity: 1, intraDensity: 0.3, topK: 2, seed: 1}},
		{"negative density", config{communities: 3, nodesPerCommunity: 10, intraDensity: -0.1, topK: 2, seed: 1}},
		{"density above one", config{communities: 3, nodesPerCommunity: 10, intraDensity: 1.5, topK: 2, seed: 1}},
		{"zero top-k", config{communities: 3, nodesPerCommunity: 10, intraDensity: 0.3, topK: 0, seed: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.validate(); err == nil {
				t.Errorf("validate accepted invalid config %+v", tt.cfg)
			}
			// run must also reject it (validate is called at the boundary).
			var buf bytes.Buffer
			if err := run(context.Background(), &buf, tt.cfg); err == nil {
				t.Errorf("run accepted invalid config %+v", tt.cfg)
			}
		})
	}
}

// BenchmarkRun runs the full example at the default config so `go test -bench`
// produces the per-run cost mechanically alongside the human-readable report.
func BenchmarkRun(b *testing.B) {
	cfg := testConfig()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := run(context.Background(), &buf, cfg); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// gatewaySet returns the set of bridge-endpoint node ids for cfg. Bridge c
// joins the right gateway of cluster c (offset 1) to the left gateway of
// cluster c+1 (offset 0), so the gateways are nodeID(c,1) for c in [0,K-2] and
// nodeID(c,0) for c in [1,K-1].
func gatewaySet(cfg config) map[int]struct{} {
	set := make(map[int]struct{})
	for c := 0; c+1 < cfg.communities; c++ {
		set[cfg.nodeID(c, 1)] = struct{}{}
		set[cfg.nodeID(c+1, 0)] = struct{}{}
	}
	return set
}

// betweennessTopSet parses the "betweenness.topN=<id>" fact lines into a set of
// node ids. It expects exactly k such lines.
func betweennessTopSet(t *testing.T, out string, k int) map[int]struct{} {
	t.Helper()
	set := make(map[int]struct{}, k)
	for rank := 1; rank <= k; rank++ {
		set[mustFactInt(t, out, fmt.Sprintf("betweenness.top%d=", rank))] = struct{}{}
	}
	return set
}

// factLines returns out with every "# " telemetry line removed, so two runs
// can be compared on their deterministic facts alone.
func factLines(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// mustContain fails the test if out does not contain the given fact line as a
// whole line.
func mustContain(t *testing.T, out, line string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if l == line {
			return
		}
	}
	t.Errorf("output missing fact line %q\n---\n%s", line, out)
}

// mustFactInt finds the single fact line beginning with prefix and returns the
// integer that follows the prefix.
func mustFactInt(t *testing.T, out, prefix string) int {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			n, err := strconv.Atoi(strings.TrimPrefix(l, prefix))
			if err != nil {
				t.Fatalf("fact line %q: %v", l, err)
			}
			return n
		}
	}
	t.Fatalf("output missing fact line with prefix %q\n---\n%s", prefix, out)
	return 0
}

// mustFactIntSlice finds the fact line beginning with prefix and parses the
// "[a b c]" slice that follows it.
func mustFactIntSlice(t *testing.T, out, prefix string) []int {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		body := strings.TrimSpace(strings.Trim(strings.TrimPrefix(l, prefix), "[]"))
		if body == "" {
			return nil
		}
		fields := strings.Fields(body)
		out := make([]int, len(fields))
		for i, f := range fields {
			n, err := strconv.Atoi(f)
			if err != nil {
				t.Fatalf("fact line %q: %v", l, err)
			}
			out[i] = n
		}
		return out
	}
	t.Fatalf("output missing fact line with prefix %q\n---\n%s", prefix, out)
	return nil
}

// equalIntSet reports whether two int sets hold exactly the same members.
func equalIntSet(a, b map[int]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// sortedKeys returns the keys of an int set in ascending order, for stable
// error messages.
func sortedKeys(set map[int]struct{}) []int {
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
