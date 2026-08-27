package cypher

// range_seek_population_floor_test.go — an equality lookup on an indexed
// property must not get dramatically slower because the graph is SMALLER
// (rmp #2367).
//
// # What went wrong
//
// rangeSeekMinLabelPopulation suppressed the range-index seek for any label
// below 1024 nodes, on the premise that "a sub-1024-node label scan is a few
// microseconds on a warm cache and the index-descent + bitmap overhead cannot
// beat it". MEASURED at the floor's own boundary that was false by more than an
// order of magnitude: `MATCH (n:Account {id: $id}) RETURN n.v` cost 68.7 us over
// a label of 1023 nodes and 5.5 us over one of 1024 — a 12.6x cliff at a
// constant, reproduced in both sweep directions.
//
// The visible symptom was an inversion: per-operation cost got WORSE as the
// graph got SMALLER, for identical timed work. That inversion is what this file
// pins.
//
// # Why this asserts ALLOCATIONS and not a duration
//
// The acceptance criterion asks for behaviour rather than plan text, and the
// behaviour that matters is "the work does not grow with the population". A
// wall-clock assertion would express that directly and would also be a flake
// generator: this sprint is largely a cleanup of gates that could not separate a
// real regression from machine load (#2517, #2572, #2589, #2506, #2499).
//
// Allocations per operation say the same thing and cannot be moved by load. The
// precedent is in this tree: rmp #2359's fixture defect was caught by exactly
// this reasoning — "allocs/op climbing 195 -> 16052 with the writer count: an
// op's allocation count cannot depend on how many peers are running". An
// equality lookup's allocation count cannot depend on how many OTHER nodes carry
// the label either.
//
// MEASURED over [testing.AllocsPerRun], integer key, allocations per lookup:
//
//	population    floor 1024 (defect)    floor 64 (fixed)
//	        64                     98                 113
//	       256                    106                 121
//	       512                    368                 127
//	      1023                    889                 124
//	      1024                    125                 125
//	      4096                    127                 123
//
// Flat after, and 7.1x at the old floor's boundary before.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	// rsfSmall sits one node BELOW the old floor of 1024 and rsfLarge well above
	// it, so the pair straddles exactly the boundary that exposed the defect and
	// carries its strongest signal (889 allocations against 127). Both are above
	// the current floor, which is the point: the seek must fire for both.
	rsfSmall = 1023
	rsfLarge = 4096
	// rsfAllocSlack is the absolute allowance between the two arms. It is an
	// ABSOLUTE number, not a ratio of one to the other: a proportional bound would
	// pass on a build where both arms had regressed together. 64 is far above the
	// measured spread (124 against 123) and far below the defect (889 against 127).
	rsfAllocSlack = 64
	// rsfQuery is an equality predicate on an indexed property — the shape a
	// Cypher CREATE INDEX exists to serve.
	rsfQuery = "MATCH (n:Account {id: $id}) RETURN n.v AS v"
)

