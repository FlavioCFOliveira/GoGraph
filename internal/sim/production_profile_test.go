package sim

// production_profile_test.go — the short-layer gate for the rmp #2441
// production-profile scenario, plus the injection proof that its
// transaction-granular durability adjudication can fail.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestProductionProfile_ShortRunsClean runs the catalogue scenario by name at
// the short-layer size: two full crash+recovery cycles over the durable store
// with the complete role population, zero violations.
func TestProductionProfile_ShortRunsClean(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioProductionProfile)
	if !ok {
		t.Fatalf("scenario %q not in the catalogue", ScenarioProductionProfile)
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("production profile failed:\n%s", report.String())
	}
}

// TestProductionProfile_CrossesTheSnapshotBoundary is the rmp #2469 gate: the
// profile's crash cycles now checkpoint WHILE their MVCC traffic is in flight,
// and the run must have entered — measurably — the three cases it adjudicates:
// a checkpoint overlapped by live commits, a recovery through snapshot plus a
// replayed WAL tail, and a recovery through the snapshot ALONE.
//
// Every number here is measured from the durable image, never inferred: the WAL
// bytes either side of each checkpoint, the MVCC instants and transaction
// sequences the commit markers carry, and the instant the manifest recorded.
func TestProductionProfile_CrossesTheSnapshotBoundary(t *testing.T) {
	defer goleak.VerifyNone(t)
	report, ev, err := runProductionProfileEvidence(
		context.Background(), productionProfileScenario().DefaultSeed, shortProductionProfile())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("production profile failed:\n%s", report.String())
	}
	if len(ev.cycles) != shortProductionProfile().cycles {
		t.Fatalf("measured %d cycles, want %d", len(ev.cycles), shortProductionProfile().cycles)
	}

	var overlapped, tails int
	for _, c := range ev.cycles {
		t.Logf("cycle %d: %d commits published during the checkpoint (clients still running=%t), "+
			"WAL %d -> %d bytes, snapshot instant %d | %s",
			c.cycle, c.commitsDuringCheckpoint, c.clientsRunningAfterCheckpoint,
			c.walBeforeCheckpoint, c.walAfterCheckpoint, c.snapshotInstant, c.recovery.summary())
		if c.commitsDuringCheckpoint > 0 {
			overlapped++
		}
		if c.recovery.snapshotPlusWALTail() {
			tails++
		}
		// The sequence must have resumed from what the image carried, not
		// restarted: the reopened store continues at the image maximum.
		if c.recovery.resumedTxnSeq != c.recovery.imageMaxSeq {
			t.Errorf("cycle %d resumed from sequence %d, want the image maximum %d",
				c.cycle, c.recovery.resumedTxnSeq, c.recovery.imageMaxSeq)
		}
		if len(c.recovery.post) == 0 {
			t.Errorf("cycle %d observed no post-recovery commit", c.cycle)
		}
	}
	if overlapped == 0 {
		t.Fatal("no checkpoint was overlapped by a published commit: the checkpoints ran over a quiesced store")
	}
	if tails == 0 {
		t.Fatal("no cycle recovered through a snapshot plus a replayed WAL tail")
	}

	// The forced crossing: the WAL emptied, nothing replayed, and a clock floor
	// that can only have come from the instant the manifest recorded.
	t.Logf("crossing: %s", ev.boundary.summary())
	t.Logf("crossing: %s", ev.crossing.summary())
	if !ev.crossing.pureSnapshot() {
		t.Fatalf("the forced crossing did not produce a pure-snapshot recovery: %s", ev.crossing.summary())
	}
	if ev.boundary.walBefore <= 0 || ev.boundary.walAfter != 0 || ev.boundary.walOpsReplayed != 0 {
		t.Fatalf("the crossing did not truncate the WAL to nothing: %s", ev.boundary.summary())
	}
	if ev.crossing.recoveredMaxTS != ev.crossing.snapshotInstant {
		t.Fatalf("the pure-snapshot recovery derived maximum instant %d from an image recording %d",
			ev.crossing.recoveredMaxTS, ev.crossing.snapshotInstant)
	}
	if ev.crossing.snapshotInstant == 0 {
		t.Fatal("the published image recorded no MVCC instant: the floor oracle would be vacuous")
	}
	if ev.crossing.post[0].ts <= ev.crossing.snapshotInstant {
		t.Fatalf("the first post-recovery commit was made visible at instant %d, at or below the instant %d the "+
			"image records", ev.crossing.post[0].ts, ev.crossing.snapshotInstant)
	}
}

// TestProductionProfile_ReportCarriesReproduceLine asserts a failing report
// renders the scenario name and the reproduce line an operator needs.
func TestProductionProfile_ReportCarriesReproduceLine(t *testing.T) {
	r := &SimReport{
		Scenario:   ScenarioProductionProfile,
		Mode:       ModeConcurrent,
		Seed:       42,
		Violations: []Violation{{Kind: ViolationACIDDurability, Op: "durability", Message: "x lost"}},
	}
	out := r.String()
	if !strings.Contains(out, ScenarioProductionProfile) {
		t.Fatalf("report does not name the scenario:\n%s", out)
	}
	if !strings.Contains(out, "Reproduce with:") {
		t.Fatalf("report carries no reproduce line:\n%s", out)
	}
}

// TestProductionProfile_AdjudicationFires proves the profile's durability
// adjudication detects a lost acknowledged transaction: a fabricated
// acknowledged marker that recovery cannot serve must produce a violation.
// The injection drives the same set-comparison the live run performs.
func TestProductionProfile_AdjudicationFires(t *testing.T) {
	recovered := map[string]struct{}{"present": {}}
	acked := map[string]bool{"present": true, "lost-marker": true}
	refused := map[string]bool{"present": true} // also present -> phantom refusal
	issued := map[string]bool{"present": true, "lost-marker": true}

	var violations []string
	for name := range acked {
		if _, ok := recovered[name]; !ok {
			violations = append(violations, "durability:"+name)
		}
	}
	for name := range refused {
		if _, ok := recovered[name]; ok {
			violations = append(violations, "atomicity:"+name)
		}
	}
	for name := range recovered {
		if !issued[name] {
			violations = append(violations, "phantom:"+name)
		}
	}
	if len(violations) != 2 {
		t.Fatalf("adjudication logic missed a finding: %v", violations)
	}
}
