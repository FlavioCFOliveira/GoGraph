package sim

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
)

// TestBulkLoadOracle_Scenario_Passes runs the registered scenario in its own
// configuration: every adjacency configuration, both build paths, all three
// ingest entry points, the row cap, the armed publish faults and the host-crash
// window must all agree with the harness model.
func TestBulkLoadOracle_Scenario_Passes(t *testing.T) {
	t.Parallel()
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBulkLoadOracle)
	if !ok {
		t.Fatalf("bulk-load-oracle scenario not registered")
	}
	if !sc.Mode.Reproducible() {
		t.Fatalf("bulk-load-oracle mode is %s, want a bit-reproducible mode", sc.Mode)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("bulk-load-oracle run: %v", err)
	}
	if report != nil {
		t.Fatalf("bulk-load-oracle reported a violation:\n%s", report)
	}
}

// TestBulkLoadOracle_NoGoroutineLeak guards the arms that spawn producer
// goroutines — the clean Drain, the cancelled Drain and the capped Drain. Two of
// the three end with the consumer walking away from a live producer, which is
// exactly the shape that leaks if the arm forgets to drain or cancel.
//
// It does NOT call t.Parallel: goleak and t.Parallel are structurally
// incompatible, because a parked parent test function is itself an extra
// goroutine goleak does not ignore.
func TestBulkLoadOracle_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	sc := bulkLoadOracleScenario()
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}
}

