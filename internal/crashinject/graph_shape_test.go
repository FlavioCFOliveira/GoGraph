//go:build gograph_crashinject

package crashinject_test

// graph_shape_test.go — task #2270.
//
// Before this file the crashinject package's entire assertion surface was
// err == nil, Out.Killed, Out.TimedOut, Out.ExitCode, one WAL frame count and
// a tail-error sentinel: it verified that recovery does not ERROR, never that
// the recovered graph is CORRECT. A recovery that silently lost, dropped or
// duplicated an edge passed every test in the package.
//
// These tests close that hole. They drive a real SIGKILL through the
// crash-injection harness and then reopen the artefacts through
// store/recovery, asserting the recovered graph's SHAPE — live node count,
// arc count, per-node out-degree, per-edge weight, and the one label and one
// property the workload commits — against expectations hand-computed here from
// what the child committed before the crash point. The expectations are
// written out literally rather than derived from the helper's own tables, so a
// change on either side has to be reconciled deliberately.
//
// The file is compiled only under the gograph_crashinject build tag: without
// it the helper embeds the production no-op crashpoint.Breakpoint, runs the
// scenario to completion and is never killed.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// expectedEdge is one arc of the hand-computed expectation: source,
// destination, and the weight the child committed for it.
type expectedEdge struct {
	src, dst, weight int64
}

// expectedShape is the complete hand-computed shape of the graph the child
// committed before the crash point. Every field is asserted; nothing is
// derived from the code under test.
type expectedShape struct {
	// LabelNode carries LabelName after recovery.
	LabelName string
	// Edges is every arc that must exist, with its weight.
	Edges []expectedEdge
	// OutDegree is the exact out-degree of every node named in the workload,
	// including the nodes whose degree must be ZERO.
	OutDegree map[int64]int
	// Nodes is the number of LIVE nodes; ArcCount is the number of arcs.
	Nodes     uint64
	ArcCount  uint64
	LabelNode int64
	// PropNode carries PropKey = PropValue after recovery.
	PropNode  int64
	PropKey   string
	PropValue int64
}

// seedShape is the shape committed by the crashinject-helper's
// runCheckpointCrash scenario: three edges forming the ring 1->2->3->1, a
// Root label on node 1, and weight=42 on node 2. Three nodes, three arcs,
// out-degree one apiece.
//
// Hand-computed from the scenario documented on cmd/crashinject-helper: the
// child commits AddEdge(1,2,100), AddEdge(2,3,200), AddEdge(3,1,300),
// SetNodeLabel(1,"Root") and SetNodeProperty(2,"weight",42) in one
// transaction, then triggers the checkpoint that self-kills.
var seedShape = expectedShape{
	Nodes:    3,
	ArcCount: 3,
	Edges: []expectedEdge{
		{src: 1, dst: 2, weight: 100},
		{src: 2, dst: 3, weight: 200},
		{src: 3, dst: 1, weight: 300},
	},
	OutDegree: map[int64]int{1: 1, 2: 1, 3: 1},
	LabelNode: 1,
	LabelName: "Root",
	PropNode:  2,
	PropKey:   "weight",
	PropValue: 42,
}

// seedPlusPostShape is the shape committed by runCheckpointPrefixCrash: the
// seed ring plus one further committed edge 3->4 (weight 400). Four nodes,
// four arcs; node 3 now has out-degree two and node 4 out-degree zero.
var seedPlusPostShape = expectedShape{
	Nodes:    4,
	ArcCount: 4,
	Edges: []expectedEdge{
		{src: 1, dst: 2, weight: 100},
		{src: 2, dst: 3, weight: 200},
		{src: 3, dst: 1, weight: 300},
		{src: 3, dst: 4, weight: 400},
	},
	OutDegree: map[int64]int{1: 1, 2: 1, 3: 2, 4: 0},
	LabelNode: 1,
	LabelName: "Root",
	PropNode:  2,
	PropKey:   "weight",
	PropValue: 42,
}

// TestCrashRecovery_GraphShape_CheckpointPreTruncate crashes the child after
// the self-sufficient snapshot is published and durable but BEFORE the WAL
// prefix is truncated, then asserts the recovered graph's exact shape.
//
// A recovery that lost an edge fails the arc count and the per-node degree; a
// recovery that duplicated one fails the arc count and the degree of its
// source; a recovery that dropped a node fails the live node count.
func TestCrashRecovery_GraphShape_CheckpointPreTruncate(t *testing.T) {
	const scenario = "checkpoint.p2-snapshot-published-pre-truncate"
	dir := runAndAssertKilled(t, scenario)
	assertShape(t, recoverGraph(t, dir), &seedShape, scenario)
}

