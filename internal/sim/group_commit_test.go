package sim

// group_commit_test.go — the wiring and the falsifiability proofs for the WAL
// group-commit oracle (rmp #2471).
//
// None of these tests calls t.Parallel: the coalescing arm and the fail-all arm
// both install the GLOBAL metrics sink (see [RunGroupCommitCoalescing]), so they
// must not run beside other metrics-emitting work.
//
// Every gate here is proved falsifiable as well as satisfied. A gate that only
// ever passes is indistinguishable from a gate that cannot fail, and this sprint
// has already found seven guards that existed and proved nothing.

import (
	"context"
	"strings"
	"testing"
)

// TestGroupCommit_CoalescingIsCovered is the coverage-precondition gate of part
// (a): under concurrent committers the WAL must coalesce at least one commit
// onto another's fsync. The non-vacuity gate runs FIRST and separately, so a run
// that simply failed to commit is reported as uninformative rather than as a
// regression in the writer.
func TestGroupCommit_CoalescingIsCovered(t *testing.T) {
	ev, err := RunGroupCommitCoalescing(context.Background(), GroupCommitConfig{Seed: 0x9E3779B97F4A7C15})
	if err != nil {
		t.Fatalf("RunGroupCommitCoalescing: %v", err)
	}
	t.Log(ev)

	if v := checkGroupCommitNonVacuity(ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the run proved nothing about coalescing; the coverage verdict below would be meaningless")
	}
	for _, viol := range checkGroupCommitCoverage(ev) {
		t.Errorf("coverage: %s", viol)
	}
}

// TestGroupCommit_SoloCommitterHasNoFollowers is the SENSITIVITY proof for the
// coalescing gate, and the seam is the honest one: a single committer cannot
// coalesce with anybody, so the identical workload through the identical stack
// must drive the follower count to zero and the gate must fire.
//
// It doubles as the standing verification that store.wal.SyncGroup.coalesced is
// a FOLLOWER counter in this workload. The counter is shared with
// wal.Writer.SyncBuffered's durable-already fast path (an empty commit takes
// it), so if that path ever started firing here the count would drift above zero
// with nothing coalescing, and a `> 0` gate would silently become unfalsifiable.
// This test fails the moment that happens.
func TestGroupCommit_SoloCommitterHasNoFollowers(t *testing.T) {
	ev, err := RunGroupCommitCoalescing(context.Background(), GroupCommitConfig{
		Seed:        0x9E3779B97F4A7C15,
		Connections: 1,
		OpsPerConn:  groupCommitDefaultOps,
	})
	if err != nil {
		t.Fatalf("RunGroupCommitCoalescing: %v", err)
	}
	t.Log(ev)

	if ev.Acked == 0 {
		t.Fatal("the control run committed nothing, so it cannot show the absence of followers")
	}
	if ev.Coalesced != 0 {
		t.Errorf("a single committer recorded %d follower(s) on %s: the coalesced counter is not a pure follower counter "+
			"in this workload, so the `> 0` coverage gate is satisfiable without any coalescing", ev.Coalesced, metricSyncGroupCoalesced)
	}
	v := checkGroupCommitCoverage(ev)
	if len(v) == 0 {
		t.Fatal("the coverage gate passed a run with no coalescing at all: it cannot fail, and therefore proves nothing")
	}
	if msg := v[0].String(); !strings.Contains(msg, "follower fast path") {
		t.Errorf("the coverage violation does not explain what regressed:\n%s", msg)
	}
}