// TestBulkLoadOracle_NonVacuous is the measured-evidence gate. Every number here
// is read off the run rather than inferred, so a scenario that silently
// degenerated — an empty fixture, a fault that never bit, a cap that was never
// crossed, a crash that changed nothing — passes the happy test above and fails
// here.
//
//nolint:gocyclo // one linear oracle: build the fixture, run the bulk load, then assert every non-vacuity clause in sequence.
func TestBulkLoadOracle_NonVacuous(t *testing.T) {
	t.Parallel()
	sc := bulkLoadOracleScenario()
	ev, report, err := runBulkLoadOracleWith(context.Background(), sc.DefaultSeed, defaultBulkOracleOptions())
	if err != nil {
		t.Fatalf("runBulkLoadOracleWith: %v", err)
	}
	if report != nil {
		t.Fatalf("violation:\n%s", report)
	}

	t.Logf("fixture: records=%d keys=%d duplicatePairs=%d selfLoops=%d reversedPairs=%d",
		len(ev.fixture.edges), ev.fixture.distinctKeys, ev.fixture.duplicatePairs,
		ev.fixture.selfLoops, ev.fixture.reversedPairs)
	for i := range ev.configs {
		c := ev.configs[i]
		t.Logf("config %-18s entries=%4d dropped=%3d mirrored=%4d modelSize=%4d closedForm=%4d "+
			"seqBytes=%d parBytes=%d byteIdentical=%t sliceIdentical=%t",
			c.name, c.entries, c.dupsDropped, c.mirrored, c.modelSize, c.closedFormSize,
			c.seqBytes, c.parBytes, c.byteIdentical, c.sliceIdentical)
	}
	t.Logf("streaming: drainClean=%d drainCancelled=%d rowsAfterCancel=%d ctxErr=%t batchRows=%d",
		ev.drainClean, ev.drainCancelled, ev.drainCancelRows, ev.drainCancelErrIsCtx, ev.batchRows)
	t.Logf("caps: addBatch=%t rowsKept=%d add=%t drain=%t drainRows=%d drained=%d discarded=%d",
		ev.batchCapErrIsTooManyRows, ev.batchCapRows, ev.addAfterCapErrIsTooMany,
		ev.drainCapErrIsTooManyRows, ev.drainCapRows, ev.drainCapDrained, ev.drainCapDiscarded)
	t.Logf("realfs: bytes=%d published=%t badPathWrapped=%t csrSalvaged=%t rows=%d",
		ev.realFSBytes, ev.realFSPublished, ev.badPathErrWrapped, ev.badPathCSRReturned, ev.badPathRows)
	t.Logf("publish faults: outcomes=%v errored=%v strandedTemps=%d parentFsyncAsymmetry=%t",
		ev.faultOutcomes, ev.faultErrors, ev.strandedTemps, ev.parentFsyncFaultPresentAndComplete)
	t.Logf("corruption: rejected=%t err=%q", ev.corruptionRejected, ev.corruptionErr)
	t.Logf("media faults: attempts=%d errors=%d bytesDiffered=%d failedWithPrior=%d outcomes=%v",
		ev.mediaAttempts, ev.mediaErrors, ev.mediaBytesDiffered,
		ev.mediaFailedWithPriorImage, ev.mediaOutcomes)
	t.Logf("crash window: control=gen%d treatment=gen%d writeback=gen%d pendingRenames=%d "+
		"rolledBack=%d discardedBytes=%d durableBefore=%d rollbacksFired=%d writebacksFired=%d",
		ev.crashControlGeneration, ev.crashTreatGeneration, ev.crashWritebackGeneration,
		ev.crashPendingRenames, ev.crashRolledBack, ev.crashDiscardedBytes,
		ev.crashDurableSizeBefore, ev.renameRollbacksFired, ev.renameWritebacksFired)

	// --- The fixture really is the shape every coverage gate assumes. ---
	if got := len(ev.fixture.edges); got < bulkOracleMinEdgeRecords {
		t.Errorf("fixture holds %d edge records, want >= %d", got, bulkOracleMinEdgeRecords)
	}
	if ev.fixture.duplicatePairs == 0 || ev.fixture.selfLoops == 0 || ev.fixture.reversedPairs == 0 {
		t.Errorf("the fixture lacks a structural feature: duplicates=%d selfLoops=%d reversed=%d",
			ev.fixture.duplicatePairs, ev.fixture.selfLoops, ev.fixture.reversedPairs)
	}

	// --- All four configurations ran, and each behaved as its config implies. ---
	if len(ev.configs) != 4 {
		t.Fatalf("%d configurations adjudicated, want 4", len(ev.configs))
	}
	byName := map[string]bulkOracleConfigEvidence{}
	for i := range ev.configs {
		byName[ev.configs[i].name] = ev.configs[i]
	}
	// A directed multigraph stores every record and nothing else: the strongest
	// exact statement available about the loader's edge accounting.
	if dm := byName["directed-multi"]; dm.modelSize != uint64(len(ev.fixture.edges)) {
		t.Errorf("directed multigraph stored %d entries for %d records, want equality",
			dm.modelSize, len(ev.fixture.edges))
	}
	// Mirroring must roughly double an undirected load, and dedup must shrink a
	// simple one. Asserting the ORDERING rather than the exact numbers keeps this
	// robust across seeds while still catching a configuration that was ignored.
	if byName["undirected-multi"].modelSize <= byName["directed-multi"].modelSize {
		t.Errorf("undirected multigraph (%d) did not exceed directed multigraph (%d): mirroring never happened",
			byName["undirected-multi"].modelSize, byName["directed-multi"].modelSize)
	}
	if byName["directed-simple"].modelSize >= byName["directed-multi"].modelSize {
		t.Errorf("directed simple (%d) did not undercut directed multigraph (%d): dedup never happened",
			byName["directed-simple"].modelSize, byName["directed-multi"].modelSize)
	}
	if byName["undirected-simple"].modelSize >= byName["undirected-multi"].modelSize {
		t.Errorf("undirected simple (%d) did not undercut undirected multigraph (%d)",
			byName["undirected-simple"].modelSize, byName["undirected-multi"].modelSize)
	}
	for i := range ev.configs {
		c := ev.configs[i]
		if c.modelSize != c.closedFormSize {
			t.Errorf("[%s] the model's own two views of its edge set disagree: %d vs %d",
				c.name, c.modelSize, c.closedFormSize)
		}
		if !c.byteIdentical || !c.sliceIdentical {
			t.Errorf("[%s] parallel/sequential identity not established (bytes=%t slices=%t)",
				c.name, c.byteIdentical, c.sliceIdentical)
		}
		if c.seqBytes < bulkOracleMinPublishBytes {
			t.Errorf("[%s] published only %d bytes; comparing two trivial images proves nothing",
				c.name, c.seqBytes)
		}
	}

	// --- The streaming and cap branches were genuinely entered. ---
	if !ev.drainCancelErrIsCtx {
		t.Error("no Drain cancellation was observed, so the ctx.Err() clause never had anything to check")
	}
	if ev.drainCancelled != bulkOracleCancelAt || ev.drainCancelRows != bulkOracleCancelAt {
		t.Errorf("the cancelled Drain stopped at drained=%d rows=%d, want both = %d exactly "+
			"(the cancellation point is meant to be deterministic, not racy)",
			ev.drainCancelled, ev.drainCancelRows, bulkOracleCancelAt)
	}
	if ev.batchCapRows != bulkOracleRowCap || ev.drainCapRows != bulkOracleRowCap ||
		ev.drainCapDrained != bulkOracleRowCap {
		t.Errorf("the cap was not honoured exactly: AddBatch kept %d, Drain kept %d and drained %d, want %d",
			ev.batchCapRows, ev.drainCapRows, ev.drainCapDrained, bulkOracleRowCap)
	}
	if !ev.batchCapErrIsTooManyRows || !ev.addAfterCapErrIsTooMany || !ev.drainCapErrIsTooManyRows {
		t.Error("ErrTooManyRows was not observed on all three ingest entry points")
	}
	// Drain consumes the edge that trips the cap and then drops it, so the
	// producer's sends split as ingested + 1 refused + discarded. Pinning the
	// exact number keeps the split visible: a Drain that consumed two, or peeked
	// without consuming, moves this by one.
	if want := len(ev.fixture.edges) - bulkOracleRowCap - 1; ev.drainCapDiscarded != want {
		t.Errorf("the capped Drain left %d sends in flight, want %d (%d sent - %d ingested - 1 refused)",
			ev.drainCapDiscarded, want, len(ev.fixture.edges), bulkOracleRowCap)
	}

	// --- Finalise's own publication and its failure contract. ---
	if !ev.realFSPublished || ev.realFSBytes < bulkOracleMinPublishBytes {
		t.Errorf("Finalise's own publication is missing or trivial: published=%t bytes=%d",
			ev.realFSPublished, ev.realFSBytes)
	}
	if !ev.badPathErrWrapped || !ev.badPathCSRReturned {
		t.Errorf("the unwritable-OutputPath contract was not observed: wrapped=%t csrReturned=%t",
			ev.badPathErrWrapped, ev.badPathCSRReturned)
	}

	// --- Every armed fault actually bit, and each landed where documented. ---
	for _, name := range []string{"sync-first", "sync-republish", "rename", "enospc", "parent-fsync"} {
		if !ev.faultErrors[name] {
			t.Errorf("the %q fault produced no error: its verdict is vacuous", name)
		}
	}
	if got := ev.faultOutcomes["sync-first"]; got != bulkOracleAbsent {
		t.Errorf("a first publish that failed at the fsync left the path %s, want absent", got)
	}
	for _, name := range []string{"sync-republish", "rename", "enospc", "parent-fsync"} {
		if got := ev.faultOutcomes[name]; got != bulkOracleComplete {
			t.Errorf("after the %q fault the path reads as %s, want complete", name, got)
		}
	}
	if !ev.parentFsyncFaultPresentAndComplete {
		t.Error("the parent-fsync asymmetry (error returned over a present, complete file) was not observed")
	}
	if !ev.corruptionRejected {
		t.Error("no corruption rejection was observed")
	}

	// --- The media arm: the measurement that justifies the THREE-way oracle. ---
	if ev.mediaAttempts != bulkOracleFaultAttempts {
		t.Errorf("media arm made %d attempts, want %d", ev.mediaAttempts, bulkOracleFaultAttempts)
	}
	if ev.mediaOutcomes[bulkOracleRejected] == 0 {
		t.Errorf("the media arm produced no REJECTED outcome (%v). That outcome is the whole reason the "+
			"post-fault oracle admits three states rather than two; without it, this arm is not "+
			"exercising the distinction it exists for", ev.mediaOutcomes)
	}
	if ev.mediaErrors == 0 {
		t.Error("the media arm saw no publish failure: the fault rate did not bite")
	}
	// The unchanged-on-failure clause must have been LIVE, not dormant: at least
	// one failed attempt has to have been adjudicated against a target that
	// already held something.
	if ev.mediaFailedWithPriorImage == 0 {
		t.Error("no failed media attempt was adjudicated against a pre-existing image, so the " +
			"unchanged-on-failure clause never actually compared anything")
	}
	if ev.mediaFailedReplacements != 0 {
		t.Errorf("%d failed publish(es) replaced the target with a partial image", ev.mediaFailedReplacements)
	}

	// --- The crash window really is a differential. ---
	if ev.crashControlGeneration != 2 {
		t.Errorf("crash control left generation %d, want 2 (a completed publish must survive power loss)",
			ev.crashControlGeneration)
	}
	if ev.crashTreatGeneration != 1 {
		t.Errorf("crash treatment left generation %d, want 1 (a rolled-back rename must restore the "+
			"previous generation whole)", ev.crashTreatGeneration)
	}
	if ev.crashWritebackGeneration != 2 {
		t.Errorf("crash writeback branch left generation %d, want 2", ev.crashWritebackGeneration)
	}
	if ev.crashPendingRenames == 0 || ev.crashRolledBack == 0 {
		t.Errorf("the rename window was not entered: pending=%d rolledBack=%d",
			ev.crashPendingRenames, ev.crashRolledBack)
	}
	if ev.renameRollbacksFired != 1 || ev.renameWritebacksFired != 1 {
		t.Errorf("the two pinned crash branches did not each fire once: rollbacks=%d writebacks=%d",
			ev.renameRollbacksFired, ev.renameWritebacksFired)
	}
	// Documented above and easy to misread: the treatment crash destroys a NAME,
	// not a byte, so the byte counter is expected to be zero. Pin the expectation
	// so a future reader does not adopt it as a non-vacuity witness.
	if ev.crashDiscardedBytes != 0 {
		t.Logf("note: the treatment crash discarded %d bytes; the arm's documentation says this is "+
			"normally zero because the loss is a directory entry, not data", ev.crashDiscardedBytes)
	}
	if ev.crashDurableSizeBefore < bulkOracleMinPublishBytes {
		t.Errorf("the control's durable image before the crash was %d bytes: nothing substantial was "+
			"durable, so surviving the crash proves little", ev.crashDurableSizeBefore)
	}
}

