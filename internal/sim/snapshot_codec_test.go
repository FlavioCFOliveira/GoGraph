package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// the boundary oracle's own sensitivity
// ─────────────────────────────────────────────────────────────────────────────

// cleanSnapshotBoundary is the evidence of a crossing that really proves
// snapshot-sourced recovery: a published manifest, a WAL that held bytes and
// was emptied, and a replay that contributed nothing.
func cleanSnapshotBoundary() snapshotBoundary {
	return snapshotBoundary{
		label:             "probe",
		walBefore:         4096,
		walAfter:          0,
		walOpsReplayed:    0,
		snapshotPublished: true,
		crossed:           true,
	}
}

// TestSnapshotSourcedRecovery_OracleDiscriminates proves the boundary oracle can
// FAIL. Every clause of [checkSnapshotSourcedRecovery] describes a way a
// crossing could complete while proving nothing, so each is perturbed
// independently and must fire; the unperturbed control must stay silent, or the
// four failures above would only show the oracle rejects everything.
func TestSnapshotSourcedRecovery_OracleDiscriminates(t *testing.T) {
	if v := checkSnapshotSourcedRecovery(1, cleanSnapshotBoundary()); len(v) > 0 {
		t.Fatalf("the control (WAL 4096 -> 0, 0 ops replayed, manifest published) fired: %v", v)
	}
	for _, tc := range []struct {
		perturb func(*snapshotBoundary)
		name    string
	}{
		{name: "never crossed", perturb: func(b *snapshotBoundary) { b.crossed = false }},
		{name: "no manifest published", perturb: func(b *snapshotBoundary) { b.snapshotPublished = false }},
		{name: "WAL already empty before the checkpoint", perturb: func(b *snapshotBoundary) { b.walBefore = 0 }},
		{name: "checkpoint refused to truncate", perturb: func(b *snapshotBoundary) { b.walAfter = 4096 }},
		{name: "recovery replayed WAL ops", perturb: func(b *snapshotBoundary) { b.walOpsReplayed = 7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := cleanSnapshotBoundary()
			tc.perturb(&b)
			if v := checkSnapshotSourcedRecovery(1, b); len(v) == 0 {
				t.Fatalf("the oracle accepted a crossing that proves nothing: %s", b.summary())
			}
		})
	}
}

