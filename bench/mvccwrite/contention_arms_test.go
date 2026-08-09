package mvccwrite

// contention_arms_test.go — one write-scaling arm per named contention source
// (rmp #2359).
//
// # Why this exists before any contention fix
//
// The pre-existing scaling arm drives `CREATE (n:Account {id: $id})`. Under that one
// statement shape several of the structures the sprint-336 audit named are never
// touched at all: the property delta chain (a create writes a first version, it never
// UPDATES one), adjacency and the per-edge side stores, the count store, and the label
// store's removal direction. Measuring a fix to any of those against a node-only
// create can only return "no change" — and that answer would be a property of the
// WORKLOAD, not evidence about the fix.
//
// Sprint 335 already paid for this mistake: cutting the per-writer allocation rate by
// 17.5 % moved the measured ceiling from 2.718× to 2.713× (p=1.000). The number was
// real; it just could not see the thing that had changed.
//
// # The oracle is the point, not the benchmark
//
// A benchmark arm is worthless if it cannot observe its target, and "no change" is
// exactly what an unobservable arm reports — indistinguishable from a fix that did
// nothing. So every arm here carries an `observe` function that names the substrate
// counters it must move, and TestWriteContentionArms_TouchTheirTargets runs every arm
// briefly and FAILS if the counters did not move. That test runs in the ordinary short
// layer, so the oracle is checked on every `make ci` even though benchmarks are not.
//
// # How these arms must be run
//
// # DO NOT QUOTE THE `scaling` METRIC OF THE MUTATING ARMS YET (rmp #2359)
//
// The arms and their oracle are sound; the SCALING RATIO of the arms that mutate a
// pooled node is not yet, and the reason is written here so nobody quotes it.
//
// Two fixture artefacts were found by measuring, and each one inverted the verdict:
//
//  1. NO INDEX ON THE LOOKUP KEY. `MATCH (n:Account {id: $id})` was a label scan, so
//     each unit's cost grew with the seeded node count — which grows with the writer
//     count, since every writer owns a pool. update-property read as COLLAPSING to
//     0.145x at 32 writers; with the index it reads 28.3x. create-edge went 0.142x to
//     19.1x. The tell was allocs/op climbing 195 -> 16052 with the writer count: an
//     op's allocation count cannot depend on how many peers are running. FIXED, in
//     seedPool.
//
//  2. STILL OPEN, and my explanation for it was TESTED AND REFUTED. update-property
//     reports an impossible 28x scaling at 32 writers: 50.9 us per commit at one
//     writer against 1.85 us at 32, i.e. per-commit cost ~27x LOWER with more
//     writers. I hypothesised per-node churn — each writer runs b.N/writers
//     iterations over a fixed pool, so one writer would give each node ~writers times
//     more updates and a deeper delta chain to walk. So I made per-writer work
//     CONSTANT, which makes each node take ~78 updates at EVERY writer count, and the
//     ratio moved 28.28x -> 27.55x. Churn is not the cause, and the speculative change
//     was reverted rather than shipped.
//
//     RESOLVED — IT WAS THE FIXTURE, and the mechanism is a documented contract.
//     A Cypher CREATE INDEX builds a STRING-KEYED hash index, and
//     cypher.tryNewHashSeek declines a seek whose value kind does not match the index
//     key type (see its #F-CY2 contract note: "a Cypher CREATE INDEX never builds" an
//     int64 hash index). The pool was keyed on an INTEGER, so every lookup fell back to
//     a label scan with a row filter — which is why the cost tracked the graph size and
//     why the whole cascade of false readings below happened. contentionKey now returns
//     a STRING. Measured after: 3212 ns/op at 256 nodes against 2949 at 4096, i.e.
//     node-count INDEPENDENT, and allocs/op flat at 52-53 across writers 1..32 where it
//     had been 174 -> 16052. rmp #2367 is retired by that evidence.
//
//     THE HISTORICAL READING, kept because four hypotheses were needed to get here and
//     three of them were wrong: seedPool creates
//     contentionPool nodes PER WRITER, so :Account holds 256 rows at one writer and
//     8192 at 32 — and per-operation cost is 10x WORSE with FEWER nodes, for IDENTICAL
//     timed work (writer 0 always updates keys 0..255, ~12 updates each either way).
//     Ordered 16/4/1/16 so warm-up cannot explain it:
//
//     	4096 other nodes ->  4354 ns/op        (seek)
//     	1024 other nodes ->  4025 ns/op        (seek)
//     	 256 other nodes -> 41933 ns/op        (scan: 10x SLOWER)
//     	4096 other nodes ->  4024 ns/op        (seek)
//
//     So per-commit cost falls ~27x as writers rise because the GRAPH GROWS, not
//     because contention eases, and the impossible 28x "scaling" is that and nothing
//     more. Filed as rmp #2367.
//
//     A CPU profile of the slow case attributes ~31 % cumulative to
//     NodeByLabelScan.Next plus newRowPredicate, which SUGGESTS the index is not being
//     consulted at 256 rows. That is a SUGGESTION and not the diagnosis: a first search
//     of the planner found NO cardinality threshold governing scan-versus-seek, so the
//     "cost-based planner with a mis-calibrated crossover" reading was WITHDRAWN from
//     #2367 as unsupported. EXPLAIN is not accepted by the parser at all, for read or
//     write, so no plan dump was available to settle it.
//
//     FIVE explanations were proposed and FOUR were wrong: a missing index (an index
//     was added, and it was silently unusable); per-node churn; unswept delta chains;
//     and a cost-based planner with a mis-calibrated crossover, which was WITHDRAWN
//     when no such threshold existed. The base rate for a plausible-sounding mechanism
//     here is poor — check the contract before theorising about the substrate.
//
// # WHAT THE ARMS NOW SHOW, and the one that FAILS
//
// With the lookup seeking, at writers 1/4/16/32: create-labelled-node 2764 -> 1296
// ns/op, update-property 3098 -> 1508 with allocs/op FLAT at 52-53, create-edge and
// mixed likewise steady. label-add-remove passes at 1 and 4 writers (4434, 3163 ns/op)
// and FAILS at 16 and 32 with "still conflicting after 64 retries" on nodes NO peer
// touches, since every writer owns a disjoint key range. That is a LIVELOCK, filed as
// rmp #2368, and it is ATTRIBUTED: 30dc804b (pre-#2354) PASSES at 16 writers with
// 3496 ns/op and 0 retries/op, b1f0974f (the #2354 commit) FAILS, HEAD fails
// identically — same arm source in all three, so only the engine differs.
//
// So rmp #2354 introduced it, and this refutes a claim #2354 made in its own commit
// message: that testing the label head unconditionally "cannot reintroduce a spurious
// abort" because the head re-asserted over "is its OWN write or an older committed one
// — visible either way". That is false for consecutive autocommit statements from one
// goroutine, and this arm is the counter-example. #2354's five regression tests are all
// SAFETY assertions and none of them could observe a LIVENESS defect, which is why a
// green suite said nothing about it.
//
// The benchmark is left FAILING rather than tuned away: it is the only thing in the
// tree that reports this.
//
// # How these arms must be run
//
// # DO NOT QUOTE THE `scaling` METRIC OF THE MUTATING ARMS YET (rmp #2359)
//
// The arms and their oracle are sound; the SCALING RATIO of the arms that mutate a
// pooled node is not yet, and the reason is written here so nobody quotes it.
//
// Two fixture artefacts were found by measuring, and each one inverted the verdict:
//
//  1. NO INDEX ON THE LOOKUP KEY. `MATCH (n:Account {id: $id})` was a label scan, so
//     each unit's cost grew with the seeded node count — which grows with the writer
//     count, since every writer owns a pool. update-property read as COLLAPSING to
//     0.145x at 32 writers; with the index it reads 28.3x. create-edge went 0.142x to
//     19.1x. The tell was allocs/op climbing 195 -> 16052 with the writer count: an
//     op's allocation count cannot depend on how many peers are running. FIXED, in
//     seedPool.
//
//  2. STILL OPEN, and my explanation for it was TESTED AND REFUTED. update-property
//     reports an impossible 28x scaling at 32 writers: 50.9 us per commit at one
//     writer against 1.85 us at 32, i.e. per-commit cost ~27x LOWER with more
//     writers. I hypothesised per-node churn — each writer runs b.N/writers
//     iterations over a fixed pool, so one writer would give each node ~writers times
//     more updates and a deeper delta chain to walk. So I made per-writer work
//     CONSTANT, which makes each node take ~78 updates at EVERY writer count, and the
//     ratio moved 28.28x -> 27.55x. Churn is not the cause, and the speculative change
//     was reverted rather than shipped.
//
//     The VACUUM/CHAIN-DEPTH candidate has since been ELIMINATED too, by the
//     straightforward test: if unswept delta chains were the cost, ns/op at one writer
//     would RISE with the number of updates each pooled node receives. It FALLS —
//     96 us/op at ~7 updates/node (b.N=2000), 63 us at ~31 (b.N=8000), 47 us at ~125
//     (b.N=32000), with allocs/op flat at 160-172 throughout. Deeper chains are
//     cheaper here, so chain depth is not the mechanism; a FIXED cost being amortised
//     over b.N is. Fitting total time against b.N puts that fixed cost at roughly
//     0.17 s per sub-benchmark, which is inside the timed region and is not the seed
//     (seedPool runs before ResetTimer).
//
//     So TWO explanations were proposed and refuted by measurement. A CPU profile of
//     the single-writer arm then named the actual cost, and it is STILL the fixture:
//     `NodeByLabelScan.Next` plus `newRowPredicate` account for ~31 % cumulative, so
//     `MATCH (n:Account {id: $id})` is executing as a LABEL SCAN WITH A ROW FILTER and
//     the index created in seedPool is NOT being used. Artefact 1 was therefore only
//     half fixed: the index exists and is not picked. (`EXPLAIN` is rejected on this
//     write shape, so the plan was established from the profile rather than from a
//     plan dump.)
//
//     CONSEQUENCE FOR EVERY NUMBER HERE: the mutating arms' ABSOLUTE figures are
//     inflated by a scan proportional to the seeded node count, and that count grows
//     with the writer count, so the cross-writer ratio is confounded as well. Nothing
//     in these arms says anything about engine contention until the lookup is a seek.
//     Whether the planner cannot use an index for `MATCH (n:Label {prop: $param})` in
//     a write statement, or seedPool declares it wrongly, is the FIRST thing to settle
//     — and if it is the planner, that is a finding about the engine and not about this
//     file. Report MVCCStats.ChainDepth alongside regardless: having it would have
//     killed the chain hypothesis in one run instead of two.
//
// So: the per-arm ABSOLUTE numbers at a FIXED writer count are usable, and the
// oracle guarantees each arm touches its target. The cross-writer-count ratio of a
// mutating arm is not usable until the fixture holds per-node churn constant. The
// create-labelled-node arm is unaffected — it creates a fresh node every iteration —
// which is why its 2.13x matches BenchmarkWriteScaling.
//
// INTERLEAVED, never back to back: build two `go test -c` binaries and alternate them
// inside one loop, n>=10, compared with benchstat. An across-time comparison on this
// host has been shown worthless. WITHOUT -race, and with NO competing load —
// TestWriteScalingInstrument_SeesConcurrency measures 7.2× in isolation and has been
// observed failing at 2.78× against its own 3.00× floor while `make ci` ran with a
// five-minute load average of 18.8. Treat any measurement taken under load as void.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// contentionPool is how many distinct nodes each writer owns.
//
// Bounded on purpose: the update and edge arms must mutate nodes that ALREADY exist,
// and pre-creating one per timed iteration would put the whole graph build inside the
// arm's setup and scale it with b.N. A fixed pool keeps setup constant while still
// spreading each writer's writes over many objects, so the arm measures the shared
// structure rather than one object's delta chain.
//
// It is also why writers cannot collide logically: every writer owns a disjoint key
// range, so any contention these arms show is contention on SHARED STATE, not
// serialization conflict.
const contentionPool = 256