// TestCrashRecovery_GraphShape_CheckpointPrefixTruncate crashes the child at
// each of the three breakpoints inside wal.Writer.TruncatePrefix's atomic
// copy-then-rename, then asserts the recovered graph's exact shape. At every
// interleaving the full committed state — the seed ring plus the post edge —
// must be reconstructed from the snapshot plus whichever WAL survives.
func TestCrashRecovery_GraphShape_CheckpointPrefixTruncate(t *testing.T) {
	scenarios := []string{
		"checkpoint.truncprefix.tmp-written-pre-rename",
		"checkpoint.truncprefix.post-rename-pre-dirfsync",
		"checkpoint.truncprefix.post-rename-pre-bookkeeping",
	}
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario, func(t *testing.T) {
			dir := runAndAssertKilled(t, scenario)
			assertShape(t, recoverGraph(t, dir), &seedPlusPostShape, scenario)
		})
	}
}

// runAndAssertKilled runs one crash scenario and asserts the child really was
// SIGKILL'd at the breakpoint (rather than timing out or exiting cleanly),
// returning the artefact directory. Without this guard a shape assertion could
// pass against artefacts written by a child that never crashed at all.
func runAndAssertKilled(t *testing.T, scenario string) string {
	t.Helper()
	out, err := crashinject.Run(t, scenario, crashinject.Opts{})
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if out.TimedOut {
		t.Fatalf("child timed out instead of crashing at %s\nstdout: %s\nstderr: %s",
			scenario, out.Stdout, out.Stderr)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s (exit code %d)\nstdout: %s\nstderr: %s",
			scenario, out.ExitCode, out.Stdout, out.Stderr)
	}
	if _, err := filepath.Abs(out.Dir); err != nil {
		t.Fatalf("crash dir %q: %v", out.Dir, err)
	}
	return out.Dir
}

// recoverGraph reopens the crash artefacts with the same int64 codecs the
// crashinject-helper used and returns the recovered graph.
func recoverGraph(t *testing.T, dir string) *lpg.Graph[int64, int64] {
	t.Helper()
	res, err := recovery.Open[int64, int64](dir, recovery.Options[int64, int64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open(%s): %v", dir, err)
	}
	if res.Graph == nil {
		t.Fatal("recovery.Open returned a nil graph")
	}
	return res.Graph
}

// assertShape compares the recovered graph against a hand-computed expectation
// on live node count, arc count, per-node out-degree, per-edge presence and
// weight, the committed label, and the committed property. want is taken by
// pointer only to keep the struct off the argument registers (gocritic
// hugeParam); it is never mutated.
func assertShape(t *testing.T, g *lpg.Graph[int64, int64], want *expectedShape, scenario string) {
	t.Helper()

	if got := g.LiveOrder(); got != want.Nodes {
		t.Errorf("%s: live node count = %d, want %d", scenario, got, want.Nodes)
	}
	if got := g.AdjList().Size(); got != want.ArcCount {
		t.Errorf("%s: arc count = %d, want %d (a lost edge under-counts, a duplicated one over-counts)",
			scenario, got, want.ArcCount)
	}
	for node, wantDeg := range want.OutDegree {
		gotDeg, ok := g.OutDegree(node)
		if !ok {
			if wantDeg != 0 {
				t.Errorf("%s: node %d absent after recovery, want out-degree %d", scenario, node, wantDeg)
			}
			continue
		}
		if gotDeg != wantDeg {
			t.Errorf("%s: node %d out-degree = %d, want %d", scenario, node, gotDeg, wantDeg)
		}
	}
	for _, e := range want.Edges {
		if !g.AdjList().HasEdge(e.src, e.dst) {
			t.Errorf("%s: edge %d->%d lost across crash+recovery", scenario, e.src, e.dst)
			continue
		}
		seen := 0
		for dst, weight := range g.AdjList().Neighbours(e.src) {
			if dst != e.dst {
				continue
			}
			seen++
			if weight != e.weight {
				t.Errorf("%s: edge %d->%d weight = %d, want %d", scenario, e.src, e.dst, weight, e.weight)
			}
		}
		if seen != 1 {
			t.Errorf("%s: edge %d->%d appears %d times after recovery, want exactly 1",
				scenario, e.src, e.dst, seen)
		}
	}
	if !g.HasNodeLabel(want.LabelNode, want.LabelName) {
		t.Errorf("%s: label %q on node %d lost across crash+recovery",
			scenario, want.LabelName, want.LabelNode)
	}
	v, ok := g.GetNodeProperty(want.PropNode, want.PropKey)
	if !ok {
		t.Errorf("%s: property %q on node %d lost across crash+recovery",
			scenario, want.PropKey, want.PropNode)
		return
	}
	if got, _ := v.Int64(); got != want.PropValue {
		t.Errorf("%s: property %q on node %d = %d, want %d",
			scenario, want.PropKey, want.PropNode, got, want.PropValue)
	}
}
