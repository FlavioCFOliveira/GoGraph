package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
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

// plandiffOnce runs the whole `plandiff` subcommand ONCE for the whole package and
// caches its output, so the two acceptance tests below assert against the same run.
//
// Sharing matters for cost, not convenience. The scenarios execute real traversals,
// the short test layer runs under `-race`, and each test driving its own run doubled a
// package that had already reached 548 s. One run, two sets of assertions.
var (
	plandiffOnceGuard sync.Once
	plandiffOnceOut   string
	plandiffOnceErr   error
)

func plandiffOutput(t *testing.T) string {
	t.Helper()
	plandiffOnceGuard.Do(func() {
		dir, err := os.MkdirTemp("", "plandiff-shared-")
		if err != nil {
			plandiffOnceErr = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if err := initEmpty(dir); err != nil {
			plandiffOnceErr = err
			return
		}
		o, err := openStore(context.Background(), dir)
		if err != nil {
			plandiffOnceErr = err
			return
		}
		if _, err := seedFixture(o.store); err != nil {
			_ = o.Close()
			plandiffOnceErr = err
			return
		}
		if err := o.Close(); err != nil {
			plandiffOnceErr = err
			return
		}
		var buf bytes.Buffer
		if err := runPlandiff(context.Background(), plandiffConfig{dir: dir, scale: 1}, &buf); err != nil {
			plandiffOnceErr = err
			return
		}
		plandiffOnceOut = buf.String()
	})
	if plandiffOnceErr != nil {
		t.Fatalf("shared plandiff run: %v", plandiffOnceErr)
	}
	return plandiffOnceOut
}

// TestPlandiff_BothPeepholesFire is the T#2119 acceptance test: on a graph
// carrying the synthetic content skew, plandiff must report both reordering
// peepholes firing, render the plan-diff (ENABLED vs DISABLED trees differ), and
// surface the exact count-store work contrast. It asserts only the deterministic
// facts — never the volatile wall-clock — so it is stable across machines.
func TestPlandiff_BothPeepholesFire(t *testing.T) {
	out := plandiffOutput(t)

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

// TestPlandiff_TraversalScenarios is the T#2154 acceptance test: on the same graph,
// plandiff must demonstrate the bound-destination seek (#2149) and the SYMMETRIC
// anchor swap (#2150), each with a rendered plan-diff and collectable evidence.
//
// It pins only DETERMINISTIC facts — never a wall-clock or a speedup, both of which
// vary by machine and load. The pinned facts are the ones that would silently stop
// being true if the access path or the peephole regressed:
//
//   - the plan renders the seek DISTINCTLY from the enumerate-and-filter path, so the
//     example would notice the operator ceasing to be chosen;
//   - the answer is IDENTICAL across the two arms (runPlandiffScenario errors out
//     otherwise, so reaching the assertions at all already proves it) and non-empty,
//     which is what stops a "win" being measured on a query that matches nothing;
//   - the degree profile shows the traversed out-degree the seek needs to be worth
//     anything — an earlier fixture left the far endpoints at out-degree zero and the
//     scenario measured 1.06x while looking perfectly healthy;
//   - the swap's exact examined-edge contrast, read from the count-store.
func TestPlandiff_TraversalScenarios(t *testing.T) {
	out := plandiffOutput(t)

	// The seek must be VISIBLE in the plan and must differ between the arms. Both
	// strings are physical plan details, so this fails if the operator stops seeking or
	// stops being rendered.
	for _, want := range []string{
		"Expand [ExpandInto seek]",
		"Expand [ExpandInto filter]",
		"--- EXPLAIN (ExpandInto seek DISABLED (enumerate + filter)) ---",
		"--- EXPLAIN (ExpandInto seek ENABLED) ---",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing plan evidence %q in:\n%s", want, out)
		}
	}

	// The traversed FOLLOWS out-degree is what bounds how much the seek can win, so a
	// fixture that lost it must fail loudly rather than report a flat scenario.
	//
	// Every synthetic user follows plandiffFanout accounts, so the maximum is
	// plandiffFanout+1 — the +1 being a mutual back-edge or a triangle-closing edge.
	// Pinned as an exact value so a fan-out change cannot silently flatten these
	// scenarios: that is precisely how an earlier fixture came to report 1.06x.
	for _, want := range []string{
		"# degree.user_follows.max=25",
		"# degree.firehose.outdegree=2001",
		"# degree.verified.indegree=1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing degree evidence %q in:\n%s", want, out)
		}
	}

	// The symmetric swap: it must FIRE (this is a written-DirOut pattern, which was
	// vetoed before #2150) and report the exact work contrast the count-store decided
	// on — the firehose account's whole out-adjacency against the small :Verified scan.
	for _, want := range []string{
		"# symmetric-anchor-swap.reordered=true",
		"# symmetric-anchor-swap.examined_edges.disabled=2001",
		"# symmetric-anchor-swap.examined_edges.enabled=50",
		"# symmetric-anchor-swap.examined_edges.ratio=40.0x",
		"--- EXPLAIN (anchor swap ENABLED (symmetric)) ---",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing symmetric-swap evidence %q in:\n%s", want, out)
		}
	}

	// Every traversal scenario must have MATCHED something. A scenario that returns no
	// rows can post any speedup it likes and mean nothing by it.
	for _, scenario := range []string{"expand-into-mutual", "expand-into-triangle", "symmetric-anchor-swap"} {
		if strings.Contains(out, "# "+scenario+".rows=0") {
			t.Fatalf("scenario %s matched no rows, so its measurement is vacuous:\n%s", scenario, out)
		}
		for _, key := range []string{".rows=", ".elapsed.enabled=", ".elapsed.disabled=",
			".allocs.enabled=", ".allocs.disabled=", ".bytes.enabled=", ".bytes.disabled="} {
			if !strings.Contains(out, "# "+scenario+key) {
				t.Fatalf("scenario %s is missing telemetry %q in:\n%s", scenario, key, out)
			}
		}
	}
}