// seedRSF builds population nodes carrying :Account with an INTEGER id, plus a
// Cypher CREATE INDEX over (n:Account).id.
//
// The id is an INTEGER deliberately. A Cypher CREATE INDEX builds a
// STRING-keyed hash index and tryNewHashSeek declines a non-string seek against
// it, so an integer key cannot reach the hash path at all and the RANGE path is
// the only index access available — which is precisely the path the floor
// governed. A string-keyed fixture would seek through the hash path at every
// population and would therefore pass this test without exercising it.
func seedRSF(t *testing.T, population int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	if _, err := eng.RunInTx(ctx, "CREATE INDEX acct_id FOR (n:Account) ON (n.id)", nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	for i := 0; i < population; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (n:Account {id: $id, v: 0})", map[string]expr.Value{
			"id": expr.IntegerValue(int64(i)),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return eng
}

// rsfLookupAllocs returns the allocations one lookup costs, and the rows it
// returned, so an allocation figure can never be read without knowing the query
// answered anything.
func rsfLookupAllocs(t *testing.T, eng *Engine, id int64) (allocs float64, returned int64) {
	t.Helper()
	ctx := context.Background()
	params := map[string]expr.Value{"id": expr.IntegerValue(id)}

	run := func() int64 {
		res, err := eng.Run(ctx, rsfQuery, params)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		var n int64
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return n
	}

	returned = run()
	allocs = testing.AllocsPerRun(rsfAllocRuns, func() { _ = run() })
	return allocs, returned
}

// rsfAllocRuns is the sample count for [testing.AllocsPerRun]. Allocation counts
// are deterministic for this shape, so a modest sample suffices; it exists only
// to average away the occasional map growth on the first iterations.
const rsfAllocRuns = 30

// TestEqualityLookupWorkIsIndependentOfLabelPopulation is the regression gate.
func TestEqualityLookupWorkIsIndependentOfLabelPopulation(t *testing.T) {
	smallAllocs, smallReturned := rsfLookupAllocs(t, seedRSF(t, rsfSmall), 0)
	largeAllocs, largeReturned := rsfLookupAllocs(t, seedRSF(t, rsfLarge), 0)

	// Both must answer the query, or "it allocated little" is satisfied by a plan
	// that found nothing.
	if smallReturned != 1 || largeReturned != 1 {
		t.Fatalf("the lookup returned %d rows at %d nodes and %d at %d; both must return exactly 1, "+
			"or the allocation figures below describe a query that answered nothing",
			smallReturned, rsfSmall, largeReturned, rsfLarge)
	}

	// The invariant. The populations differ by 4x; the work must not.
	if smallAllocs > largeAllocs+rsfAllocSlack {
		t.Errorf("an equality lookup on an indexed property cost %.0f allocations over a label of "+
			"%d nodes and %.0f over a label of %d — %.0f more, against an allowance of %d. An "+
			"operation's allocation count cannot depend on how many OTHER nodes carry the label. "+
			"rangeSeekMinLabelPopulation used to suppress the range seek below 1024, which made "+
			"the SMALLER graph cost 889 allocations against 127 and run 12.6x slower for "+
			"identical work (#2367)",
			smallAllocs, rsfSmall, largeAllocs, rsfLarge, smallAllocs-largeAllocs, rsfAllocSlack)
	}

	// And it must be the seek's cost, not the scan's. Without this the comparison
	// above is satisfied by both arms scanning their whole label, which is what the
	// defect did whenever both populations sat below the floor.
	if smallAllocs >= float64(rsfSmall)/4 {
		t.Errorf("the lookup cost %.0f allocations over a label of %d nodes, which is the order of "+
			"the population rather than of the answer. An indexed equality must touch the matching "+
			"rows, not the label", smallAllocs, rsfSmall)
	}
}

// TestRangeSeekFloorIsBelowTheReproductionPopulation pins the constant against
// the fixture that exposed it, so a future rise past 256 cannot silently
// reintroduce the cliff while the test above still passes on a bigger fixture.
func TestRangeSeekFloorIsBelowTheReproductionPopulation(t *testing.T) {
	if rangeSeekMinLabelPopulation > rsfSmall {
		t.Fatalf("rangeSeekMinLabelPopulation is %d, above the %d-node fixture this defect was "+
			"reproduced on; the gate above would then pass by scanning both arms",
			rangeSeekMinLabelPopulation, rsfSmall)
	}
}

// TestRangeSeekFloorStillExists is the other half of the constant's contract. The
// floor was lowered, not removed: keeping one is what stops a trivial label from
// paying an exact RangeCount to decide something it cannot win, which is the same
// "a gate must cost less than the decision it informs" rule #2380 and #2392
// imposed on the parallel-scan gate. A build that deleted the floor would pass
// every assertion above and lose that property silently.
func TestRangeSeekFloorStillExists(t *testing.T) {
	if rangeSeekMinLabelPopulation <= 1 {
		t.Errorf("rangeSeekMinLabelPopulation is %d, i.e. no floor at all. Below the floor no "+
			"count can change the verdict, which is why rangeSeekBudget refuses to take one; "+
			"removing it makes every trivial-label equality pay for a decision it cannot win",
			rangeSeekMinLabelPopulation)
	}
}