// TestBulkLoadOracle_Deterministic asserts the run is a pure function of the
// seed: two runs must produce the identical report (both nil, or identical text)
// and identical evidence on every measured axis.
//
// The evidence comparison is the stronger half. Two nil reports agree trivially;
// two runs whose published byte counts, fault outcomes and crash generations all
// agree have actually done the same thing twice.
func TestBulkLoadOracle_Deterministic(t *testing.T) {
	t.Parallel()
	sc := bulkLoadOracleScenario()
	a, ra, err := runBulkLoadOracleWith(context.Background(), sc.DefaultSeed, defaultBulkOracleOptions())
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	b, rb, err := runBulkLoadOracleWith(context.Background(), sc.DefaultSeed, defaultBulkOracleOptions())
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	if (ra == nil) != (rb == nil) {
		t.Fatalf("one run reported and the other did not: A=%v B=%v", ra, rb)
	}
	if ra != nil && ra.String() != rb.String() {
		t.Fatalf("two runs of seed %d produced different reports:\nA:\n%s\nB:\n%s",
			sc.DefaultSeed, ra, rb)
	}

	if !slices.EqualFunc(a.fixture.edges, b.fixture.edges, func(x, y bulk.Edge) bool { return x == y }) {
		t.Error("the seed-drawn edge stream differs between two runs of the same seed")
	}
	if len(a.configs) != len(b.configs) {
		t.Fatalf("run A adjudicated %d configs, run B %d", len(a.configs), len(b.configs))
	}
	for i := range a.configs {
		if a.configs[i] != b.configs[i] {
			t.Errorf("config %d differs between runs:\nA: %+v\nB: %+v", i, a.configs[i], b.configs[i])
		}
	}
	for _, k := range []string{"sync-first", "sync-republish", "rename", "enospc", "parent-fsync"} {
		if a.faultOutcomes[k] != b.faultOutcomes[k] {
			t.Errorf("fault %q: outcome %s vs %s", k, a.faultOutcomes[k], b.faultOutcomes[k])
		}
	}
	for _, o := range []bulkOracleOutcome{bulkOracleAbsent, bulkOracleComplete, bulkOracleRejected} {
		if a.mediaOutcomes[o] != b.mediaOutcomes[o] {
			t.Errorf("media outcome %s: %d vs %d", o, a.mediaOutcomes[o], b.mediaOutcomes[o])
		}
	}
	if a.mediaErrors != b.mediaErrors || a.mediaBytesDiffered != b.mediaBytesDiffered {
		t.Errorf("media fault stream diverged: errors %d/%d bytesDiffered %d/%d",
			a.mediaErrors, b.mediaErrors, a.mediaBytesDiffered, b.mediaBytesDiffered)
	}
	if a.crashControlGeneration != b.crashControlGeneration ||
		a.crashTreatGeneration != b.crashTreatGeneration ||
		a.crashWritebackGeneration != b.crashWritebackGeneration {
		t.Errorf("crash-window generations diverged: A=%d/%d/%d B=%d/%d/%d",
			a.crashControlGeneration, a.crashTreatGeneration, a.crashWritebackGeneration,
			b.crashControlGeneration, b.crashTreatGeneration, b.crashWritebackGeneration)
	}
	if a.drainCancelled != b.drainCancelled || a.batchCapRows != b.batchCapRows {
		t.Errorf("streaming/cap accounting diverged: cancelled %d/%d capRows %d/%d",
			a.drainCancelled, b.drainCancelled, a.batchCapRows, b.batchCapRows)
	}
}