// contentionWorkload is one arm: a name, an optional graph to build first, the unit of
// work to time, and the oracle that proves the unit touched what the arm claims.
type contentionWorkload struct {
	name string
	// setup builds whatever the unit needs to already exist. It runs before the timer.
	setup func(ctx context.Context, eng *cypher.Engine, writers int) error
	// unit is one transaction's worth of work for writer w on iteration i.
	unit func(ctx context.Context, eng *cypher.Engine, w, i int) error
	// observe names the substrate counters this arm must move, and is what stops a
	// "no change" result from being an unobservability artefact.
	observe func(before, after lpg.MVCCStats) error
}

// key packs a writer's id space so the ranges are disjoint, exactly as the
// pre-existing arm does.
// It returns a STRING, and that is load-bearing rather than cosmetic. A Cypher
// CREATE INDEX builds a STRING-KEYED hash index, and cypher.tryNewHashSeek declines a
// seek whose value kind does not match the index key type — a documented contract, not
// a defect. Keying the pool on an INTEGER therefore made every lookup fall back to a
// label scan with a row filter, which is what produced this file's whole cascade of
// false readings. Measured after the change: 3212 ns/op at 256 nodes against 2949 at
// 4096, i.e. node-count INDEPENDENT, where the integer key gave 41933 against 4025.
func contentionKey(w, i int) string { return "w" + itoaSmall(w) + "-" + itoaSmall(i%contentionPool) }

