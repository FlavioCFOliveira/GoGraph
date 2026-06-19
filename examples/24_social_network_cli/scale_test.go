package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// factLines returns only the deterministic fact lines from out: every
// non-empty line that is NOT a "# " telemetry line. The regression tests
// assert on these and ignore the volatile telemetry, exactly as the
// examples standard requires.
func factLines(out string) []string {
	var facts []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		facts = append(facts, line)
	}
	return facts
}

// telemetryLines returns only the "# " telemetry lines from out.
func telemetryLines(out string) []string {
	var tel []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			tel = append(tel, line)
		}
	}
	return tel
}

// TestScaleConfig_Validate covers the boundary checks on the opt-in scale
// knobs: a disabled config is always valid, and an enabled config rejects a
// degree that cannot be satisfied by the synthetic population.
func TestScaleConfig_Validate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     scaleConfig
		wantErr bool
	}{
		{"disabled default", defaultScaleConfig(), false},
		{"disabled explicit zero", scaleConfig{users: 0, friends: 8}, false},
		{"enabled ok", scaleConfig{users: 100, friends: 8, seed: 1}, false},
		{"negative users", scaleConfig{users: -1}, true},
		{"negative friends", scaleConfig{users: 100, friends: -1}, true},
		{"friends equals users", scaleConfig{users: 10, friends: 10}, true},
		{"friends exceeds users", scaleConfig{users: 10, friends: 11}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr && err == nil {
				t.Fatalf("validate(%+v) = nil, want error", tc.cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validate(%+v) = %v, want nil", tc.cfg, err)
			}
		})
	}
}

// TestParseSeedArgs maps the seed flag surface to a seedConfig and checks
// that a missing -d and an impossible scale are both rejected with the
// right error class (usage vs runtime).
func TestParseSeedArgs(t *testing.T) {
	cfg, err := parseSeedArgs([]string{"-d", "x", "-users", "500", "-friends", "12", "-seed", "9", "-evidence"})
	if err != nil {
		t.Fatalf("parseSeedArgs: %v", err)
	}
	if cfg.dir != "x" || cfg.scale.users != 500 || cfg.scale.friends != 12 || cfg.scale.seed != 9 || !cfg.evidence {
		t.Fatalf("parseSeedArgs returned %+v", cfg)
	}

	if _, err := parseSeedArgs([]string{"-users", "500"}); err == nil {
		t.Fatal("parseSeedArgs without -d: want usage error, got nil")
	} else if !isUsageError(err) {
		t.Fatalf("missing -d must be a usage error, got %T: %v", err, err)
	}

	// friends >= users is a runtime (not usage) error: the flags parsed
	// fine, the requested shape is just impossible.
	if _, err := parseSeedArgs([]string{"-d", "x", "-users", "5", "-friends", "5"}); err == nil {
		t.Fatal("parseSeedArgs with friends>=users: want error, got nil")
	} else if isUsageError(err) {
		t.Fatalf("impossible scale must be a runtime error, got *usageError: %v", err)
	}
}

// isUsageError reports whether err unwraps to a *usageError.
func isUsageError(err error) bool {
	var ue *usageError
	return errors.As(err, &ue)
}

// TestRunSeedWithConfig_DefaultUnchanged confirms that the default config
// (scale off, evidence off) produces exactly the original single fact line
// and no telemetry, so the golden tests stay valid.
func TestRunSeedWithConfig_DefaultUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	var buf bytes.Buffer
	cfg := seedConfig{dir: dir, scale: defaultScaleConfig()}
	if err := runSeedWithConfig(context.Background(), cfg, &buf); err != nil {
		t.Fatalf("runSeedWithConfig: %v", err)
	}
	if got := buf.String(); got != `{"seeded":true,"status":"ok"}`+"\n" {
		t.Fatalf("default seed output changed: %q", got)
	}
}