// TestBulkLoadOracle_CheckerDiscriminates proves the CONTENT checkers can fail,
// dimension by dimension, without touching the engine.
//
// It builds the model's own expected shape, hands the checker a CSR that differs
// from it in exactly one way, and requires a violation naming that dimension. A
// comparison that silently accepted any of these would make every "complete"
// verdict in the scenario meaningless — including the byte-identity and
// crash-generation verdicts, which are all expressed through these two
// functions.
//
// The ORDER case is the one that cannot be reached through the model-perturbation
// seam (the model sorts its own rows, so an unsorted model is not
// constructible), which is precisely why it is tested here instead.
func TestBulkLoadOracle_CheckerDiscriminates(t *testing.T) {
	t.Parallel()
	edges := buildBulkOracleFixture(NewSeed(0xB0142488), false).edges
	model := buildBulkOracleModel(edges, true, true)
	want := model.expect()

	// The control: the model's own shape, rebuilt as a CSR, must PASS. Without
	// this, a checker that rejected everything would look like a good checker.
	base := csr.FromArrays(slices.Clone(want.vertices), slices.Clone(want.edges),
		slices.Clone(want.weights), want.order, want.size)
	if v := bulkOracleCheckCSR("control", want, base); len(v) > 0 {
		t.Fatalf("the checker rejects the model's own shape, so every RED below is meaningless:\n%v", v)
	}

	// Locate a row holding at least two entries with DIFFERENT destinations, so
	// the order case is a real reordering rather than a no-op.
	rowStart, rowEnd := -1, -1
	for i := 0; i+1 < len(want.vertices); i++ {
		lo, hi := int(want.vertices[i]), int(want.vertices[i+1])
		if hi-lo >= 2 && want.edges[lo] != want.edges[lo+1] {
			rowStart, rowEnd = lo, hi
			break
		}
	}
	if rowStart < 0 {
		t.Fatal("no row with two distinct destinations: the order case cannot be constructed")
	}

	for _, tc := range []struct {
		mutate func() *csr.CSR[int64]
		name   string
		wantIn string
	}{
		{
			name:   "MissingEdge",
			wantIn: "edge multiset",
			mutate: func() *csr.CSR[int64] {
				e := slices.Clone(want.edges)
				w := slices.Clone(want.weights)
				return csr.FromArrays(slices.Clone(want.vertices), e[:len(e)-1], w[:len(w)-1],
					want.order, want.size)
			},
		},
		{
			name:   "WrongDestination",
			wantIn: "edge multiset",
			mutate: func() *csr.CSR[int64] {
				e := slices.Clone(want.edges)
				e[rowStart]++
				return csr.FromArrays(slices.Clone(want.vertices), e, slices.Clone(want.weights),
					want.order, want.size)
			},
		},
		{
			name:   "SwappedWithinRow",
			wantIn: "edge multiset",
			mutate: func() *csr.CSR[int64] {
				e := slices.Clone(want.edges)
				// Reverse the row: a genuine ordering change that keeps the
				// multiset identical, so only an order-sensitive comparison sees it.
				slices.Reverse(e[rowStart:rowEnd])
				return csr.FromArrays(slices.Clone(want.vertices), e, slices.Clone(want.weights),
					want.order, want.size)
			},
		},
		{
			name:   "WrongWeight",
			wantIn: "weight column",
			mutate: func() *csr.CSR[int64] {
				w := slices.Clone(want.weights)
				w[rowStart]++
				return csr.FromArrays(slices.Clone(want.vertices), slices.Clone(want.edges), w,
					want.order, want.size)
			},
		},
		{
			name:   "SignFlippedWeight",
			wantIn: "weight column",
			mutate: func() *csr.CSR[int64] {
				w := slices.Clone(want.weights)
				w[rowStart] = -w[rowStart] - 1 // never a fixed point, even at zero
				return csr.FromArrays(slices.Clone(want.vertices), slices.Clone(want.edges), w,
					want.order, want.size)
			},
		},
		{
			name:   "ShiftedOffsets",
			wantIn: "offsets array",
			mutate: func() *csr.CSR[int64] {
				vs := slices.Clone(want.vertices)
				// Move one boundary: the multiset and the weights are untouched, so
				// only the offsets comparison can see it.
				for i := 1; i+1 < len(vs); i++ {
					if vs[i] > vs[i-1] {
						vs[i]--
						break
					}
				}
				return csr.FromArrays(vs, slices.Clone(want.edges), slices.Clone(want.weights),
					want.order, want.size)
			},
		},
		{
			name:   "WrongOrder",
			wantIn: "CSR order",
			mutate: func() *csr.CSR[int64] {
				return csr.FromArrays(slices.Clone(want.vertices), slices.Clone(want.edges),
					slices.Clone(want.weights), want.order+1, want.size)
			},
		},
		{
			name:   "WrongSize",
			wantIn: "CSR size",
			mutate: func() *csr.CSR[int64] {
				return csr.FromArrays(slices.Clone(want.vertices), slices.Clone(want.edges),
					slices.Clone(want.weights), want.order, want.size+1)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := bulkOracleCheckCSR("sens", want, tc.mutate())
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the checker is blind to this dimension, "+
					"so every passing comparison along it proves nothing", tc.name)
			}
			joined := bulkOracleJoin(v)
			if !strings.Contains(joined, tc.wantIn) {
				t.Fatalf("perturbation %q fired on the wrong dimension.\nwant a message containing %q\ngot:\n%s",
					tc.name, tc.wantIn, joined)
			}
			t.Logf("%s -> %d violation(s); first: %s", tc.name, len(v), v[0].Message)
		})
	}
}