// itoaSmall formats a small non-negative int without pulling strconv in.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	p := len(b)
	for n > 0 {
		p--
		b[p] = byte('0' + n%10)
		n /= 10
	}
	return string(b[p:])
}

// runOne executes q with the given parameters in one autocommit transaction and
// drains it, because a write-operator error is reported on the Result and not by
// RunInTx.
func runOne(ctx context.Context, eng *cypher.Engine, q string, params map[string]expr.Value) error {
	res, err := eng.RunInTx(ctx, q, params)
	if err != nil {
		return err
	}
	if rerr := res.Err(); rerr != nil {
		_ = res.Close()
		return rerr
	}
	return res.Close()
}

// seedPool creates each writer's pool of nodes, so the mutating arms have something
// to mutate.
func seedPool(ctx context.Context, eng *cypher.Engine, writers int) error {
	// AN INDEX ON THE LOOKUP KEY, and it is load-bearing for the MEASUREMENT rather
	// than for the engine.
	//
	// Without it, `MATCH (n:Account {id: $id})` is a label scan, so every unit's cost
	// grows with the number of seeded nodes — which grows with the writer count,
	// because each writer owns a pool. The first run of these arms reported
	// update-property collapsing to 0.145x at 32 writers with allocs/op climbing 195
	// -> 16052, and that was almost entirely the fixture: an op's allocation count
	// cannot depend on how many peers are running. A real workload looks up by an
	// indexed key, and so must this one, or the arm measures its own seed data.
	if err := runOne(ctx, eng, `CREATE INDEX acct_k FOR (n:Account) ON (n.k)`, nil); err != nil {
		return fmt.Errorf("seed index: %w", err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < contentionPool; i++ {
			if err := runOne(ctx, eng, `CREATE (n:Account {k: $k, v: 0})`,
				map[string]expr.Value{"k": expr.StringValue(contentionKey(w, i))}); err != nil {
				return fmt.Errorf("seed w=%d i=%d: %w", w, i, err)
			}
		}
	}
	return nil
}

// writerSessions holds one cypher.Session per writer, so an arm whose statements are
// DEPENDENT gets read-your-own-writes. Keyed by writer index; built lazily under a
// mutex because arms are constructed once and driven from many goroutines.
var (
	writerSessionsMu sync.Mutex
	writerSessions   map[int]*cypher.Session
	writerSessionEng *cypher.Engine
)

// writerSession returns writer w's session on eng, resetting the table when the engine
// changes (each sub-benchmark builds a fresh one).
func writerSession(eng *cypher.Engine, w int) *cypher.Session {
	writerSessionsMu.Lock()
	defer writerSessionsMu.Unlock()
	if writerSessionEng != eng {
		writerSessionEng, writerSessions = eng, map[int]*cypher.Session{}
	}
	s, ok := writerSessions[w]
	if !ok {
		s = eng.NewSession()
		writerSessions[w] = s
	}
	return s
}

// drainResult drains a statement and returns the first error from either channel.
func drainResult(r *cypher.Result, err error) error {
	if err != nil {
		return err
	}
	for r.Next() { //nolint:revive // draining is the point
	}
	re := r.Err()
	_ = r.Close()
	return re
}

// grew reports the delta of one counter, naming it for the failure message.
func grew(name string, before, after int64) error {
	if after <= before {
		return fmt.Errorf("%s did not grow (%d -> %d): this arm cannot observe its target, "+
			"so any 'no change' it reports would be an artefact", name, before, after)
	}
	return nil
}

// contentionRetries counts serialization conflicts retried across an arm, so a
// conflict is MEASURED rather than fatal.
//
// A conflict here is not a harness defect to be silenced: an arm that aborts on the
// first one cannot report throughput at the writer counts where contention actually
// bites, which is precisely the range this task exists to measure. A real client
// retries, so the arm retries and REPORTS the rate as retries/op — making the
// conflict rate a first-class number of every arm rather than an invisible cost
// folded into latency.
//
// It is also a finding in its own right: label-add-remove tripped
// mvcc.ErrSerializationConflict at sixteen writers on nodes NO other writer touches
// (every writer owns a disjoint key range), so consecutive autocommit statements from
// one goroutine can collide with their own predecessor. Recorded on rmp #2359 rather
// than guessed at here.
var contentionRetries atomic.Int64

// maxContentionRetries bounds the retry loop so a genuine livelock fails the arm
// instead of hanging it.
const maxContentionRetries = 64

// withRetry runs fn, retrying a serialization conflict and counting it.
func withRetry(fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !errors.Is(err, mvcc.ErrSerializationConflict) {
			return err
		}
		if attempt >= maxContentionRetries {
			return fmt.Errorf("still conflicting after %d retries: %w", maxContentionRetries, err)
		}
		contentionRetries.Add(1)
	}
}

