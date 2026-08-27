package main

import (
	"math/rand"
	"sort"
	"testing"
)

// torn_delta_attribution_test.go — why the 2026-08-06 torn-total delta cannot be
// attributed arithmetically (rmp #2336).
//
// The sighting reported one reader observing a total 941758 low. rmp #2333
// established that this is NOT one transfer (no planned amount equals it) and
// proposed the remaining shape: "one account resolved at a different instant from
// the rest, whose delta would be that account's net flow".
//
// That proposal is TESTABLE, and this file tests it — with the power check that
// makes the answer mean something. The conclusion is that the delta carries NO
// attribution: it is as consistent with that mechanism as almost any other value
// would be, so no amount of arithmetic on the total can classify the sighting.
// This is the quantitative form of the task's own instruction not to re-argue
// from the total alone.

// tornSightingDelta is how far below the expected total the 2026-08-06 reader's
// sum(a.balance) was: expected 46625986168, observed 46625044410.
const tornSightingDelta = int64(941758)

// sightingConfig rebuilds the exact configuration of the run that produced the
// sighting: TestRunReproducibleAcrossReaderScaling's shape, default seed. Its
// initialTotal is asserted against the sighting's expected total, so this file
// cannot silently drift onto a different workload.
func sightingConfig() config {
	cfg := defaultConfig()
	cfg.writers = 3
	cfg.readers = 7
	cfg.opsPerWriter = 200
	cfg.sweepOps = 0
	return cfg
}

// accountRunsByWriter returns, per account, each writer's ordered list of signed
// amounts touching that account (negative when debited).
//
// The per-writer split is what makes the test EXACT rather than a guess about
// scheduling. A contiguous block of one account's applied transfers — which is
// what "resolved at a different instant" means — must, restricted to any single
// writer, be a contiguous run of that writer's transfers on that account, because
// a writer commits its own transfers in a fixed order and no interleaving can
// reorder them. So the set of achievable net flows is exactly the set of sums of
// one contiguous run per writer.
func accountRunsByWriter(p *plan, writers int) map[int][][]int64 {
	type ev struct {
		g int
		a int64
	}
	evs := make(map[int][]ev)
	for w, ws := range p.byWriter {
		for k, tr := range ws {
			g := k*writers + w // transfer t was assigned to writer t%writers
			evs[tr.from] = append(evs[tr.from], ev{g, -tr.amount})
			evs[tr.to] = append(evs[tr.to], ev{g, tr.amount})
		}
	}
	out := make(map[int][][]int64, len(evs))
	for acct, list := range evs {
		sort.Slice(list, func(i, j int) bool { return list[i].g < list[j].g })
		byW := make([][]int64, writers)
		for _, e := range list {
			byW[e.g%writers] = append(byW[e.g%writers], e.a)
		}
		out[acct] = byW
	}
	return out
}

// contiguousRunSums returns every contiguous-run sum of seq, including the empty
// run (0), which is how a writer contributes nothing to the block.
func contiguousRunSums(seq []int64) map[int64]struct{} {
	s := map[int64]struct{}{0: {}}
	for i := range seq {
		var acc int64
		for j := i; j < len(seq); j++ {
			acc += seq[j]
			s[acc] = struct{}{}
		}
	}
	return s
}

// achievableNetFlows returns every net flow the account could show across some
// pair of instants, under any valid interleaving of the writers: the sumset of
// one contiguous run per writer.
//
// Built ONCE per account and then queried, because the null distribution below
// asks the same question of hundreds of targets. Rebuilding it per target cost
// 311s; building it per account costs a fraction of a second.
func achievableNetFlows(byWriter [][]int64) map[int64]struct{} {
	acc := map[int64]struct{}{0: {}}
	for _, seq := range byWriter {
		s := contiguousRunSums(seq)
		next := make(map[int64]struct{}, len(acc)*len(s)/2+1)
		for a := range acc {
			for b := range s {
				next[a+b] = struct{}{}
			}
		}
		acc = next
	}
	return acc
}

func countAccountsProducing(sets []map[int64]struct{}, target int64) int {
	n := 0
	for _, s := range sets {
		if _, ok := s[target]; ok {
			n++
		}
	}
	return n
}

// TestTornDeltaCarriesNoAttribution proves the 2026-08-06 delta cannot classify
// the sighting: the number of accounts that could have produced it as a net flow
// is indistinguishable from what an ARBITRARY value produces on this workload.
//
// Without the power check the observed count would look like evidence. It is not:
// a test that fires for almost every input has no discriminating power, and the
// comparison against random targets is what converts "3 of 32 accounts could have
// done it" from a finding into a non-finding.
func TestTornDeltaCarriesNoAttribution(t *testing.T) {
	cfg := sightingConfig()
	p := generatePlan(&cfg)

	// Guard against drifting onto a different workload than the one that failed.
	const sightingExpectedTotal = int64(46625986168)
	if p.initialTotal != sightingExpectedTotal {
		t.Fatalf("plan initialTotal = %d, want the sighting's %d — this file no longer "+
			"reconstructs the run that produced the delta it reasons about",
			p.initialTotal, sightingExpectedTotal)
	}

	runs := accountRunsByWriter(&p, cfg.writers)
	sets := make([]map[int64]struct{}, 0, len(runs))
	for _, byW := range runs {
		sets = append(sets, achievableNetFlows(byW))
	}
	observed := countAccountsProducing(sets, tornSightingDelta)

	// The null distribution: how many accounts an arbitrary in-range value reaches.
	//nolint:gosec // G404: a fixed seed keeps this test deterministic.
	rng := rand.New(rand.NewSource(20260806))
	const trials = 200
	var total, none int
	minHit, maxHit := 1<<30, 0
	for i := 0; i < trials; i++ {
		n := countAccountsProducing(sets, rng.Int63n(1_000_000)+1)
		total += n
		if n == 0 {
			none++
		}
		if n < minHit {
			minHit = n
		}
		if n > maxHit {
			maxHit = n
		}
	}
	mean := float64(total) / float64(trials)

	t.Logf("delta %d is producible by %d of %d accounts; random in-range values reach "+
		"mean %.2f (min %d, max %d), and only %.1f%% of them reach none",
		tornSightingDelta, observed, len(sets), mean, minHit, maxHit,
		100*float64(none)/float64(trials))

	// The claim: the observed delta is UNREMARKABLE. If it were ever to become
	// remarkable — reachable by no account, or by far more than chance — that
	// would be new information about the sighting and this test must say so.
	if observed == 0 {
		t.Errorf("delta %d is reachable by NO account as a net flow, which would REFUTE "+
			"the 'one account resolved at a different instant' shape outright — a real "+
			"finding for rmp #2336, not a passing test", tornSightingDelta)
	}
	if float64(observed) > 4*mean+1 {
		t.Errorf("delta %d reaches %d accounts against a chance mean of %.2f — unexpectedly "+
			"distinctive, which would be new evidence for rmp #2336", tornSightingDelta, observed, mean)
	}

	// And the power check itself: if the null distribution ever collapsed so that
	// arbitrary values stopped being reachable, the reasoning above would no longer
	// hold and the vacuity conclusion would have to be revisited.
	if none > trials/4 {
		t.Errorf("%d of %d random values reach no account: the arithmetic test has regained "+
			"discriminating power on this workload, so rmp #2336's delta is worth re-examining",
			none, trials)
	}
}