// TestRunSeedWithConfig_ScaledFacts drives a scaled seed and asserts the
// deterministic facts that result: the synthetic users and FOLLOWS edges
// are counted by the stats battery, on top of the canonical fixture. It
// never asserts on the "# " telemetry — only that telemetry is present
// when -evidence is set and absent when it is not.
func TestRunSeedWithConfig_ScaledFacts(t *testing.T) {
	const (
		extraUsers = 200
		friends    = 6
	)
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}

	var seedBuf bytes.Buffer
	cfg := seedConfig{
		dir:      dir,
		scale:    scaleConfig{users: extraUsers, friends: friends, seed: 5},
		evidence: true,
	}
	if err := runSeedWithConfig(context.Background(), cfg, &seedBuf); err != nil {
		t.Fatalf("runSeedWithConfig: %v", err)
	}

	// Fact: the only bare line is the seeded reply.
	facts := factLines(seedBuf.String())
	if len(facts) != 1 || facts[0] != `{"seeded":true,"status":"ok"}` {
		t.Fatalf("seed fact lines = %v, want a single seeded reply", facts)
	}
	// Telemetry must be present (evidence on) and every telemetry line must
	// carry the "# " prefix; none may leak into the facts.
	if len(telemetryLines(seedBuf.String())) == 0 {
		t.Fatal("evidence on but no telemetry emitted")
	}

	// Deterministic facts on the scaled graph: 5 fixture users + extraUsers,
	// 8 fixture follows + extraUsers*friends synthetic follows. The rest of
	// the fixture (posts, comments, etc.) is unchanged.
	var statsBuf bytes.Buffer
	if err := runStatsWithEvidence(context.Background(), dir, &statsBuf, true); err != nil {
		t.Fatalf("runStatsWithEvidence: %v", err)
	}
	statsFacts := factLines(statsBuf.String())
	if len(statsFacts) != 1 {
		t.Fatalf("stats fact lines = %v, want exactly one JSON object", statsFacts)
	}
	var counts map[string]int64
	if err := json.Unmarshal([]byte(statsFacts[0]), &counts); err != nil {
		t.Fatalf("decode stats fact line %q: %v", statsFacts[0], err)
	}
	wantUsers := int64(len(fixtureUsers) + extraUsers)
	wantFollows := int64(len(fixtureFollows) + extraUsers*friends)
	if counts["users"] != wantUsers {
		t.Errorf("users = %d, want %d", counts["users"], wantUsers)
	}
	if counts["follows"] != wantFollows {
		t.Errorf("follows = %d, want %d", counts["follows"], wantFollows)
	}
	// The fixture-only categories must be untouched by the synthetic scale.
	for k, want := range map[string]int64{
		"posts": int64(len(fixturePosts)), "comments": int64(len(fixtureComments)),
		"likes": int64(len(fixtureLikes)), "authored": 8, "on": 5, "replies": 2,
	} {
		if counts[k] != want {
			t.Errorf("%s = %d, want %d (synthetic scale must not touch fixture categories)", k, counts[k], want)
		}
	}
}

// TestRunSeedWithConfig_ScaledDeterministic confirms that a fixed seed
// fixes the data shape: two independent scaled seeds with the same knobs
// yield byte-identical stats fact lines (telemetry excluded).
func TestRunSeedWithConfig_ScaledDeterministic(t *testing.T) {
	seedAndStats := func() string {
		dir := t.TempDir()
		if err := initEmpty(dir); err != nil {
			t.Fatalf("initEmpty: %v", err)
		}
		var sb bytes.Buffer
		cfg := seedConfig{dir: dir, scale: scaleConfig{users: 150, friends: 5, seed: 99}}
		if err := runSeedWithConfig(context.Background(), cfg, &sb); err != nil {
			t.Fatalf("runSeedWithConfig: %v", err)
		}
		var st bytes.Buffer
		if err := runStats(context.Background(), dir, &st); err != nil {
			t.Fatalf("runStats: %v", err)
		}
		return st.String()
	}
	a, b := seedAndStats(), seedAndStats()
	if a != b {
		t.Fatalf("scaled stats not reproducible for a fixed seed:\n  a: %s\n  b: %s", a, b)
	}
}

// TestRunStatsWithEvidence_FactPinnedTelemetryIgnored confirms that the
// JSON fact line is byte-identical whether evidence is on or off, and that
// the only difference is the trailing "# " telemetry block.
func TestRunStatsWithEvidence_FactPinnedTelemetryIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := seedFixture(o.store); err != nil {
		_ = o.Close()
		t.Fatalf("seedFixture: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var off, on bytes.Buffer
	if err := runStatsWithEvidence(context.Background(), dir, &off, false); err != nil {
		t.Fatalf("runStatsWithEvidence(off): %v", err)
	}
	if err := runStatsWithEvidence(context.Background(), dir, &on, true); err != nil {
		t.Fatalf("runStatsWithEvidence(on): %v", err)
	}

	// Without evidence: exactly the golden single fact line, no telemetry.
	if len(telemetryLines(off.String())) != 0 {
		t.Fatalf("evidence off but telemetry present: %q", off.String())
	}
	// The fact line is identical in both modes.
	offFacts, onFacts := factLines(off.String()), factLines(on.String())
	if len(offFacts) != 1 || len(onFacts) != 1 || offFacts[0] != onFacts[0] {
		t.Fatalf("fact line differs between evidence modes:\n  off: %v\n  on:  %v", offFacts, onFacts)
	}
	// With evidence: telemetry is present and includes the graph order and
	// the per-query latency lines.
	tel := strings.Join(telemetryLines(on.String()), "\n")
	for _, want := range []string{"# graph.order=", "# graph.size=", "# mem.heap_alloc=", "# q.users.latency="} {
		if !strings.Contains(tel, want) {
			t.Errorf("telemetry missing %q\n%s", want, tel)
		}
	}
}