// TestBulkLoadOracle_Sensitivity proves every SCENARIO clause can fail.
//
// Each case engages one falsifiability seam, which makes one clause's
// precondition false — the cap is never crossed, the fault is never armed, the
// cancellation never happens, the fixture has no duplicate — and the run must go
// RED. A clause whose failure has never been observed is not a guard; it is
// decoration.
//
// The model-perturbation cases work the other way round: they leave the engine's
// output exactly as a passing run produces it and change only the model, so what
// is measured is the checker's power to see a difference on real engine output.
func TestBulkLoadOracle_Sensitivity(t *testing.T) {
	t.Parallel()
	sc := bulkLoadOracleScenario()

	for _, tc := range []struct {
		opts   func(*testing.T) bulkOracleOptions
		name   string
		wantIn string
	}{
		{
			name:   "TameFixtureKillsDedupCoverage",
			wantIn: "repeats no endpoint pair",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{tameFixture: true} },
		},
		{
			name:   "ParallelBuildFedADifferentStream",
			wantIn: "byte-identical",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{extraParallelEdge: true} },
		},
		{
			name:   "RowCapNeverCrossed",
			wantIn: "ErrTooManyRows",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{liftRowCap: true} },
		},
		{
			name:   "DrainNeverCancelled",
			wantIn: "cancelled mid-stream",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{skipDrainCancel: true} },
		},
		{
			name:   "FirstPublishFsyncFaultDisarmed",
			wantIn: "did not make the publish fail",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{disarmSyncFault: true} },
		},
		{
			name:   "TornImagePlantedAfterFailedRepublish",
			wantIn: "torn or partial write reached the live path",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{tornRepublish: true} },
		},
		{
			name:   "RenameFaultDisarmed",
			wantIn: "armed rename fault did not make the publish fail",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{disarmRenameFault: true} },
		},
		{
			name:   "CapacityBoundLifted",
			wantIn: "exact-capacity bound succeeded",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{liftDiskCapacity: true} },
		},
		{
			name:   "ParentDirFsyncFaultDisarmed",
			wantIn: "PRESENT and COMPLETE",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{disarmParentDirFault: true} },
		},
		{
			name:   "PublishedImageNeverDamaged",
			wantIn: "NOT refused by the reader",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{skipCorruption: true} },
		},
		{
			name:   "UnwritableOutputPathMadeWritable",
			wantIn: "unwritable OutputPath",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{writableBadPath: true} },
		},
		{
			name:   "CrashDifferentialCollapses",
			wantIn: "made no difference",
			opts:   func(*testing.T) bulkOracleOptions { return bulkOracleOptions{keepCrashDurability: true} },
		},
		{
			name:   "ModelLosesAnEdge",
			wantIn: "edge multiset",
			opts: func(t *testing.T) bulkOracleOptions {
				return bulkOracleOptions{perturb: func(m *bulkOracleModel) {
					id := bulkOracleTestRow(t, m, 1)
					m.rows[id] = m.rows[id][1:]
				}}
			},
		},
		{
			name:   "ModelGainsAPhantomEdge",
			wantIn: "edge multiset",
			opts: func(t *testing.T) bulkOracleOptions {
				return bulkOracleOptions{perturb: func(m *bulkOracleModel) {
					id := bulkOracleTestRow(t, m, 1)
					m.rows[id] = append(m.rows[id], bulkOracleEntry{dst: id, w: 987654321})
				}}
			},
		},
		{
			name:   "ModelMisweightsAnEdge",
			wantIn: "weight column",
			opts: func(t *testing.T) bulkOracleOptions {
				return bulkOracleOptions{perturb: func(m *bulkOracleModel) {
					id := bulkOracleTestRow(t, m, 1)
					m.rows[id][0].w = -m.rows[id][0].w - 1
				}}
			},
		},
		{
			name:   "ModelRetargetsAnEdge",
			wantIn: "edge multiset",
			opts: func(t *testing.T) bulkOracleOptions {
				return bulkOracleOptions{perturb: func(m *bulkOracleModel) {
					id := bulkOracleTestRow(t, m, 1)
					m.rows[id][0].dst++
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, report, err := runBulkLoadOracleWith(context.Background(), sc.DefaultSeed, tc.opts(t))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if report == nil {
				t.Fatalf("seam %q produced NO violation: the clause it disables cannot fail, so a passing "+
					"run proves nothing about that dimension", tc.name)
			}
			joined := bulkOracleJoin(report.Violations)
			if !strings.Contains(joined, tc.wantIn) {
				t.Fatalf("seam %q fired, but on the wrong dimension.\nwant a message containing %q\ngot:\n%s",
					tc.name, tc.wantIn, joined)
			}
			t.Logf("%s -> %d violation(s); first: %s", tc.name, len(report.Violations), report.Violations[0].Message)
		})
	}
}

