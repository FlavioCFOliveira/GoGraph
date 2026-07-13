package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// Expected shape of the committed survivor and the aborted phantom, pinned
// here independently of the example's own accounting so the test is a genuine
// external check. survivorNodes/survivorEdges are what the committed survivor
// adds on top of the genesis build; phantomOps is the number of mutations the
// aborted phantom transaction buffers before it is rolled back.
const (
	survivorNodes = 2  // survivor Package + Release
	survivorEdges = 2  // survivor PUBLISHED + DEPENDS_ON
	phantomOps    = 13 // mutations buffered by the aborted phantom transaction
)

// testConfig is a small version of the default specification: the same
// model and code path, sized to persist, snapshot and recover well under
// the short-layer 60 s package budget. The shape is deterministic for the
// fixed seed, so the invariants asserted below are stable across machines.
func testConfig() config {
	return config{
		packages: 200,
		depsMin:  2,
		depsMax:  5,
		batch:    40,
		seed:     7,
	}
}

// TestRun drives run into a buffer and asserts only the deterministic
// invariants — recovered node/edge/label counts and the sampled property
// values that prove the WAL -> snapshot -> recovery round-trip. The
// volatile telemetry lines (prefixed "# ", and the per-run temp path,
// which is never printed) are ignored, as required by the examples
// standard for non-deterministic output.
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := testConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	// The temp directory path must never reach stdout (it differs every
	// run); asserting its absence keeps the report stable.
	if strings.Contains(out, "gograph-ex04-") {
		t.Errorf("output leaked the temp directory path:\n%s", out)
	}

	facts := parseFacts(t, out)

	// Node counts are exact and independent of the RNG: one Release per
	// Package (2N for the genesis build) plus the committed survivor's two
	// nodes (its Package and Release). The aborted phantom adds none.
	if got := facts["nodes.packages"]; got != int64(cfg.packages) {
		t.Errorf("nodes.packages = %d, want %d", got, cfg.packages)
	}
	if got := facts["nodes.releases"]; got != int64(cfg.packages) {
		t.Errorf("nodes.releases = %d, want %d", got, cfg.packages)
	}
	if got, want := facts["recovered.nodes"], int64(2*cfg.packages+survivorNodes); got != want {
		t.Errorf("recovered.nodes = %d, want %d (2N genesis + survivor)", got, want)
	}

	// Exactly one PUBLISHED edge per package (genesis only; the survivor's
	// PUBLISHED edge is not counted in this genesis fact).
	if got := facts["edges.published"]; got != int64(cfg.packages) {
		t.Errorf("edges.published = %d, want %d", got, cfg.packages)
	}

	// DEPENDS_ON out-degree is in [depsMin, depsMax] per release, so the
	// total lands in the corresponding band (genesis only).
	deps := facts["edges.depends_on"]
	if lo, hi := int64(cfg.packages*cfg.depsMin), int64(cfg.packages*cfg.depsMax); deps < lo || deps > hi {
		t.Errorf("edges.depends_on = %d, want within [%d,%d]", deps, lo, hi)
	}

	// The recovered edge total must equal every committed edge: the genesis
	// PUBLISHED and DEPENDS_ON edges plus the survivor's two edges. The aborted
	// phantom's edges must not appear.
	if got, want := facts["recovered.edges"], facts["edges.published"]+deps+survivorEdges; got != want {
		t.Errorf("recovered.edges = %d, want %d (genesis published+depends_on + survivor)", got, want)
	}

	// ── Atomicity of the aborted transaction ─────────────────────────────────
	// The phantom transaction buffers a fixed number of mutations, then aborts.
	if got := facts["rollback.ops_attempted"]; got != phantomOps {
		t.Errorf("rollback.ops_attempted = %d, want %d", got, phantomOps)
	}
	// The WAL holds one OpCommit marker per committed transaction — every
	// genesis commit plus the single survivor — and none for the aborted one.
	if got, want := facts["wal.commit_markers"], int64(cfg.packages+1); got != want {
		t.Errorf("wal.commit_markers = %d, want %d (genesis + survivor, phantom leaves none)", got, want)
	}
	// Not one byte of the aborted transaction reached the durable log.
	if got := facts["wal.phantom_frames"]; got != 0 {
		t.Errorf("wal.phantom_frames = %d, want 0 (aborted tx must leave no trace in the WAL)", got)
	}
	// After the reopen, none of the phantom's entities are present, the state
	// equals the pre-tx baseline, and both the write- and recovery-phase
	// survivor facts confirm the committed transaction persisted.
	if got := facts["rollback.applied_after_reopen"]; got != 0 {
		t.Errorf("rollback.applied_after_reopen = %d, want 0 (rolled-back mutations must not survive)", got)
	}
	if got := facts["state.matches_pre_tx"]; got != 1 {
		t.Errorf("state.matches_pre_tx = %d, want 1 (recovered graph must equal the pre-tx baseline)", got)
	}
	if got := facts["survivor.committed"]; got != 1 {
		t.Errorf("survivor.committed = %d, want 1", got)
	}
	if got := facts["survivor.present"]; got != 1 {
		t.Errorf("survivor.present = %d, want 1 (committed survivor must survive the reopen)", got)
	}

	// Both labels (Package, Release) are in use after recovery.
	if got := facts["recovered.labels"]; got != 2 {
		t.Errorf("recovered.labels = %d, want 2", got)
	}

	// Recovery hit the snapshot rather than replaying from an empty base.
	if !boolFact(t, out, "recovered.snapshot_hit") {
		t.Error("recovered.snapshot_hit = false, want true")
	}

	// Sampled string and int64 property values round-tripped. The concrete
	// values are deterministic for the fixed seed; assert they are present
	// and self-consistent rather than pinning their exact text.
	for _, key := range []string{
		"recovered.sample_name", "recovered.sample_coord",
		"recovered.sample_downloads", "recovered.sample_published",
	} {
		if !lineExists(out, key) {
			t.Errorf("missing recovered fact %q", key)
		}
	}
	// The sampled coord is "<name>@<version>", so it must begin with the
	// sampled name — a cross-check that both string properties recovered
	// from the same release/package pair.
	name := stringFact(out, "recovered.sample_name")
	coord := stringFact(out, "recovered.sample_coord")
	if name == "" || !strings.HasPrefix(coord, name+"@") {
		t.Errorf("recovered coord %q does not start with name %q@", coord, name)
	}
	// Downloads is a non-negative int64.
	if dls := facts["recovered.sample_downloads"]; dls < 0 {
		t.Errorf("recovered.sample_downloads = %d, want >= 0", dls)
	}
}