// contentionWorkloads is the set of arms. Each one exists because the node-only
// create cannot observe the structure it names.
var contentionWorkloads = []contentionWorkload{
	{
		// The BASELINE, kept here so every other arm is read against a shape whose
		// numbers are already established by BenchmarkWriteScaling.
		name: "create-labelled-node",
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			return runOne(ctx, eng, `CREATE (n:Account {id: $id})`,
				map[string]expr.Value{"id": expr.IntegerValue(int64(w)<<40 | int64(i))})
		},
		observe: func(b, a lpg.MVCCStats) error { return grew("NodeLifeRecords", b.NodeLifeRecords, a.NodeLifeRecords) },
	},
	{
		// PROPERTY STORE, update direction. A create writes a first version; only an
		// update extends a delta chain, which is what the property store's conflict
		// test and its reclamation actually walk.
		name:  "update-property",
		setup: seedPool,
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			return runOne(ctx, eng, `MATCH (n:Account {k: $k}) SET n.v = $v`,
				map[string]expr.Value{
					"k": expr.StringValue(contentionKey(w, i)),
					"v": expr.IntegerValue(int64(i)),
				})
		},
		observe: func(b, a lpg.MVCCStats) error { return grew("PropDeltas", b.PropDeltas, a.PropDeltas) },
	},
	{
		// LABEL STORE, both directions. The node-only create never removes a label, so
		// the removal path — and the deferred label-index removal it feeds — is
		// invisible to the baseline arm.
		name:  "label-add-remove",
		setup: seedPool,
		// THROUGH ONE SESSION PER WRITER, and that is a correctness requirement of the
		// ARM, not a tuning choice. The two statements are DEPENDENT: the REMOVE must
		// observe the SET. cypher/session.go promises read-your-own-writes only within
		// a Session; a bare Engine caller gets snapshot isolation alone, so consecutive
		// autocommit statements have no guarantee of seeing their own predecessor.
		//
		// Written with bare eng.RunInTx this arm LIVELOCKED at 16 and 32 writers — 64
		// fresh retries all refused, on nodes no peer touches — which was filed as rmp
		// #2368 and looked like an engine defect introduced by rmp #2354. It was the
		// arm. Through per-writer Sessions the same workload runs with ZERO retries and
		// ZERO conflicts. See TestLabelToggle_PerWriterSessionMakesProgress.
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			id := map[string]expr.Value{"k": expr.StringValue(contentionKey(w, i))}
			s := writerSession(eng, w)
			if err := drainResult(s.RunInTx(ctx, `MATCH (n:Account {k: $k}) SET n:Hot`, id)); err != nil {
				return err
			}
			return drainResult(s.RunInTx(ctx, `MATCH (n:Account {k: $k}) REMOVE n:Hot`, id))
		},
		observe: func(b, a lpg.MVCCStats) error { return grew("LabelDeltas", b.LabelDeltas, a.LabelDeltas) },
	},
	{
		// ADJACENCY, the per-edge SIDE STORES and the COUNT STORE — none of which a
		// node-only create touches at all.
		name:  "create-edge",
		setup: seedPool,
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			// BOTH ENDPOINTS ARE FRESH, and that is what makes the arm measurable.
			//
			// It used to join two POOLED nodes, so pairs repeated and the arm
			// accumulated PARALLEL EDGES between them — as many as b.N/writers per
			// pair. Per-op cost then depended on the writer count (allocs/op ran
			// 1118 -> 215 across writers 1..32), so the arm failed the independence
			// invariant that says a fixture is not leaking into its own measurement,
			// and it was excluded from the rmp #2359 baseline for that reason.
			//
			// Creating both endpoints per iteration keeps adjacency, the per-edge side
			// stores and the count store exercised — the three structures this arm
			// exists for — while no node's degree and no pair's edge count grows with
			// b.N. It is a heavier unit than a bare create, as expected: two nodes and
			// an edge.
			return runOne(ctx, eng,
				`CREATE (a:Account {k: $a})-[:PAYS {amt: $amt}]->(b:Account {k: $b})`,
				map[string]expr.Value{
					"a":   expr.StringValue("e" + itoaSmall(w) + "-" + itoaSmall(i) + "-a"),
					"b":   expr.StringValue("e" + itoaSmall(w) + "-" + itoaSmall(i) + "-b"),
					"amt": expr.IntegerValue(int64(i)),
				})
		},
		observe: func(b, a lpg.MVCCStats) error {
			if err := grew("AdjVersions", b.AdjVersions, a.AdjVersions); err != nil {
				return err
			}
			return grew("EdgeSideVersions", b.EdgeSideVersions, a.EdgeSideVersions)
		},
	},
	{
		// MIXED, because a real workload interleaves shapes and the shared structures
		// are contended together rather than one at a time.
		name:  "mixed",
		setup: seedPool,
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			switch i % 3 {
			case 0:
				return runOne(ctx, eng, `CREATE (n:Account {k: $k})`,
					map[string]expr.Value{"k": expr.StringValue("m" + itoaSmall(w) + "-" + itoaSmall(i))})
			case 1:
				return runOne(ctx, eng, `MATCH (n:Account {k: $k}) SET n.v = $v`,
					map[string]expr.Value{
						"k": expr.StringValue(contentionKey(w, i)),
						"v": expr.IntegerValue(int64(i)),
					})
			default:
				return runOne(ctx, eng,
					`MATCH (a:Account {k: $a}), (b:Account {k: $b}) CREATE (a)-[:PAYS]->(b)`,
					map[string]expr.Value{
						"a": expr.StringValue(contentionKey(w, i)),
						"b": expr.StringValue(contentionKey(w, i+1)),
					})
			}
		},
		observe: func(b, a lpg.MVCCStats) error {
			if err := grew("PropDeltas", b.PropDeltas, a.PropDeltas); err != nil {
				return err
			}
			return grew("AdjVersions", b.AdjVersions, a.AdjVersions)
		},
	},
}