// TestBulkLoadOracle_ForgedChecksumIsCaughtByContent is the POSITIVE control for
// the forbidden fourth outcome.
//
// It damages a published image and then repairs its CRC32C, producing the one
// state the format's checksum is designed to make unreachable: an image that is
// internally self-consistent, that the reader ACCEPTS, and that describes a
// different graph. The run must stay green — the harness built that image on
// purpose — while recording that the CONTENT comparison, not the checksum, is
// what noticed.
//
// Its value is what it rules out: it shows that "present and accepted implies
// equal to the model" does not rest on the CRC. A scenario whose only
// divergence-detector were the checksum would pass this and record nothing.
func TestBulkLoadOracle_ForgedChecksumIsCaughtByContent(t *testing.T) {
	t.Parallel()
	sc := bulkLoadOracleScenario()
	ev, report, err := runBulkLoadOracleWith(context.Background(), sc.DefaultSeed,
		bulkOracleOptions{repairForgedCRC: true})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("the forged-checksum control must not fail the run:\n%s", report)
	}
	if !ev.forgedCRCDetected {
		t.Error("an image whose payload was damaged and whose CRC32C was then repaired was accepted as " +
			"EQUAL to the model. The content oracle is resting on the checksum, so it cannot see a " +
			"silent divergence — and every 'present and complete' verdict in this scenario is unfalsifiable")
	}
}

