//go:build soak

package main

// soak_torn_test.go — the standing reproduction harness for the torn-total
// sighting of rmp #2333.
//
// # Why this exists
//
// On 2026-08-06 a full `make ci` reported one torn total in this example and it has
// never been seen again. Everything that followed was a manual sweep: individual
// invocations, concurrent invocations, invocations under the race detector, under
// coverage, and under a constrained GOMAXPROCS. Roughly 47 million further
// observations came back clean, and every one of those sweeps had to be
// re-improvised because none of them was written down as a runnable thing.
//
// A defect at that rate is not found by a gate that runs for two seconds inside the
// short layer. It is found by an instrument that runs for as long as it is given,
// which is what the soak layer is for. This is that instrument: the same workload
// the short-layer gate runs, repeated until the budget is spent, failing on the
// FIRST torn observation and printing the same-instant attribution the gate now
// captures (see [tornReport]).
//
// # What a failure here means, and what it does not
//
// It fails on a torn total and on nothing else. A conflict-retry exhaustion, a lost
// update or a conservation break are different findings with different messages, and
// they are reported as themselves rather than folded into this one.
//
// The observation COUNT is the product even when the run is clean: it is the
// denominator of the rate anybody may later quote, and quoting a rate without it is
// how rmp #2333's original arithmetic came to be believed. It is therefore always
// logged, pass or fail.

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// soakTornBudget bounds the search. It is a WALL-CLOCK budget rather than a run
// count because a run's duration varies by an order of magnitude across the build
// arms this has to be exercised under (plain, -race, coverage), and a count would
// silently mean something different in each.
const soakTornBudget = 4 * time.Minute

// soakTornBudgetEnv overrides [soakTornBudget] with any duration Go can parse. It
// exists so the harness can be smoke-checked in seconds — a search instrument that
// has never been observed to run is not an instrument — and so an operator hunting
// this defect can give it hours without editing the source.
const soakTornBudgetEnv = "GOGRAPH_TORN_SEARCH_BUDGET"

// tornSearchBudget resolves the wall-clock budget for the search.
func tornSearchBudget(tb testing.TB) time.Duration {
	tb.Helper()
	raw, ok := os.LookupEnv(soakTornBudgetEnv)
	if !ok {
		return soakTornBudget
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		tb.Fatalf("%s=%q is not a positive duration: %v", soakTornBudgetEnv, raw, err)
	}
	return d
}

// TestSoak_TornTotalSearch runs the conservation workload until the budget is spent,
// failing on the first torn observation with its attribution.
func TestSoak_TornTotalSearch(t *testing.T) {
	testlayers.RequireSoak(t)

	// The shape of the sighting: 3 writers, 7 readers, 200 transfers each, no
	// sweep — TestRunReproducibleAcrossReaderScaling's configuration, which is
	// what actually produced the one observation on record.
	cfg := defaultConfig()
	cfg.writers = 3
	cfg.readers = 7
	cfg.opsPerWriter = 200
	cfg.sweepOps = 0

	ctx := context.Background()
	budget := tornSearchBudget(t)
	deadline := time.Now().Add(budget)

	var (
		runs         int
		observations int64
	)
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		err := run(ctx, &buf, &cfg)
		runs++
		observations += readerObservations(&buf)

		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "ISOLATION VIOLATION") {
			t.Fatalf("run %d failed for a reason other than a torn total after %d observations: %v",
				runs, observations, err)
		}
		// The find. Everything needed to attribute it is already in the message.
		t.Fatalf("REPRODUCED after %d runs / %d observations:\n%v", runs, observations, err)
	}

	// Clean. The denominator is the product; state it.
	t.Logf("no torn total in %d runs / %d observations over %s", runs, observations, budget)
	if observations == 0 {
		t.Fatalf("the search made %d runs but counted 0 observations — the harness is not measuring what it claims", runs)
	}
}

// readerObservations extracts the "# reader.observations=N" telemetry line, so the
// search reports a denominator rather than a run count. It returns 0 when the line
// is absent, which the caller treats as a broken harness rather than as a clean run.
func readerObservations(buf *bytes.Buffer) int64 {
	const key = "# reader.observations="
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, key); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}