// TestWriteContentionArms_TouchTheirTargets is the ORACLE, and the reason this task
// came before any contention fix.
//
// For each arm it drives a short burst and asserts the substrate counters that arm
// claims to exercise actually moved. An arm that cannot move them is an arm whose
// "no change" verdict means nothing, and this is the only thing standing between a
// future perf task and an unfalsifiable claim.
//
// It runs in the SHORT layer deliberately: the oracle must be checked on every gate,
// not only when somebody remembers to run the benchmarks.
func TestWriteContentionArms_TouchTheirTargets(t *testing.T) {
	for _, wl := range contentionWorkloads {
		t.Run(wl.name, func(t *testing.T) {
			g := lpg.New[string, float64](contentionAdjConfig())
			eng := cypher.NewEngine(g)
			ctx := context.Background()
			const writers, iters = 2, 8

			if wl.setup != nil {
				if err := wl.setup(ctx, eng, writers); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			before := g.MVCCStats()
			for w := 0; w < writers; w++ {
				for i := 0; i < iters; i++ {
					if err := wl.unit(ctx, eng, w, i); err != nil {
						t.Fatalf("unit w=%d i=%d: %v", w, i, err)
					}
				}
			}
			after := g.MVCCStats()
			if err := wl.observe(before, after); err != nil {
				t.Errorf("arm %q is BLIND: %v", wl.name, err)
			}
			// And it must actually have committed, or "the counters moved" could be
			// setup residue rather than the unit's work.
			if after.Write.Commits <= before.Write.Commits {
				t.Errorf("arm %q committed nothing: %d -> %d",
					wl.name, before.Write.Commits, after.Write.Commits)
			}
		})
	}
}

// BenchmarkWriteContention reports commits/s, ns/commit and the scaling factor against
// the single-writer arm, for every contention workload and writer count.
//
// Read the header of this file before comparing two runs of it: interleaved, no -race,
// no competing load, or the numbers are void.
func BenchmarkWriteContention(b *testing.B) {
	for _, wl := range contentionWorkloads {
		b.Run(wl.name, func(b *testing.B) {
			var base float64
			for _, writers := range scalingWriters {
				b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
					g := lpg.New[string, float64](contentionAdjConfig())
					eng := cypher.NewEngine(g)
					ctx := context.Background()
					if wl.setup != nil {
						if err := wl.setup(ctx, eng, writers); err != nil {
							b.Fatalf("setup: %v", err)
						}
					}
					perWriter := (b.N + writers - 1) / writers

					b.ResetTimer()
					contentionRetries.Store(0)
					got, err := runArm(writers, perWriter, func(w, i int) error {
						return withRetry(func() error { return wl.unit(ctx, eng, w, i) })
					})
					b.StopTimer()
					if err != nil {
						b.Fatalf("writer failed: %v", err)
					}
					if got.commits == 0 {
						b.Fatal("no commits made")
					}
					cps := got.commitsPerSec()
					if writers == 1 {
						base = cps
					}
					b.ReportMetric(cps, "commits/s")
					b.ReportMetric(got.nsPerCommit(), "ns/commit")
					if base > 0 {
						b.ReportMetric(cps/base, "scaling")
					}
					// The conflict rate is a first-class number here: an arm whose
					// throughput collapses because it is retrying is a different
					// diagnosis from one bound on a lock, and only this separates them.
					b.ReportMetric(float64(contentionRetries.Load())/float64(got.commits), "retries/op")
				})
			}
		})
	}
}

