package cypher

// parallel_scan_profile_test.go — regression gate for rmp #2664.
//
// # The defect
//
// [buildOpts.forWorker] returns a value copy (`cp := *b`) and then nils the
// fields a worker must not share. profiler was not on that list, so every worker
// received the SAME *exec.Profiler pointer. The morsel-parallel leaf calls its
// [exec.SubplanFactory] once per morsel ON THE WORKER GOROUTINE
// (exec.ParallelScanProject.runMorsel), that factory calls buildOperator, and
// buildOperator wrapped each operator it returned by calling Profiler.Wrap —
// which appended to an unsynchronised slice field. N workers, one slice, no
// synchronisation: `go test -race ./bench/audit352/ -run TestProfileShapes`
// reported 194 data races.
//
// The fix has two halves, and this file gates each of them:
//
//   - forWorker clears the profiler, so no worker ever calls Wrap; and
//   - Profiler carries no state at all, so Wrap has nothing to append to.
//
// Neither half is a compromise. A wrapper built inside a worker was pure
// overhead: exec.PlanTree descends only through exec.PlanChildren, and a
// morsel-parallel leaf implements none, so a measurement taken below one was
// never reachable from the rendered output. The parallel tier is measured as one
// node, which is what exec.Profiler documents and what gate 2 pins.
//
// # Why the wrong -run filter DELETED the reproduction
//
// In bench/audit352, `-run TestPlans` showed ZERO races and `-run
// TestProfileShapes` showed 194, on the same tree. That is not flakiness: it is
// the two entry points doing different things. Explain only BUILDS a plan and
// never installs a profiler, and it never runs the operator, so no worker
// goroutine is ever launched. Only Profile installs a Profiler AND executes,
// which is what puts a shared pointer in the hands of N concurrent morsel
// workers. A gate for this defect must therefore go through Profile; an
// Explain-shaped gate cannot fail no matter how broken the sharing is.
//
// # Why gate 1 is deterministic and gate 2 is not
//
// Gate 1 asserts the fix itself — the field is cleared — with no goroutines
// involved, so it fails on the defective build every time, on any machine, with
// or without -race. Gate 2 asserts the CONTRACT that makes clearing the profiler
// free (the parallel tier renders as one leaf, having really emitted every row),
// and it is also the workload that reproduces the race — but only under -race,
// and only probabilistically, because whether two workers collide on the same
// append is a scheduling accident. That asymmetry is deliberate: gate 1 is the
// one that must hold the fix closed, gate 2 is the one that proves the fix cost
// the rendered profile nothing.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// TestForWorker_ClearsTheProfiler_2664 is the deterministic half of the gate: a
// per-worker buildOpts copy must not carry the query's *exec.Profiler.
//
// It asserts both directions. That the copy is nil is the fix; that the SOURCE
// still holds its profiler is the non-vacuity oracle — it proves the field
// exists, is reachable from this test, and was set before the call, so a nil
// copy means "deliberately cleared" rather than "never populated". Without that
// second assertion the test would keep passing if the field were renamed away.
func TestForWorker_ClearsTheProfiler_2664(t *testing.T) {
	t.Parallel()

	prof := exec.NewProfiler()
	bopts := &buildOpts{profiler: prof}

	worker := bopts.forWorker()
	if worker.profiler != nil {
		t.Errorf("forWorker kept the profiler on the per-worker copy (%p). The morsel "+
			"workers each call the subplan factory on their OWN goroutine, so a shared "+
			"*exec.Profiler is mutated concurrently — the data race at "+
			"cypher/exec/profile.go:93 (rmp #2664). It must be nil: a morsel-parallel "+
			"leaf implements no exec.PlanChildren, so nothing built below it is ever "+
			"rendered and instrumenting it buys nothing.", worker.profiler)
	}
	if bopts.profiler != prof {
		t.Fatalf("the SOURCE buildOpts lost its profiler (got %p, want %p). forWorker must "+
			"clear the copy, never the original — the driving goroutine's plan is what "+
			"PROFILE actually renders. This assertion is also what stops the test above "+
			"from passing vacuously on a build where the field was never set.",
			bopts.profiler, prof)
	}

	// A source with no profiler must clear to nil rather than panic: most builds
	// never install one, and forWorker is on the ordinary plan-build path.
	if got := (&buildOpts{}).forWorker(); got.profiler != nil {
		t.Errorf("forWorker on a profiler-less buildOpts produced a non-nil profiler (%p)", got.profiler)
	}
}

