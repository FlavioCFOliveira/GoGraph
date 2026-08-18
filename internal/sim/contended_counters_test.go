package sim

// contended_counters_test.go — the rmp #2552 regression battery for the
// production profile's contended-counter adjudication.
//
// The adjudication compares two slices sized by DIFFERENT predicates: the
// expected tally is sized by the profile's configured counter count, while the
// measured finals were sized by whether the seeded role draw happened to assign
// a contended writer (and by whether any transaction was issued at all). A
// cycle that drew none produced an EMPTY final set, and the comparison — bounded
// by the tally, which is never empty — indexed past its end. The panic killed
// the PROCESS, not merely the run: the 2026-08-18 parallel swarm exercise lost
// an entire 30-minute instance to it, along with its summary, its coverage
// report, and every run it had left to do.
//
// Every precondition below is CONSTRUCTED — a mix with zero contended weight, a
// result value carrying no counter evidence — never drawn from a seed that
// supplies it roughly one run in five hundred. The one end-to-end test that does
// use the recorded seed asserts its precondition from the RUN's own population
// evidence, so a change to the mix or to the draw order fails it loudly instead
// of quietly turning it into a test of nothing.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// productionProfileNoContendedSeed is the seed the 2026-08-18 swarm exercise
// crashed on: its cycle-0 role draw assigns ZERO contended writers out of 24
// connections, so the cycle reaches the counter adjudication having never
// touched a shared counter.
const productionProfileNoContendedSeed uint64 = 17477768168859964485

// TestRunConcurrent_CounterEvidenceWellFormedWithoutContendedWriters is the
// root-cause gate: whatever the role draw produces, [RunConcurrent] reports as
// many counter finals as it reports acknowledged tallies, and as many as the
// caller configured. A counter no connection touched reads 0 — it is never
// ABSENT, because absent is what the caller cannot index.
//
// Both routes to an empty final set are driven directly, by mix rather than by
// seed: a population with transactional traffic but no contended role, and a
// population with no transactional role at all.
func TestRunConcurrent_CounterEvidenceWellFormedWithoutContendedWriters(t *testing.T) {
	defer goleak.VerifyNone(t)

	const counters = 3
	cases := []struct {
		name string
		mix  *ConcurrentMix
		// wantTxIssued asserts the case really is the one it claims to be:
		// transactional traffic present (the haveContended route) or absent
		// entirely (the TxIssued==0 route).
		wantTxIssued bool
	}{
		{
			name:         "transactions issued but no connection drew the contended role",
			mix:          &ConcurrentMix{TxWriterWeight: 0.8, ReaderWeight: 0.2},
			wantTxIssued: true,
		},
		{
			name:         "no transactional role at all, so no transaction is issued",
			mix:          &ConcurrentMix{WriterWeight: 0.7, ReaderWeight: 0.3},
			wantTxIssued: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := NewSimServer(SimEngineForServer(), clock.Real())
			if err != nil {
				t.Fatalf("NewSimServer: %v", err)
			}
			defer func() { _ = srv.Close() }()

			res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
				Seed:              0x2552,
				Connections:       8,
				OpsPerConn:        4,
				ContendedCounters: counters,
				Mix:               tc.mix,
			})
			if err != nil {
				t.Fatalf("RunConcurrent: %v", err)
			}

			// The constructed precondition, verified rather than assumed.
			if res.ContendedConnections != 0 {
				t.Fatalf("%d connections drew the contended role — the mix was meant to exclude it,"+
					" so this run does not exercise the case at all", res.ContendedConnections)
			}
			if issued := res.TxIssued > 0; issued != tc.wantTxIssued {
				t.Fatalf("txIssued=%d (issued=%t), want issued=%t — this run is not the route it claims",
					res.TxIssued, issued, tc.wantTxIssued)
			}

			if len(res.ContendedAcked) != counters {
				t.Errorf("len(ContendedAcked)=%d, want %d", len(res.ContendedAcked), counters)
			}
			if len(res.ContendedFinal) != counters {
				t.Errorf("len(ContendedFinal)=%d, want %d — a caller comparing acked against final"+
					" indexes both by the same k and would run off the end", len(res.ContendedFinal), counters)
			}
			for k := range res.ContendedFinal {
				if res.ContendedFinal[k] != 0 {
					t.Errorf("counter %d final=%d, want 0: no connection ever incremented it",
						k, res.ContendedFinal[k])
				}
			}
		})
	}
}