// TestBulkLoadOracle_ParallelFanOutStillUnreachable is a documentation tripwire.
//
// The scenario's header states that `Loader.buildParallel` — the multi-goroutine
// fan-out — is unreachable from outside package `bulk`, because
// `csrDirectEligible()` is matched first and is unconditionally true for a
// directed load: it requires `MaxShardCapacity == 0`, and [bulk.Options] exposes
// no shard-capacity knob. That reasoning depends entirely on the field set of
// [bulk.Options].
//
// If a future revision adds such a knob, the fan-out becomes reachable, the
// byte-identity arm silently starts (or stops) covering it, and the header's
// unreachable-regimes section becomes a false claim in shipped documentation.
// This test fails at that moment rather than letting the doc rot.
func TestBulkLoadOracle_ParallelFanOutStillUnreachable(t *testing.T) {
	t.Parallel()
	ty := reflect.TypeOf(bulk.Options{})
	got := make([]string, 0, ty.NumField())
	for i := 0; i < ty.NumField(); i++ {
		got = append(got, ty.Field(i).Name)
	}
	sort.Strings(got)
	want := []string{"Directed", "ExpectNodes", "MaxRows", "Multigraph", "OutputPath", "Parallel"}
	if !slices.Equal(got, want) {
		t.Errorf("bulk.Options fields changed: got %v, want %v.\n\n"+
			"The bulk-load-oracle scenario documents that Loader.buildParallel's goroutine fan-out is "+
			"UNREACHABLE from an external package, because Finalise matches "+
			"`Parallel && csrDirectEligible()` first and csrDirectEligible() requires "+
			"MaxShardCapacity == 0 — which no public Option can change. If a shard-capacity knob was "+
			"just added, that claim is now FALSE and the scenario's header must be corrected (and the "+
			"fan-out covered). If the field set merely changed for another reason, update this list.",
			got, want)
	}
}

// bulkOracleTestRow returns the lowest NodeID whose model row holds at least
// minLen entries. Walking the ids in ascending order keeps the choice
// deterministic, so a perturbation is reproducible rather than dependent on map
// iteration order.
func bulkOracleTestRow(t *testing.T, m *bulkOracleModel, minLen int) graph.NodeID {
	t.Helper()
	maxID := uint64(m.mapper.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		if len(m.rows[graph.NodeID(id)]) >= minLen {
			return graph.NodeID(id)
		}
	}
	t.Fatalf("no model row holds %d entries", minLen)
	return 0
}

// bulkOracleJoin concatenates violation messages for a substring assertion.
func bulkOracleJoin(v []Violation) string {
	parts := make([]string, 0, len(v))
	for i := range v {
		parts = append(parts, v[i].String())
	}
	return strings.Join(parts, "\n")
}
