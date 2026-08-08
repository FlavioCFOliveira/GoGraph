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
//     The anomaly is therefore unexplained and the ratio must not be quoted. Next
//     candidate, untested: the VACUUM. Total write volume grows with the writer count
//     (640k updates at 32 writers against 20k at one), so the reclaim threshold may
//     only be reached in the busy arms, leaving the single-writer arm walking chains
//     nobody has swept. MVCCStats.ChainDepth and VacuumStats would settle it. Until
//     something is MEASURED, "more writers is faster per commit" is an observation
//     about this fixture and not about the engine.
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
func contentionKey(w, i int) int64 { return int64(w)<<40 | int64(i%contentionPool) }

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
	if err := runOne(ctx, eng, `CREATE INDEX acct_id FOR (n:Account) ON (n.id)`, nil); err != nil {
		return fmt.Errorf("seed index: %w", err)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < contentionPool; i++ {
			if err := runOne(ctx, eng, `CREATE (n:Account {id: $id, v: 0})`,
				map[string]expr.Value{"id": expr.IntegerValue(contentionKey(w, i))}); err != nil {
				return fmt.Errorf("seed w=%d i=%d: %w", w, i, err)
			}
		}
	}
	return nil
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
			return runOne(ctx, eng, `MATCH (n:Account {id: $id}) SET n.v = $v`,
				map[string]expr.Value{
					"id": expr.IntegerValue(contentionKey(w, i)),
					"v":  expr.IntegerValue(int64(i)),
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
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			id := map[string]expr.Value{"id": expr.IntegerValue(contentionKey(w, i))}
			if err := runOne(ctx, eng, `MATCH (n:Account {id: $id}) SET n:Hot`, id); err != nil {
				return err
			}
			return runOne(ctx, eng, `MATCH (n:Account {id: $id}) REMOVE n:Hot`, id)
		},
		observe: func(b, a lpg.MVCCStats) error { return grew("LabelDeltas", b.LabelDeltas, a.LabelDeltas) },
	},
	{
		// ADJACENCY, the per-edge SIDE STORES and the COUNT STORE — none of which a
		// node-only create touches at all.
		name:  "create-edge",
		setup: seedPool,
		unit: func(ctx context.Context, eng *cypher.Engine, w, i int) error {
			return runOne(ctx, eng,
				`MATCH (a:Account {id: $a}), (b:Account {id: $b}) CREATE (a)-[:PAYS {amt: $amt}]->(b)`,
				map[string]expr.Value{
					"a":   expr.IntegerValue(contentionKey(w, i)),
					"b":   expr.IntegerValue(contentionKey(w, i+1)),
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
				return runOne(ctx, eng, `CREATE (n:Account {id: $id})`,
					map[string]expr.Value{"id": expr.IntegerValue(int64(w)<<40 | int64(contentionPool+i))})
			case 1:
				return runOne(ctx, eng, `MATCH (n:Account {id: $id}) SET n.v = $v`,
					map[string]expr.Value{
						"id": expr.IntegerValue(contentionKey(w, i)),
						"v":  expr.IntegerValue(int64(i)),
					})
			default:
				return runOne(ctx, eng,
					`MATCH (a:Account {id: $a}), (b:Account {id: $b}) CREATE (a)-[:PAYS]->(b)`,
					map[string]expr.Value{
						"a": expr.IntegerValue(contentionKey(w, i)),
						"b": expr.IntegerValue(contentionKey(w, i+1)),
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