// TestGroupCommit_NonVacuityGateIsSeparate proves the two gates are genuinely
// independent, which is the point of keeping them apart (rmp #2470): a trivial
// population must be reported by the NON-VACUITY gate, and must not be dressed
// up as a coalescing regression by the coverage gate.
func TestGroupCommit_NonVacuityGateIsSeparate(t *testing.T) {
	// A population that is trivial in all three respects, but whose (tiny) round
	// count did include a follower.
	trivial := GroupCommitEvidence{Committers: 2, Coalesced: 1, Leaders: 1, Acked: 0}

	v := checkGroupCommitNonVacuity(trivial)
	if len(v) != 3 {
		t.Fatalf("non-vacuity reported %d violation(s); want 3 (committers, rounds, acked):\n%v", len(v), v)
	}
	for _, want := range []string{"concurrent committer", "SyncGroup round", "acknowledged"} {
		found := false
		for _, viol := range v {
			if strings.Contains(viol.Message, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no non-vacuity violation mentions %q:\n%v", want, v)
		}
	}
	if cv := checkGroupCommitCoverage(trivial); len(cv) != 0 {
		t.Errorf("the coverage gate also fired on a merely-uninformative run, so an uninformative run reads as a faulty one:\n%v", cv)
	}

	// And the converse: an ample population with zero coalescing is NOT a
	// non-vacuity problem — it is exactly the regression the coverage gate owns.
	ample := GroupCommitEvidence{Committers: groupCommitMinCommitters, Coalesced: 0, Leaders: groupCommitMinRounds, Acked: 1}
	if v := checkGroupCommitNonVacuity(ample); len(v) != 0 {
		t.Errorf("non-vacuity fired on an ample population:\n%v", v)
	}
	if v := checkGroupCommitCoverage(ample); len(v) != 1 {
		t.Errorf("the coverage gate did not own the zero-coalescing case: %d violation(s)", len(v))
	}
}

// TestGroupCommit_FailAll is part (b): a genuine multi-member commit group whose
// shared fsync fails. Every member must receive the durability fail-stop, none
// may be acknowledged, and recovery must keep the previously acknowledged commit
// while discarding the whole failed group.
func TestGroupCommit_FailAll(t *testing.T) {
	res, err := RunGroupCommitFailAll(context.Background(), 0xD4B1EC0117)
	if err != nil {
		t.Fatalf("RunGroupCommitFailAll: %v", err)
	}
	t.Log(res)

	for _, viol := range checkGroupCommitFailAll(&res) {
		t.Errorf("%s", viol)
	}
}

// TestGroupCommit_FailAllArmDetectsAWronglyAckedMember is the SENSITIVITY proof
// for the fail-all arm. The adjudicator is a pure function of the observed
// result, so the failure it exists to catch can be injected directly instead of
// being hoped for: a member that was acknowledged although the group's shared
// fsync failed is precisely the durability violation fail-all prevents.
func TestGroupCommit_FailAllArmDetectsAWronglyAckedMember(t *testing.T) {
	// The shape of a healthy run, taken from the arm's own contract.
	healthy := GroupCommitFailAllResult{
		Members: groupCommitFailAllMembers, Acked: 0,
		Failed: groupCommitFailAllMembers, DurabilityClass: groupCommitFailAllMembers,
		Leaders: 0, PoisonedRounds: 1, FsyncAttempts: groupCommitFailAllFsyncs, GateFired: true,
		PriorDurable: 1, GroupDurable: 0,
	}
	if v := checkGroupCommitFailAll(&healthy); len(v) != 0 {
		t.Fatalf("the adjudicator rejected a healthy result, so its clauses do not describe the contract:\n%v", v)
	}

	// (i) One member wrongly acknowledged.
	wronglyAcked := healthy
	wronglyAcked.Acked = 1
	wronglyAcked.Failed = groupCommitFailAllMembers - 1
	wronglyAcked.DurabilityClass = groupCommitFailAllMembers - 1
	v := checkGroupCommitFailAll(&wronglyAcked)
	if len(v) == 0 {
		t.Fatal("a member acknowledged by a FAILED group fsync passed: the fail-all arm cannot fail and proves nothing")
	}
	if !hasViolationKind(v, ViolationACIDDurability) {
		t.Errorf("a wrongly-acked member was not reported as a DURABILITY violation:\n%v", v)
	}

	// (ii) The failed group survived recovery — uncommitted state leaking in.
	resurrected := healthy
	resurrected.GroupDurable = groupCommitFailAllMembers
	if v := checkGroupCommitFailAll(&resurrected); !hasViolationKind(v, ViolationACIDAtomicity) {
		t.Errorf("a resurrected failed group was not reported as an ATOMICITY violation:\n%v", v)
	}

	// (iii) The members did not actually share one fsync, so the run tested the
	// sticky poison rather than group fail-all. This is the clause that separates
	// this arm from the pre-existing store/wal unit test, which fails EVERY fsync
	// and so cannot tell one shared round from N serialised ones.
	notOneGroup := healthy
	notOneGroup.PoisonedRounds = groupCommitFailAllMembers
	notOneGroup.FsyncAttempts = 2 * groupCommitFailAllMembers
	if v := checkGroupCommitFailAll(&notOneGroup); len(v) == 0 {
		t.Error("a run in which every member paid its own fsync passed as a group fail-all")
	}

	// (iv) The gate never fired, so no group was constructed at all.
	noGate := healthy
	noGate.GateFired = false
	if v := checkGroupCommitFailAll(&noGate); len(v) == 0 {
		t.Error("a run whose rendezvous never fired passed: an unconstructed group cannot demonstrate fail-all")
	}

	// (v) The prior acknowledged commit was destroyed by the poison.
	priorLost := healthy
	priorLost.PriorDurable = 0
	if v := checkGroupCommitFailAll(&priorLost); !hasViolationKind(v, ViolationACIDDurability) {
		t.Errorf("losing a previously acknowledged commit was not reported as a DURABILITY violation:\n%v", v)
	}
}

// hasViolationKind reports whether any violation carries the given kind.
func hasViolationKind(vs []Violation, kind ViolationKind) bool {
	for _, v := range vs {
		if v.Kind == kind {
			return true
		}
	}
	return false
}