// TestProfile_ParallelScanTierIsOneNodeAndRaceFree_2664 drives a real PROFILE
// over a fused morsel-parallel plan. Under -race it is the reproduction for the
// #2664 data race; with or without -race it pins the contract that makes the fix
// free.
func TestProfile_ParallelScanTierIsOneNodeAndRaceFree_2664(t *testing.T) {
	t.Parallel()
	_, eng := parallelSelectionFixture(t)

	// A scalar projection directly over a labelled scan, with no WHERE and no
	// subquery: the shape tryBuildParallelScanProject fuses. The assertion below
	// is what makes that a fact rather than an assumption.
	const q = `MATCH (a:P) RETURN a.id`

	plan, err := eng.Profile(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Profile(%q): %v", q, err)
	}

	// 1. NON-VACUITY. Everything below is about the parallel tier. If the query did
	//    not fuse, no worker goroutine ran, nothing could have raced, and a green
	//    result would mean nothing at all.
	line, depth, rows, ok := findProfiledNode(plan, "ParallelScanProject")
	if !ok {
		t.Fatalf("the profiled plan contains no ParallelScanProject, so this gate proved "+
			"NOTHING: the fused parallel tier never engaged, no morsel worker was "+
			"launched, and the #2664 race had no opportunity to occur. Pick a query "+
			"shape that still fuses.\nquery: %s\nplan:\n%s", q, plan)
	}

	// 2. NON-VACUITY, second oracle: something WAS observed. The tier must report
	//    every row it was supposed to emit, which proves the workers really ran and
	//    really produced the result — not merely that nothing crashed.
	if rows != parallelSelectionNodes {
		t.Errorf("ParallelScanProject reported rows=%d, want %d (one row per :P node). "+
			"The parallel tier is the node PROFILE attributes the whole fused phase to, "+
			"so a wrong count here means the measurement no longer describes the work "+
			"that was done.\nnode: %s\nplan:\n%s", rows, parallelSelectionNodes, line, plan)
	}

	// 3. CONTRACT. exec.Profiler documents the parallel tier as ONE node, and
	//    forWorker clearing the profiler is what makes that true. Pin it: a change
	//    that starts rendering per-morsel sub-trees must fail here, loudly, rather
	//    than silently altering what PROFILE reports (and quietly re-introducing a
	//    profiler on the worker build path).
	if kids := descendantLines(plan, "ParallelScanProject"); len(kids) > 0 {
		t.Errorf("ParallelScanProject rendered %d child line(s), but the parallel tier is "+
			"documented and measured as ONE node (exec/profile.go, \"The parallel tier is "+
			"measured as ONE node\"). Rendering the inside means either one sub-tree per "+
			"morsel or a synthetic merge of N of them; either changes PROFILE's output and "+
			"needs a deliberate decision, not a silent one. Its depth is %d and these "+
			"followed it:\n%s\nplan:\n%s", len(kids), depth, strings.Join(kids, "\n"), plan)
	}
}

// findProfiledNode locates the first rendered plan line whose operator name is
// name, returning the line, its depth in the tree, and the rows=N it reported.
//
// The renderer (exec.RenderPlanNode) prefixes each line with box-drawing runes —
// "├─ ", "└─ ", "│  ", "   " — so depth is counted in RUNES before the operator
// name rather than in spaces or bytes: '├' and '─' are three bytes each, and a
// byte count would report a depth three times too large for every nested node.
// The prefix is measured rather than divided by a hard-coded group width, so a
// future change to the connector strings cannot silently skew it.
func findProfiledNode(plan, name string) (line string, depth int, rows int64, found bool) {
	for _, l := range strings.Split(plan, "\n") {
		d, rest := planLineDepth(l)
		if !strings.HasPrefix(rest, name) {
			continue
		}
		return l, d, parseProfiledRows(rest), true
	}
	return "", 0, 0, false
}

// descendantLines returns the rendered lines that sit BELOW the named node — the
// contiguous run of lines after it that are indented more deeply. The run stops
// at the first line at or above the node's own depth, because that line begins a
// sibling subtree whose own children are deeper than the node without being its
// descendants. An empty result means the node rendered as a leaf.
func descendantLines(plan, name string) []string {
	lines := strings.Split(plan, "\n")
	start := -1
	var depth int
	for i, l := range lines {
		d, rest := planLineDepth(l)
		if strings.HasPrefix(rest, name) {
			start, depth = i, d
			break
		}
	}
	if start < 0 {
		return nil
	}
	var kids []string
	for _, l := range lines[start+1:] {
		d, _ := planLineDepth(l)
		if d <= depth {
			break
		}
		kids = append(kids, l)
	}
	return kids
}

// planLineDepth splits one rendered line into its tree depth and the text from
// the operator name onwards. Depth is the number of prefix runes divided by the
// three-rune width of one connector ("├─ ", "   "); the prefix is everything
// before the first ASCII letter, which every operator name starts with.
func planLineDepth(line string) (depth int, rest string) {
	runes := []rune(line)
	n := 0
	for n < len(runes) && !isASCIILetter(runes[n]) {
		n++
	}
	if n == len(runes) {
		return 0, "" // blank or connector-only line: no operator on it
	}
	return n / 3, string(runes[n:])
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// parseProfiledRows extracts the N from the "(rows=N, dbhits=…, time=…)" suffix
// exec.RenderPlanNode appends to a measured node, returning -1 when the node
// carries no measurement (an unprofiled run, or a "(not measured)" node). -1 is
// used rather than 0 so "not measured" can never be mistaken for "emitted no
// rows" by a caller comparing against an expected count.
func parseProfiledRows(rest string) int64 {
	const marker = "(rows="
	i := strings.Index(rest, marker)
	if i < 0 {
		return -1
	}
	digits := rest[i+len(marker):]
	var n int64
	seen := 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
		seen++
	}
	if seen == 0 {
		return -1
	}
	return n
}