// TestFoldContendedCounters covers the adjudication itself against constructed
// results, including the two dimensions that matter most: it must still FIRE on
// a genuinely lost increment (the oracle was not weakened to stop the panic),
// and it must report missing counter evidence as a typed violation rather than
// panicking on it or, worse, silently skipping the comparison.
func TestFoldContendedCounters(t *testing.T) {
	cases := []struct {
		name string
		// expected is the run-wide tally BEFORE this cycle is folded in.
		expected   []int64
		acked      []int64
		final      []int64
		wantExpect []int64
		// wantOps and wantMsgs describe the violations in order; a nil wantOps
		// asserts silence.
		wantOps  []string
		wantMsgs []string
	}{
		{
			name:       "every counter holds exactly its accumulated acknowledged increments",
			expected:   []int64{4, 0},
			acked:      []int64{3, 2},
			final:      []int64{7, 2},
			wantExpect: []int64{7, 2},
		},
		{
			name:       "a counter lost an acknowledged increment",
			expected:   []int64{0, 0},
			acked:      []int64{5, 3},
			final:      []int64{4, 3},
			wantExpect: []int64{5, 3},
			wantOps:    []string{"lost update"},
			wantMsgs:   []string{"counter 0 final=4, accumulated acked=5"},
		},
		{
			name:       "a counter gained an increment nobody acknowledged",
			expected:   []int64{0, 0},
			acked:      []int64{2, 2},
			final:      []int64{2, 3},
			wantExpect: []int64{2, 2},
			wantOps:    []string{"lost update"},
			wantMsgs:   []string{"counter 1 final=3, accumulated acked=2"},
		},
		{
			name:       "an increment lost across the accumulated total of an earlier cycle",
			expected:   []int64{6, 1},
			acked:      []int64{0, 0},
			final:      []int64{6, 0},
			wantExpect: []int64{6, 1},
			wantOps:    []string{"lost update"},
			wantMsgs:   []string{"counter 1 final=0, accumulated acked=1"},
		},
		{
			name:       "no counter evidence at all: every counter is reported unadjudicated, none is skipped",
			expected:   []int64{0, 3},
			acked:      []int64{0, 0},
			final:      nil,
			wantExpect: []int64{0, 3},
			wantOps:    []string{"contended counter evidence", "contended counter evidence"},
			wantMsgs: []string{
				"counter 0 was never read at quiescence",
				"counter 1 was never read at quiescence",
			},
		},
		{
			name:       "partial counter evidence: the covered counter is still compared, the rest reported",
			expected:   []int64{0, 0},
			acked:      []int64{1, 4},
			final:      []int64{9},
			wantExpect: []int64{1, 4},
			wantOps:    []string{"lost update", "contended counter evidence"},
			wantMsgs: []string{
				"counter 0 final=9, accumulated acked=1",
				"counter 1 was never read at quiescence",
			},
		},
		{
			name:       "more acknowledged tallies than the profile tracks",
			expected:   []int64{0},
			acked:      []int64{2, 2},
			final:      []int64{2, 2},
			wantExpect: []int64{2},
			wantOps:    []string{"contended counter evidence"},
			wantMsgs:   []string{"reported 2 acknowledged counter tallies, the profile tracks 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := ConcurrentResult{ContendedAcked: tc.acked, ContendedFinal: tc.final}
			got := foldContendedCounters(7, tc.expected, &res)

			if len(tc.expected) != len(tc.wantExpect) {
				t.Fatalf("malformed case: expected and wantExpect differ in length")
			}
			for k := range tc.wantExpect {
				if tc.expected[k] != tc.wantExpect[k] {
					t.Errorf("after the fold, expected[%d]=%d, want %d", k, tc.expected[k], tc.wantExpect[k])
				}
			}
			if len(got) != len(tc.wantOps) {
				t.Fatalf("got %d violations, want %d: %v", len(got), len(tc.wantOps), got)
			}
			for i, v := range got {
				if v.Kind != ViolationACIDDurability {
					t.Errorf("violation %d kind=%q, want %q", i, v.Kind, ViolationACIDDurability)
				}
				if v.Op != tc.wantOps[i] {
					t.Errorf("violation %d op=%q, want %q", i, v.Op, tc.wantOps[i])
				}
				if !strings.Contains(v.Message, tc.wantMsgs[i]) {
					t.Errorf("violation %d message %q does not contain %q", i, v.Message, tc.wantMsgs[i])
				}
				if !strings.Contains(v.Message, "cycle 7") {
					t.Errorf("violation %d message %q does not name the cycle", i, v.Message)
				}
			}
		})
	}
}

// TestProductionProfile_SurvivesACycleWithNoContendedWriter is the end-to-end
// reproduction: the exact seed the swarm exercise crashed on, run through the
// whole profile.
//
// Its precondition — a cycle whose seeded draw assigned no contended writer at
// all — is asserted from the run's OWN population evidence, not from a replay of
// the draw here, so it cannot quietly stop covering the case if the mix or the
// draw order changes.
func TestProductionProfile_SurvivesACycleWithNoContendedWriter(t *testing.T) {
	defer goleak.VerifyNone(t)

	report, ev, err := runProductionProfileEvidence(
		context.Background(), productionProfileNoContendedSeed, shortProductionProfile())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var withoutContended int
	for _, c := range ev.cycles {
		t.Logf("cycle %d: %d of %d connections drew the contended-writer role",
			c.cycle, c.contendedConnections, shortProductionProfile().connections)
		if c.contendedConnections == 0 {
			withoutContended++
		}
	}
	if withoutContended == 0 {
		t.Fatalf("seed %d no longer produces a cycle without a contended writer, so this test"+
			" no longer reproduces anything: pick a seed that does, or delete it",
			productionProfileNoContendedSeed)
	}
	if report != nil {
		t.Fatalf("production profile failed:\n%s", report.String())
	}
}