// TestAtomicRollbackLeavesNoTrace is the focused regression pin for task
// #1976: a deliberately aborted transaction must leave no trace after a reopen
// from disk, while a committed transaction in the same run survives. run
// fail-stops on any ACID violation, so a non-nil error here is itself the
// signal that atomicity or durability regressed; the fact assertions document
// the expected clean values.
func TestAtomicRollbackLeavesNoTrace(t *testing.T) {
	cfg := testConfig()
	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run reported an ACID violation: %v", err)
	}
	facts := parseFacts(t, buf.String())

	for _, c := range []struct {
		key  string
		want int64
	}{
		{"rollback.ops_attempted", phantomOps},          // the phantom buffered K mutations
		{"rollback.applied_after_reopen", 0},            // none of them survived the reopen
		{"wal.phantom_frames", 0},                       // and none reached the WAL
		{"wal.commit_markers", int64(cfg.packages + 1)}, // one marker per committed tx, none for the abort
		{"state.matches_pre_tx", 1},                     // recovered graph equals the pre-tx baseline
		{"survivor.committed", 1},                       // the contrast: a committed tx ...
		{"survivor.present", 1},                         // ... survives the same reopen
	} {
		if got := facts[c.key]; got != c.want {
			t.Errorf("%s = %d, want %d", c.key, got, c.want)
		}
	}
}

// TestRunRejectsBadConfig confirms the boundary validation: asking for
// more dependencies than there are other packages is rejected before any
// work (and before any temp directory is created).
func TestRunRejectsBadConfig(t *testing.T) {
	bad := config{packages: 5, depsMin: 0, depsMax: 20, batch: 1, seed: 1}
	if err := run(context.Background(), &bytes.Buffer{}, bad); err == nil {
		t.Fatal("run accepted a config with depsMax > packages-1; want error")
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

// TestDeterministic confirms the dataset shape is reproducible: two runs
// with the same config produce identical deterministic fact lines (the
// recovered counts and sampled values), independent of the per-run temp
// path and timing telemetry.
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

// parseFacts extracts the deterministic "key=int" lines (everything not
// prefixed with "# ") whose value parses as an integer, returning them as
// a map. Lines whose value is not an integer (e.g. the config range line
// or the string-valued sampled properties) are skipped.
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

// lineExists reports whether out has any bare (non-telemetry) line
// beginning "key=".
func lineExists(out, key string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if strings.HasPrefix(line, key+"=") {
			return true
		}
	}
	return false
}

// stringFact returns the string value of the bare fact line "key=value",
// or "" when the line is absent.
func stringFact(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// boolFact returns the boolean value of the bare fact line "key=true|false".
func boolFact(t *testing.T, out, key string) bool {
	t.Helper()
	v := stringFact(out, key)
	b, err := strconv.ParseBool(v)
	if err != nil {
		t.Fatalf("fact %q = %q, not a bool", key, v)
	}
	return b
}