// contentionAdjConfig matches the wiring newRig uses: directed so openCypher
// relationship semantics hold, multigraph so the edge arm can add parallel
// relationships between one pair without the engine refusing them.
func contentionAdjConfig() adjlist.Config {
	return adjlist.Config{Directed: true, Multigraph: true}
}

// TestLabelToggle_PerWriterSessionMakesProgress answers the question rmp #2368 was
// left on: is the label-toggle livelock a DEFECT, or the documented cross-statement
// boundary seen from the write side?
//
// The livelock is that a writer's retry conflicts with ITS OWN previous commit,
// because Graph.readTS returns the contiguous frontier and the retry's fresh snapshot
// is still behind that commit. cypher/session.go promises read-your-own-writes only
// WITHIN a Session, via a floor plus a wait; a bare Engine caller gets snapshot
// isolation alone. So a writer issuing consecutive autocommit statements through bare
// eng.RunInTx has no guarantee of observing its own predecessor — which is exactly
// what the conflict reports.
//
// If per-writer Sessions make progress, rmp #2368 is a benchmark error and the only
// open question is the API DEFAULT (rmp #2369). If they do NOT, the livelock is real
// and #2354's unconditional head test needs an exemption for a head this session has
// already committed.
//
// It lives in this file rather than its own because a separate probe file was silently
// excluded from this package's test binary (go test -list showed the function absent,
// with no build error) and the cause was never diagnosed.
func TestLabelToggle_PerWriterSessionMakesProgress(t *testing.T) {
	const writers, rounds, maxRetry = 8, 40, 64

	g := lpg.New[string, float64](contentionAdjConfig())
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	if err := seedPool(ctx, eng, writers); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sess := make([]*cypher.Session, writers)
	for w := range sess {
		sess[w] = eng.NewSession()
	}

	drain := func(r *cypher.Result, err error) error {
		if err != nil {
			return err
		}
		for r.Next() { //nolint:revive // draining is the point
		}
		re := r.Err()
		_ = r.Close()
		return re
	}

	var retries atomic.Int64
	before := g.MVCCStats().Write.Conflicts
	got, err := runArm(writers, rounds, func(w, i int) error {
		k := map[string]expr.Value{"k": expr.StringValue(contentionKey(w, i))}
		for _, q := range []string{
			`MATCH (n:Account {k: $k}) SET n:Hot`,
			`MATCH (n:Account {k: $k}) REMOVE n:Hot`,
		} {
			var last error
			ok := false
			for a := 0; a < maxRetry; a++ {
				last = drain(sess[w].RunInTx(ctx, q, k))
				if last == nil {
					ok = true
					break
				}
				if !errors.Is(last, mvcc.ErrSerializationConflict) {
					return last
				}
				retries.Add(1)
			}
			if !ok {
				return fmt.Errorf("%s: no progress in %d retries THROUGH A SESSION: %w", q, maxRetry, last)
			}
		}
		return nil
	})
	conflicts := g.MVCCStats().Write.Conflicts - before
	t.Logf("per-writer Sessions: commits=%d retries=%d conflicts=%d err=%v",
		got.commits, retries.Load(), conflicts, err)
	if err != nil {
		t.Fatalf("a writer made no progress even through its own Session, so rmp #2368 is "+
			"NOT merely the cross-statement contract: %v", err)
	}
}
