package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// seedForPlandiff initialises dir, applies the canonical fixture, and closes the
// store so a subsequent openStore (inside runPlandiff) has exclusive WAL access.
func seedForPlandiff(t *testing.T, dir string) {
	t.Helper()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := seedFixture(o.store); err != nil {
		t.Fatalf("seedFixture: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestPlandiff_BothPeepholesFire is the T#2119 acceptance test: on a graph
// carrying the synthetic content skew, plandiff must report both reordering
// peepholes firing, render the plan-diff (ENABLED vs DISABLED trees differ), and
// surface the exact count-store work contrast. It asserts only the deterministic
// facts — never the volatile wall-clock — so it is stable across machines.
func TestPlandiff_BothPeepholesFire(t *testing.T) {
	dir := t.TempDir()
	seedForPlandiff(t, dir)

	var buf bytes.Buffer
	if err := runPlandiff(context.Background(), plandiffConfig{dir: dir, scale: 1}, &buf); err != nil {
		t.Fatalf("runPlandiff: %v", err)
	}
	out := buf.String()

	// Deterministic cardinalities: base fixture (5 users / 3 posts / 5 comments)
	// plus the synthetic layer (2000 / 1500 / 100).
	for _, want := range []string{
		"# cardinality.users=2005",
		"# cardinality.posts=1503",
		"# cardinality.comments=105",
		"# content.seeded_now=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing telemetry %q in:\n%s", want, out)
		}
	}

	// Both peepholes must fire (the rendered ENABLED and DISABLED plans differ).
	for _, want := range []string{
		"# anchor-swap.reordered=true",
		"# disjoint-reorder.reordered=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// Exact count-store work contrast (the db-hits-style figures).
	for _, want := range []string{
		"# anchor-swap.scanned_start_rows.disabled=1503",
		"# anchor-swap.scanned_start_rows.enabled=105",
		"# disjoint-reorder.inner_reinitialisations.disabled=2005",
		"# disjoint-reorder.inner_reinitialisations.enabled=105",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing work metric %q in:\n%s", want, out)
		}
	}

	// The anchor-swap plan-diff must show the re-rooted scan and flipped direction.
	anchorOn := planBlock(t, out, "--- EXPLAIN (reordering ENABLED) ---", "anchor-swap.reordered")
	if !strings.Contains(anchorOn, "NodeByLabelScan [c:Comment]") || !strings.Contains(anchorOn, "(c)-[:ON]->(p)") {
		t.Fatalf("anchor ENABLED plan should scan :Comment and expand outgoing:\n%s", anchorOn)
	}
	anchorOff := planBlock(t, out, "--- EXPLAIN (reordering DISABLED) ---", "--- EXPLAIN (reordering ENABLED) ---")
	if !strings.Contains(anchorOff, "NodeByLabelScan [p:Post]") || !strings.Contains(anchorOff, "(p)<-[:ON]-(c)") {
		t.Fatalf("anchor DISABLED plan should scan :Post and expand incoming:\n%s", anchorOff)
	}

	// The disjoint reorder must drive :Comment first when enabled (its scan line
	// precedes the :User scan line under CartesianProduct).
	disjointOn := lastPlanBlock(t, out, "--- EXPLAIN (reordering ENABLED) ---")
	ci := strings.Index(disjointOn, "NodeByLabelScan [c:Comment]")
	ui := strings.Index(disjointOn, "NodeByLabelScan [u:User]")
	if ci < 0 || ui < 0 || ci > ui {
		t.Fatalf("disjoint ENABLED plan should drive :Comment before :User:\n%s", disjointOn)
	}
}

// TestPlandiff_Idempotent verifies the synthetic content layer is seeded once:
// the second invocation reports content.seeded_now=false and still fires both
// peepholes (the persisted skew survives the reopen).
func TestPlandiff_Idempotent(t *testing.T) {
	dir := t.TempDir()
	seedForPlandiff(t, dir)

	var first bytes.Buffer
	if err := runPlandiff(context.Background(), plandiffConfig{dir: dir, scale: 1}, &first); err != nil {
		t.Fatalf("runPlandiff (1): %v", err)
	}
	if !strings.Contains(first.String(), "# content.seeded_now=true") {
		t.Fatalf("first run should seed the content layer:\n%s", first.String())
	}

	var second bytes.Buffer
	if err := runPlandiff(context.Background(), plandiffConfig{dir: dir, scale: 1}, &second); err != nil {
		t.Fatalf("runPlandiff (2): %v", err)
	}
	if !strings.Contains(second.String(), "# content.seeded_now=false") {
		t.Fatalf("second run should NOT re-seed:\n%s", second.String())
	}
	if !strings.Contains(second.String(), "# anchor-swap.reordered=true") ||
		!strings.Contains(second.String(), "# disjoint-reorder.reordered=true") {
		t.Fatalf("second run should still fire both peepholes:\n%s", second.String())
	}
}

// planBlock returns the substring of out between the first occurrence of start
// and the next occurrence of end (exclusive), failing the test when either
// marker is absent.
func planBlock(t *testing.T, out, start, end string) string {
	t.Helper()
	si := strings.Index(out, start)
	if si < 0 {
		t.Fatalf("marker %q not found in:\n%s", start, out)
	}
	rest := out[si+len(start):]
	ei := strings.Index(rest, end)
	if ei < 0 {
		t.Fatalf("marker %q not found after %q in:\n%s", end, start, out)
	}
	return rest[:ei]
}

// lastPlanBlock returns the substring of out from the LAST occurrence of start to
// the end of the string — used to isolate the disjoint scenario's ENABLED plan
// (the second such block in the report).
func lastPlanBlock(t *testing.T, out, start string) string {
	t.Helper()
	si := strings.LastIndex(out, start)
	if si < 0 {
		t.Fatalf("marker %q not found in:\n%s", start, out)
	}
	return out[si+len(start):]
}
