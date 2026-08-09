package isolationtest_test

// blocking_test.go — acceptance criterion 2 (rmp #2340): a blocked step is
// detected and reported within a bounded timeout rather than hanging.
//
// # Why a Go hook and not a Cypher statement
//
// Under MVCC-only concurrency control there is almost nothing for a GoGraph step
// to block ON. An ordinary read acquires nothing; a write acquires nothing; a
// write-write collision is REFUSED rather than queued; and the one genuinely
// exclusive hold — the schema gate a DDL takes — is released when that DDL's own
// statement returns. Because this harness awaits each step before launching the
// next, whatever a step might have waited for has already been released by the
// time the next step starts.
//
// That is a fact about the engine, and a good one: it is exactly why the
// exhaustive enumeration is tractable here where PostgreSQL's README has to warn
// that a spec using blocking "must manually specify valid permutations". It is
// ALSO why the blocking machinery cannot be exercised by any Cypher spec, and a
// mechanism that is never exercised is a mechanism nobody knows works — which is
// how the harness would come to hang one day on a future DDL scenario.
//
// So the block is manufactured with a [Step.Hook] rendezvous: a step that
// genuinely does not return until another session's step releases it. What is
// under test here is the HARNESS, so a synthetic block is the honest instrument,
// not a shortcut.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// gateSpec builds a two-session spec in which s1's step blocks on a channel that
// s2's step closes, under the given explicit permutation.
func gateSpec(name string, perm []string) (*isolationtest.Spec, chan struct{}) {
	release := make(chan struct{})
	return &isolationtest.Spec{
		Name: name,
		Sessions: []*isolationtest.Session{
			{
				Name: "s1",
				Steps: []isolationtest.Step{{
					Name:  "hold",
					Label: "<block until released>",
					Hook: func(ctx context.Context) error {
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				}},
			},
			{
				Name: "s2",
				Steps: []isolationtest.Step{
					{Name: "poke", Label: "<release>", Hook: func(context.Context) error {
						close(release)
						return nil
					}},
					{Name: "after", Query: "RETURN 1 AS ok"},
				},
			},
		},
		Permutations: [][]string{perm},
	}, release
}

// TestBlockedStepIsReportedNotHung is the criterion proper: a step that has not
// returned within the block timeout must be REPORTED as waiting, the run must
// continue, and the step's later completion must be reported too.
//
// The test's own deadline is what proves "not hung": the whole run is given far
// less time than a hang would take, so an implementation that waited for the
// blocked step would fail here rather than time out the package.
func TestBlockedStepIsReportedNotHung(t *testing.T) {
	t.Parallel()
	spec, _ := gateSpec("blocking-detected", []string{"hold", "poke", "after"})
	r := &isolationtest.Runner{
		NewEngine:          memEngine,
		BlockTimeout:       150 * time.Millisecond,
		PermutationTimeout: 10 * time.Second,
	}

	done := make(chan string, 1)
	go func() { done <- runToStringNoFatal(t, spec, r) }()

	var transcript string
	select {
	case transcript = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner HUNG on a blocked step; blocking detection is not bounded")
	}

	if !strings.Contains(transcript, "step hold: <block until released> <waiting ...>") {
		t.Errorf("blocked step was not reported as waiting:\n%s", transcript)
	}
	if !strings.Contains(transcript, "step hold: <... completed>") {
		t.Errorf("released step's completion was not reported:\n%s", transcript)
	}
	// Ordering is the substantive claim: `waiting` must precede `poke`, and the
	// completion must follow it. Anything else would mean the harness reported a
	// block it had not actually observed.
	iWait := strings.Index(transcript, "step hold: <block until released> <waiting ...>")
	iPoke := strings.Index(transcript, "step poke:")
	iDone := strings.Index(transcript, "step hold: <... completed>")
	if iWait >= iPoke || iPoke >= iDone {
		t.Errorf("transcript order is wrong: waiting@%d poke@%d completed@%d\n%s", iWait, iPoke, iDone, transcript)
	}
}

// TestBlockedSessionCannotBeHandedItsNextStep pins the invalid-permutation
// guard. PostgreSQL calls an interleaving that hands a blocked session another
// step invalid, and cancels it after a timeout; this harness reports it
// immediately and by name, which is strictly better because it is deterministic
// and it says WHICH step is stuck.
func TestBlockedSessionCannotBeHandedItsNextStep(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release) // let the parked goroutine exit when the test ends
	spec := &isolationtest.Spec{
		Name: "blocking-invalid",
		Sessions: []*isolationtest.Session{
			{
				Name: "s1",
				Steps: []isolationtest.Step{
					{Name: "hold", Label: "<block forever>", Hook: func(ctx context.Context) error {
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}},
					{Name: "next", Query: "RETURN 1 AS ok"},
				},
			},
		},
		Permutations: [][]string{{"hold", "next"}},
	}
	r := &isolationtest.Runner{
		NewEngine:          memEngine,
		BlockTimeout:       100 * time.Millisecond,
		PermutationTimeout: 10 * time.Second,
	}
	transcript := runToStringNoFatal(t, spec, r)
	if !strings.Contains(transcript, "INVALID PERMUTATION") {
		t.Errorf("handing a blocked session its next step was not reported as invalid:\n%s", transcript)
	}
	if !strings.Contains(transcript, "still blocked on hold") {
		t.Errorf("the invalid-permutation report does not name the stuck step:\n%s", transcript)
	}
}

// TestNoGoGraphStepBlocks records, as an executable assertion rather than as a
// comment, the engine property this file's header rests on: across every
// interleaving of every shipped spec, no Cypher step is ever observed to block.
//
// It is the counterpart of the synthetic tests above. They prove the harness can
// see a block; this proves GoGraph does not produce one — so if a future change
// introduces a wait on the write path, THIS test is what says so, and says it in
// the one place a reader would look for the claim.
func TestNoGoGraphStepBlocks(t *testing.T) {
	t.Parallel()
	for _, s := range []*isolationtest.Spec{lostUpdateSpec(), writeSkewSpec(), bankTransferSpec()} {
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			transcript := runToString(t, s, memRunner())
			if strings.Contains(transcript, "<waiting ...>") {
				t.Errorf("spec %s has a step that blocked; GoGraph's concurrency control is MVCC alone and "+
					"nothing on these paths should wait. Transcript:\n%s", s.Name, transcript)
			}
			if strings.Contains(transcript, "INVALID PERMUTATION") || strings.Contains(transcript, "NEVER COMPLETED") {
				t.Errorf("spec %s produced an unrunnable interleaving:\n%s", s.Name, transcript)
			}
		})
	}
}

// runToStringNoFatal is [runToString] for tests that must inspect the transcript
// even when the run reported an error, and that may be called off the test
// goroutine (where t.Fatal is not allowed).
func runToStringNoFatal(t *testing.T, s *isolationtest.Spec, r *isolationtest.Runner) string {
	t.Helper()
	var b strings.Builder
	if err := r.Run(context.Background(), s, &b); err != nil {
		b.WriteString("RUN ERROR: " + err.Error() + "\n")
	}
	return b.String()
}
