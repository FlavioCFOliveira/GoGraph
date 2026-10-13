package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
)

// TestBulkImportFaults_EveryRegimeFiresAndIsAllOrNothing is the measured
// evidence gate for the fault arm (rmp #2518). Each regime must have fired and
// failed its publish, and every outcome must be all or nothing: a fault before
// the publish rename is durable leaves no import at all, and the two outcomes
// that make the rename durable — a clean publish, and a crash after a
// written-back rename — leave the whole import.
func TestBulkImportFaults_EveryRegimeFiresAndIsAllOrNothing(t *testing.T) {
	t.Parallel()
	sc := bulkImportParityScenario()
	ev, report, err := runBulkImportParityWith(context.Background(), sc.DefaultSeed, defaultBulkImportOptions())
	if err != nil {
		t.Fatalf("runBulkImportParityWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
	f := ev.faults
	t.Logf("clean publish: %d filesystem operations, publish rename at operation %d, %d component fsyncs, "+
		"crash-then-recover outcome %s", f.cleanOps, f.renameOp, f.cleanSyncs, f.controlOutcome)
	for _, r := range f.regimes {
		t.Logf("regime %-55q fired=%t publishFailed=%t outcome=%s", r.name, r.fired, r.publishFailed, r.outcome)
	}
	t.Logf("crash sweep: fired at %d of %d points; outcomes empty=%d complete=%d; acknowledged=%d",
		f.crashFired, f.cleanOps, f.crashEmpty, f.crashComplete, f.crashAcked)

	want := map[string]string{
		"ENOSPC at write":                                     bulkImportOutcomeEmpty,
		"ENOSPC at sync":                                      bulkImportOutcomeEmpty,
		"fsync fault on the first component":                  bulkImportOutcomeEmpty,
		"fsync fault on the last component":                   bulkImportOutcomeEmpty,
		"rename fault on snapshot.tmp -> snapshot":            bulkImportOutcomeEmpty,
		"crash after the publish rename, rename written back": bulkImportOutcomeComplete,
	}
	if len(f.regimes) != len(want) {
		t.Fatalf("ran %d fault regimes, want %d", len(f.regimes), len(want))
	}
	for _, r := range f.regimes {
		w, ok := want[r.name]
		if !ok {
			t.Errorf("unexpected regime %q", r.name)
			continue
		}
		if !r.fired {
			t.Errorf("regime %q: the fault never fired", r.name)
		}
		if !r.publishFailed {
			t.Errorf("regime %q: the publish reported success", r.name)
		}
		if r.outcome != w {
			t.Errorf("regime %q: recovery found %q, want %q", r.name, r.outcome, w)
		}
	}
	if f.controlOutcome != bulkImportOutcomeComplete {
		t.Errorf("a clean publish followed by a host crash recovered %q, want %q",
			f.controlOutcome, bulkImportOutcomeComplete)
	}
	// The sweep reached every operation, and each point was adjudicated.
	if f.cleanOps < 10 {
		t.Errorf("a clean publish performed %d filesystem operations; the sweep would be vacuous", f.cleanOps)
	}
	if f.crashFired != f.cleanOps {
		t.Errorf("the crash fired at %d of %d sweep points", f.crashFired, f.cleanOps)
	}
	if f.crashEmpty+f.crashComplete != f.cleanOps {
		t.Errorf("sweep outcomes empty=%d + complete=%d do not cover the %d points",
			f.crashEmpty, f.crashComplete, f.cleanOps)
	}
	if f.crashEmpty == 0 {
		t.Error("no crash point left the store empty; the sweep never interrupted a publish before its rename")
	}
}

// TestBulkImportFaults_VerdictIsFalsifiable proves each clause of the fault
// arm's verdict can fail: an unfired regime, a publish that reported success
// under a fault, a crash sweep that missed points, and the two partial
// outcomes — a graph without a snapshot, and a snapshot that does not equal
// the model.
func TestBulkImportFaults_VerdictIsFalsifiable(t *testing.T) {
	t.Parallel()
	good := bulkImportFaultEvidence{
		regimes:  []bulkImportFaultRegime{{name: "r", fired: true, publishFailed: true, outcome: bulkImportOutcomeEmpty}},
		cleanOps: 20, renameOp: 15, cleanSyncs: 5, crashFired: 20,
	}
	if v := bulkImportCheckFaultEvidence(&good); len(v) != 0 {
		t.Fatalf("healthy evidence produced violations: %v", v)
	}
	for name, mutate := range map[string]func(*bulkImportFaultEvidence){
		"unfired regime":            func(e *bulkImportFaultEvidence) { e.regimes[0].fired = false },
		"publish succeeded":         func(e *bulkImportFaultEvidence) { e.regimes[0].publishFailed = false },
		"sweep missed a point":      func(e *bulkImportFaultEvidence) { e.crashFired = 19 },
		"seam not exercised":        func(e *bulkImportFaultEvidence) { e.renameOp = 0 },
		"no component fsync":        func(e *bulkImportFaultEvidence) { e.cleanSyncs = 0 },
		"no operation was measured": func(e *bulkImportFaultEvidence) { e.cleanOps, e.crashFired = 0, 0 },
	} {
		e := good
		e.regimes = append([]bulkImportFaultRegime(nil), good.regimes...)
		mutate(&e)
		if len(bulkImportCheckFaultEvidence(&e)) == 0 {
			t.Errorf("%s: produced no violation", name)
		}
	}

	model, nodes, edges := buildBulkImportFixture(NewSeed(bulkImportParityScenario().DefaultSeed))
	b := bulkimport.New[int64](bulkimport.Options{ExpectNodes: bulkImportNodes})
	if err := b.AddNodes(nodes); err != nil {
		t.Fatalf("AddNodes: %v", err)
	}
	if err := b.AddEdges(edges); err != nil {
		t.Fatalf("AddEdges: %v", err)
	}
	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	g := b.Graph()

	if out, v, err := bulkImportJudgeOutcome(model, true, 0, g, "control"); err != nil || len(v) != 0 ||
		out != bulkImportOutcomeComplete {
		t.Fatalf("the built graph against its own model: outcome %q, violations %v, err %v", out, v, err)
	}
	if out, v, _ := bulkImportJudgeOutcome(model, false, 0, lpg.New[string, int64](g.AdjList().Config()), "empty"); len(v) != 0 ||
		out != bulkImportOutcomeEmpty {
		t.Fatalf("an empty graph without a snapshot: outcome %q, violations %v", out, v)
	}
	if _, v, _ := bulkImportJudgeOutcome(model, false, 0, g, "no snapshot"); len(v) == 0 {
		t.Error("a populated graph without a snapshot produced no violation")
	}
	if _, v, _ := bulkImportJudgeOutcome(model, true, 3, g, "wal"); len(v) == 0 {
		t.Error("a recovery that replayed WAL ops produced no violation")
	}
	model.nodes["phantom"] = &bulkImportModelNode{labels: map[string]struct{}{}, props: map[string]lpg.PropertyValue{}}
	if _, v, _ := bulkImportJudgeOutcome(model, true, 0, g, "mismatch"); len(v) == 0 {
		t.Error("a snapshot that does not equal the model produced no violation")
	}
}