// TestCrossSnapshotBoundary_RefusesAWALOnlyStore proves the crossing fails LOUDLY
// rather than silently degrading when the simulator has no full-stack store: a
// WAL-only store has no snapshot directory, so nothing it recovered could have
// come from a snapshot. This is the misconfiguration guard for the trap rmp
// #2464 found — recovery aimed at the wrong durable layout.
func TestCrossSnapshotBoundary_RefusesAWALOnlyStore(t *testing.T) {
	// Crashes enabled but checkpointing disabled: a durable store opens, and it
	// is WAL-only (no checkpoint dir).
	sm, err := New(Config{
		Seed: 11, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(11)),
		Multigraph: true,
		Crash:      CrashConfig{Enabled: true, CrashProb: 0},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	if sm.store == nil {
		t.Fatal("no durable store opened: the fixture cannot test what it claims")
	}
	if got := sm.store.Config().dir; got != "" {
		t.Fatalf("store dir = %q, want empty (a WAL-only store): the fixture is not WAL-only", got)
	}
	err = sm.crossSnapshotBoundary("probe")
	if err == nil {
		t.Fatal("crossing a WAL-only store succeeded: a run could claim snapshot-sourced recovery with no snapshot")
	}
	if !strings.Contains(err.Error(), "WAL-only") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// type-coverage: the typed matrix through the snapshot codec
// ─────────────────────────────────────────────────────────────────────────────

// TestTypeCoverage_SnapshotSourcedRecoveryMeasured is the evidence test for the
// type-coverage half of rmp #2468. It runs the scenario and asserts on the
// MEASURED durable image rather than on the run merely passing: the terminal
// checkpoint must have emptied a non-empty WAL, and the recovery that followed
// the crash must have replayed ZERO ops — so the full typed matrix the run then
// re-verified (string, integer, float, boolean, list, the plain-ISO control, the
// six temporals and the absent key, each on its expr KIND) can only have come
// out of the snapshot codec.
func TestTypeCoverage_SnapshotSourcedRecoveryMeasured(t *testing.T) {
	sm, report, err := runTypeCoverageSim(context.Background(), 0x7A9E5)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runTypeCoverageSim: %v", err)
	}
	if report != nil {
		t.Fatalf("type-coverage reported a violation:\n%s", report)
	}
	t.Logf("type-coverage terminal boundary: %s", sm.boundary.summary())
	t.Logf("type-coverage run: checkpoints=%d crashes=%d replayedWALOps(total)=%d typedNodes=%d",
		sm.CheckpointCount(), sm.CrashCount(), sm.ReplayedOps(), len(sm.oracle.TypedIDs()))
	assertSnapshotSourced(t, sm, "type-coverage")

	// In-loop checkpointing must have fired on its own: the forced crossing adds
	// exactly one, so a count of 1 would mean the loop never called
	// maybeCheckpoint and only the terminal boundary published anything.
	if sm.CheckpointCount() < 2 {
		t.Fatalf("checkpoints=%d: the run loop published none of its own"+
			" (only the forced terminal crossing did)", sm.CheckpointCount())
	}
	if len(sm.oracle.TypedIDs()) == 0 {
		t.Fatal("no Typed node was modelled: the matrix crossed the boundary empty")
	}

	// Sensitivity: the post-boundary parity check must be able to FAIL on the
	// snapshot-recovered graph. Perturb the MODEL (the engine holds the real
	// values) and the typed battery must fire.
	ids := sm.oracle.TypedIDs()
	props, ok := sm.oracle.TypedNode(ids[0])
	if !ok {
		t.Fatalf("Typed{id:%d} is not modelled", ids[0])
	}
	props["s"] = "perturbed-by-the-test"
	if v := checkTypedAll(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("the typed battery accepted a perturbed model on the SNAPSHOT-recovered graph:" +
			" a value the snapshot codec dropped or corrupted would pass unnoticed")
	}
}

// TestTypeCoverage_CheckpointGateWired proves the checkpoint non-vacuity gate is
// really wired into the type-coverage run rather than merely present: with
// checkpointing DISABLED the run must report the gate's violation instead of
// passing silently. It also pins the gate's ORDER — it is adjudicated before the
// forced crossing publishes a checkpoint of its own, which would otherwise
// silence it.
func TestTypeCoverage_CheckpointGateWired(t *testing.T) {
	sc := typeCoverageScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.MaxTicks = 120
	cfg.Checkpoint = CheckpointConfig{} // disabled: no snapshot can be published

	sm, report, err := runTypeCoverageWith(context.Background(), cfg)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runTypeCoverageWith: %v", err)
	}
	if report == nil {
		t.Fatal("a run that published no checkpoint passed silently: the gate is not wired")
	}
	if !strings.Contains(report.String(), "published NO checkpoint") {
		t.Fatalf("the report is not the checkpoint non-vacuity gate:\n%s", report)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// edge-properties: per-instance maps through the snapshot codec
// ─────────────────────────────────────────────────────────────────────────────

// TestEdgeProperties_SnapshotSourcedRecoveryMeasured is the evidence test for the
// edge-properties half of rmp #2468: the per-instance property maps of parallel
// KNOWS twins must survive a recovery the WAL contributed nothing to, so they
// come out of the snapshot's per-handle component (edgehandles.bin) rather than
// out of the per-pair union labels.bin/properties.bin collapse parallel edges
// onto.
func TestEdgeProperties_SnapshotSourcedRecoveryMeasured(t *testing.T) {
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	// Record every SCHEDULED crash so the in-loop half of the coverage can be
	// asserted too: a crash after the first checkpoint recovers through
	// recovery.OpenFS (snapshot + WAL tail), which is a different path from both
	// the WAL-only replay before it and the pure-snapshot terminal crossing.
	// The hook takes no draws, so the run stays bit-reproducible.
	var crashTicks []int64
	cfg.OnCrash = func(tick int64, _ int) { crashTicks = append(crashTicks, tick) }

	sm, report, err := runEdgePropertiesWith(context.Background(), cfg)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runEdgePropertiesWith: %v", err)
	}
	if report != nil {
		t.Fatalf("edge-properties reported a violation:\n%s", report)
	}
	insts := sm.oracle.KnowsInstancesByEID()
	t.Logf("edge-properties terminal boundary: %s", sm.boundary.summary())
	t.Logf("edge-properties run: checkpoints=%d crashes=%d replayedWALOps(total)=%d liveInstances=%d parallelPairs=%d",
		sm.CheckpointCount(), sm.CrashCount(), sm.ReplayedOps(), len(insts), countParallelPairs(insts))
	t.Logf("edge-properties scheduled crash ticks=%v (first checkpoint at tick %d)",
		crashTicks, edgePropertiesCheckpointEvery)
	assertSnapshotSourced(t, sm, "edge-properties")

	// The in-loop half: at least one scheduled crash must land AFTER the first
	// checkpoint, so the run also exercises the snapshot + WAL-tail recovery
	// (recovery.OpenFS) and not only the WAL-only replay that preceded #2468.
	postCheckpointCrashes := 0
	for _, tk := range crashTicks {
		if tk > edgePropertiesCheckpointEvery {
			postCheckpointCrashes++
		}
	}
	if postCheckpointCrashes == 0 {
		t.Fatalf("no scheduled crash landed after the first checkpoint (crash ticks %v):"+
			" every in-loop recovery was a WAL-only replay", crashTicks)
	}

	if sm.CheckpointCount() < 2 {
		t.Fatalf("checkpoints=%d: the run loop published none of its own"+
			" (only the forced terminal crossing did) — maybeCheckpoint is not wired",
			sm.CheckpointCount())
	}
	// Non-vacuity of the property under test: a parallel pair must have SURVIVED
	// to the boundary, or the per-handle bag was never the thing being recovered.
	if got := countParallelPairs(insts); got == 0 {
		t.Fatal("no parallel-edge pair survived to the snapshot boundary:" +
			" the by-handle bag versus per-pair aggregate distinction was never exercised")
	}

	// Sensitivity on the SNAPSHOT-recovered graph: perturb one instance's
	// modelled map and the read-back must fire.
	perturbOneKnowsInstance(t, sm)
	if v := CheckEdgeProperties(1, sm.oracle, sm.engine); len(v) == 0 {
		t.Fatal("the edge-property check accepted a perturbed model on the SNAPSHOT-recovered graph:" +
			" a per-instance map the snapshot codec collapsed would pass unnoticed")
	}
}

// TestEdgeProperties_CheckpointGateWired proves the checkpoint non-vacuity gate
// is wired into the edge-properties run: with checkpointing disabled the run
// must report the gate's violation rather than reaching (and passing) the
// terminal boundary. This is the guard for the exact trap rmp #2457/#2464 hit
// twice — a CheckpointConfig that a custom run loop never acts on.
func TestEdgeProperties_CheckpointGateWired(t *testing.T) {
	sc := edgePropertiesScenario()
	cfg := sc.DeterministicConfig(sc.DefaultSeed)
	cfg.MaxTicks = 150
	cfg.Checkpoint = CheckpointConfig{} // disabled: no snapshot can be published

	sm, report, err := runEdgePropertiesWith(context.Background(), cfg)
	if sm != nil {
		t.Cleanup(func() { _ = sm.Close() })
	}
	if err != nil {
		t.Fatalf("runEdgePropertiesWith: %v", err)
	}
	if report == nil {
		t.Fatal("a run that published no checkpoint passed silently: the gate is not wired")
	}
	if !strings.Contains(report.String(), "published NO checkpoint") {
		t.Fatalf("the report is not the checkpoint non-vacuity gate:\n%s", report)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the typed value matrix ON AN EDGE INSTANCE, through the snapshot codec
// ─────────────────────────────────────────────────────────────────────────────

// tmplCreateKnowsTyped links two Persons with a KNOWS instance carrying a value
// of every round-tripping non-temporal kind. It exists only for the
// snapshot-codec probe below: the scenario workload deliberately keeps its
// three-key shape so the per-op counters oracle stays pinned, while this
// template widens the value matrix the per-handle component must carry.
//
// The value parameters are named pS/pI/pF/pB/pL rather than after their property
// keys because $a and $b already bind the two endpoint NAMES: a `$b` carrying
// the boolean would silently rebind the endpoint, the MATCH would find nothing,
// and the CREATE would commit having created no edge at all.
const tmplCreateKnowsTyped = "MATCH (a:Person {name:$a}),(b:Person {name:$b}) " +
	"CREATE (a)-[:KNOWS {eid:$eid, s:$pS, i:$pI, f:$pF, b:$pB, lst:$pL}]->(b)"

// knowsTypedKeys are the probe's property keys, in projection order, and
// knowsTypedParams maps each to the parameter that carries its value.
var knowsTypedKeys = []string{"s", "i", "f", "b", "lst"}

var knowsTypedParams = map[string]string{
	"s": "pS", "i": "pI", "f": "pF", "b": "pB", "lst": "pL",
}

// knowsTypedExpectKind is the expr KIND each probe property must read back as.
// The kind is asserted as well as the text because a value that degraded to an
// untagged string can render identically — the same reason the type-coverage
// checker asserts kinds (rmp #2457).
var knowsTypedExpectKind = map[string]expr.Kind{
	"s":   expr.KindString,
	"i":   expr.KindInteger,
	"f":   expr.KindFloat,
	"b":   expr.KindBool,
	"lst": expr.KindList,
}

// TestEdgeProperties_TypedMatrixThroughSnapshotCodec drives the full
// non-temporal value matrix — string, integer, float, boolean, list — onto TWO
// PARALLEL KNOWS instances between the same endpoints, with a DIFFERENT value of
// each kind per instance, then forces the run across the snapshot boundary and
// reads each instance back pinned by its eid.
//
// The parallel pair is the point. labels.bin and properties.bin collapse
// parallel edges onto one (src, dst) record, so the per-instance maps have no
// home in a snapshot except the per-handle component (edgehandles.bin), which
// carries its own kind byte per property and is emitted ONLY when the graph
// holds per-handle metadata. A snapshot that omitted or mis-encoded it would
// hand both twins the per-pair union — which the cross-check at the end proves
// this probe would catch.
func TestEdgeProperties_TypedMatrixThroughSnapshotCodec(t *testing.T) {
	ctx := context.Background()
	sm, err := New(Config{
		Seed: 21, MaxTicks: 1, Workload: edgePropertiesWorkload(NewSeed(21)),
		Multigraph: true,
		// Full-stack durable store: WAL at db/wal, snapshot at db/snapshot. The
		// cadence is irrelevant — this probe has no tick loop and checkpoints by
		// crossing the boundary explicitly.
		Checkpoint: CheckpointConfig{Enabled: true, Every: 1_000_000},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	for _, name := range []string{"A", "B"} {
		if !runWriteCommitted(ctx, sm.engine, tmplCreatePerson, map[string]any{"name": name, "age": int64(30)}) {
			t.Fatalf("CREATE Person %q did not commit", name)
		}
	}
	// Two parallel instances, each with its OWN value of every kind.
	want := map[int64]map[string]any{
		1: {"s": "one", "i": int64(11), "f": 1.5, "b": true, "lst": []any{int64(1), int64(2), int64(3)}},
		2: {"s": "two", "i": int64(22), "f": 2.5, "b": false, "lst": []any{int64(9)}},
	}
	for _, eid := range []int64{1, 2} {
		params := map[string]any{"a": "A", "b": "B", "eid": eid}
		for k, v := range want[eid] {
			params[knowsTypedParams[k]] = v
		}
		if !runWriteCommitted(ctx, sm.engine, tmplCreateKnowsTyped, params) {
			t.Fatalf("CREATE typed KNOWS instance eid=%d did not commit", eid)
		}
	}
	// A committed statement is not a written edge: a CREATE whose MATCH found no
	// endpoint commits happily having created nothing. Count what really landed
	// before anything below claims to have round-tripped it.
	if n, err := sm.engine.EdgeCount(); err != nil {
		t.Fatalf("EdgeCount: %v", err)
	} else if n != 2 {
		t.Fatalf("engine holds %d KNOWS edges, want 2 (a parallel pair): the fixture built nothing to test", n)
	}
	// Baseline BEFORE the boundary: the matrix must already round-trip on the
	// live engine, so a failure afterwards is unambiguously a snapshot defect.
	for _, eid := range []int64{1, 2} {
		if diffs := diffTypedKnows(t, sm, eid, want[eid]); len(diffs) > 0 {
			t.Fatalf("pre-snapshot baseline for eid=%d: %s", eid, strings.Join(diffs, "; "))
		}
	}

	if err := sm.crossSnapshotBoundary("typed edge matrix probe"); err != nil {
		t.Fatalf("crossSnapshotBoundary: %v", err)
	}
	t.Logf("typed edge matrix boundary: %s", sm.boundary.summary())
	if v := checkSnapshotSourcedRecovery(1, sm.boundary); len(v) > 0 {
		t.Fatalf("the crossing does not prove snapshot-sourced recovery: %v", v)
	}

	// Post-boundary: the WAL is empty and replayed nothing, so every value below
	// was reconstructed by the snapshot codec alone.
	for _, eid := range []int64{1, 2} {
		if diffs := diffTypedKnows(t, sm, eid, want[eid]); len(diffs) > 0 {
			t.Errorf("eid=%d did NOT round-trip through the snapshot codec: %s", eid, strings.Join(diffs, "; "))
		}
	}

	// The probe must DISCRIMINATE: compare each instance against its twin's
	// expectations. If the snapshot had collapsed both onto a per-pair union (or
	// the probe were reading the wrong instance), this cross-check would pass —
	// so requiring it to fail is what makes the two checks above evidence.
	if diffs := diffTypedKnows(t, sm, 1, want[2]); len(diffs) == 0 {
		t.Error("instance eid=1 matched its TWIN's property map: the probe cannot tell the two apart," +
			" so its success above proves nothing about per-handle identity")
	}
}

// diffTypedKnows reads the KNOWS instance pinned by eid back through the engine
// and returns one description per property that disagrees with want, on either
// its expr KIND or its canonical value. An empty result means the instance's own
// map round-tripped exactly.
func diffTypedKnows(t *testing.T, sm *Simulator, eid int64, want map[string]any) []string {
	t.Helper()
	cols := make([]string, len(knowsTypedKeys))
	for i, k := range knowsTypedKeys {
		cols[i] = "r." + k
	}
	q := fmt.Sprintf("MATCH (:Person {name:'A'})-[r:KNOWS]->(:Person {name:'B'}) WHERE r.eid = %d RETURN %s",
		eid, strings.Join(cols, ", "))
	got, err := sm.engine.projectRowValues(context.Background(), q, len(knowsTypedKeys))
	if err != nil {
		t.Fatalf("read-back of eid=%d failed: %v", eid, err)
	}
	if got == nil {
		return []string{fmt.Sprintf("instance eid=%d is ABSENT", eid)}
	}
	var diffs []string
	for i, k := range knowsTypedKeys {
		gv := got[i]
		if gv == nil {
			diffs = append(diffs, fmt.Sprintf("%s: no value returned", k))
			continue
		}
		if wantKind := knowsTypedExpectKind[k]; gv.Kind() != wantKind {
			diffs = append(diffs, fmt.Sprintf("%s: kind %v (value %s), want kind %v",
				k, gv.Kind(), gv.String(), wantKind))
			continue
		}
		if wantText := canonicalValueString(want[k]); gv.String() != wantText {
			diffs = append(diffs, fmt.Sprintf("%s = %s, want %s", k, gv.String(), wantText))
		}
	}
	return diffs
}

// ─────────────────────────────────────────────────────────────────────────────
// shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// assertSnapshotSourced fails the test unless the simulator's terminal boundary
// really put the snapshot codec between the writes and the read-back: a
// published manifest, a WAL that held bytes and was emptied, and a recovery that
// replayed none of them.
func assertSnapshotSourced(t *testing.T, sm *Simulator, what string) {
	t.Helper()
	b := sm.boundary
	if !b.crossed {
		t.Fatalf("%s: the run never crossed the snapshot boundary", what)
	}
	if !b.snapshotPublished {
		t.Fatalf("%s: no snapshot manifest was published (%s)", what, b.summary())
	}
	if b.walBefore <= 0 {
		t.Fatalf("%s: the WAL was empty before the checkpoint, so its truncation proves nothing (%s)",
			what, b.summary())
	}
	if b.walAfter != 0 {
		t.Fatalf("%s: the checkpoint left %d WAL bytes on disk, so recovery could still replay them (%s)",
			what, b.walAfter, b.summary())
	}
	if b.walOpsReplayed != 0 {
		t.Fatalf("%s: recovery replayed %d WAL ops, so the read-back is not snapshot-sourced (%s)",
			what, b.walOpsReplayed, b.summary())
	}
	if v := checkSnapshotSourcedRecovery(1, b); len(v) > 0 {
		t.Fatalf("%s: the boundary oracle disagrees with the assertions above: %v", what, v)
	}
}

// countParallelPairs counts the endpoint pairs carrying more than one live
// KNOWS instance — the shape that makes the per-handle bag load-bearing.
func countParallelPairs(insts []KnowsInstance) int {
	per := make(map[string]int, len(insts))
	for _, in := range insts {
		per[in.Src+"\x00"+in.Dst]++
	}
	n := 0
	for _, c := range per {
		if c > 1 {
			n++
		}
	}
	return n
}

// perturbOneKnowsInstance corrupts one modelled KNOWS instance's property map,
// preferring an instance that has a parallel twin so the perturbation lands
// exactly where per-handle identity matters.
func perturbOneKnowsInstance(t *testing.T, sm *Simulator) {
	t.Helper()
	insts := sm.oracle.KnowsInstancesByEID()
	if len(insts) == 0 {
		t.Fatal("no live KNOWS instance to perturb")
	}
	per := make(map[string]int, len(insts))
	for _, in := range insts {
		per[in.Src+"\x00"+in.Dst]++
	}
	target := insts[0]
	for _, in := range insts {
		if per[in.Src+"\x00"+in.Dst] > 1 {
			target = in
			break
		}
	}
	k := edgeKey{
		src:   sm.oracle.byName[target.Src],
		dst:   sm.oracle.byName[target.Dst],
		label: "KNOWS",
		eid:   target.EID,
	}
	e, ok := sm.oracle.edges[k]
	if !ok {
		t.Fatalf("modelled instance %+v is not in the oracle's edge map", target)
	}
	e.Properties["weight"] = 999_999.0
}
