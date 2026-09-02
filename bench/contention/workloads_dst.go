package contention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// dstWorkloads reach the deterministic-simulation drivers themselves, which
// rmp #2679 recorded as NOT reached: "the DST's fault-injection and crash paths
// are not represented in this inventory".
//
// Two of the DST's three concurrency-bearing entry points are driven here; the
// third is recorded as unreachable rather than approximated.
//
//   - [sim.RunConcurrent] IS driven ([dstConcurrentWorkload]). It is the DST's
//     real multi-connection driver: one goroutine per connection over the real
//     Bolt wire, a seeded role population, the transaction ledger and the
//     during-run isolation oracles.
//   - The fault-injection path IS driven ([dstDiskWorkload]), through
//     [sim.SimDisk] under a live WAL rather than through the DST's own scenario
//     runner; see that workload for why that route was taken, and for the
//     measurement that forced it to be published as a CONTROLLED PAIR.
//   - [sim.RunMVCCSessions] is NOT driven, and that is not an omission. Its own
//     godoc states it "runs entirely on the calling goroutine and spawns none",
//     so it interleaves transactions deterministically rather than running them
//     in parallel: it is a determinism instrument, not a contention one. The
//     same reasoning is recorded on [mvccWorkloads].
func dstWorkloads() []Workload {
	return []Workload{
		dstConcurrentWorkload(),
		dstDiskWorkload(dstDiskCleanName, 0),
		dstDiskWorkload(dstDiskFaultName, dstDiskFaultRate),
		dstMVCCSessionsWorkload(),
	}
}

// dstConcurrentOpsPerConn is how many seed-derived operations one
// dst-concurrent-bolt operation asks its single connection to perform.
//
// It is small on purpose. Every [sim.RunConcurrent] call carries a FIXED
// overhead the harness cannot amortise away — the Bolt parameter type matrix
// probed before any connection spawns, the transaction-quiescence verification,
// and the node-count oracle, each over its own fresh connection. A large
// OpsPerConn would bury that overhead and turn the workload into a measurement
// of the writer role alone; a small one keeps the DRIVER, which is what this
// workload exists to reach, a visible fraction of every operation.
const dstConcurrentOpsPerConn = 4

// dstConcurrentWorkload drives [sim.RunConcurrent], the DST's own concurrent
// multi-connection driver, against one shared [sim.SimServer].
//
// # Why one connection per operation, and not one run per operation
//
// RunConcurrent spawns cfg.Connections goroutines of its own. Letting each of
// the harness's `level` workers ask for N connections would make the real
// concurrency level×N, so the ladder would no longer be the single knob and the
// scaling column would compare runs at different true concurrencies. Pinning
// Connections to 1 keeps `level` the only concurrency variable, exactly as every
// other workload in the registry does.
//
// The role MIX is still realised, across operations rather than within one run:
// every operation draws its own seed, so the population of writer / reader /
// overload / transactional / batch / RYOW roles is sampled over the whole
// window instead of inside a single call.
//
// # What its oracle can and cannot assert
//
// [sim.ConcurrentResult.Consistent] is deliberately NOT checked. It compares the
// engine's live node count against THIS call's acknowledged creates, and every
// other worker is committing to the same shared server at the same time, so it
// is false by construction here and asserting it would be a lie. The two
// oracles that survive the sharing are checked instead: a recovered panic in a
// connection goroutine, and an unexpected transport error. Both are properties
// of one call's own goroutines and neither is perturbed by a concurrent caller.
func dstConcurrentWorkload() Workload {
	return Workload{
		Name:    "dst-concurrent-bolt",
		Surface: "internal/sim.RunConcurrent, bolt/server, cypher, graph/lpg",
		// Measured, not guessed: at level 1 one operation costs ~2.6 ms
		// (384/s), so 2000 gives a level-1 window of ~5 s and every rung above
		// it a shorter one. A larger count made the level-1 window 15.6 s and
		// the whole ladder unaffordable.
		Ops: 2000,
		Setup: func(_ string) (Op, func() error, error) {
			srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
			if err != nil {
				return nil, nil, fmt.Errorf("new sim server: %w", err)
			}
			op := func(ctx context.Context, worker, iter int) error {
				return dstConcurrentOp(ctx, srv, worker, iter)
			}
			return op, srv.Close, nil
		},
	}
}

// dstConcurrentOp is one dst-concurrent-bolt operation. It is a named function
// so the ceiling arm runs the IDENTICAL body against an unshared server; two
// copies of it would let the arms drift and the ratio between them would then
// measure the drift rather than the sharing.
func dstConcurrentOp(ctx context.Context, srv *sim.SimServer, worker, iter int) error {
	res, err := sim.RunConcurrent(ctx, srv, sim.ConcurrentConfig{
		// Disjoint seed per operation: worker in the high bits, iteration in
		// the low, so no two operations replay the same role and op stream.
		Seed:        uint64(worker)<<40 | uint64(iter), //nolint:gosec // G115: both are non-negative loop indices
		Connections: 1,
		OpsPerConn:  dstConcurrentOpsPerConn,
	})
	if err != nil {
		return fmt.Errorf("RunConcurrent: %w", err)
	}
	if res.Panics != 0 {
		return fmt.Errorf("RunConcurrent: %d recovered panic(s)", res.Panics)
	}
	if res.TransportErrors != 0 {
		return fmt.Errorf("RunConcurrent: %d transport error(s)", res.TransportErrors)
	}
	return nil
}

// dstDiskFaultRate is the per-Sync / per-written-sector fault probability the
// faulted arm runs at, and dstDiskFaultSeed fixes the sequence those decisions
// are drawn from. [sim.SimDisk] draws every fault decision from its seed, so a
// fixed value is what makes the arm reproducible across runs — which a fault
// workload needs more than a clean one, since an unseeded stream would put a
// different number of faults in every window and make two runs incomparable.
const (
	dstDiskFaultRate = 1.0 / 512.0
	dstDiskFaultSeed = 0x5EED_FA17
)

// The two arms' names. They are constants because the non-vacuity test asserts
// against the pair and a typo would silently test one arm twice.
const (
	dstDiskCleanName = "dst-disk-wal"
	dstDiskFaultName = "dst-disk-fault-wal"
)

// simWALName is the in-memory disk key the WAL file lives under.
const simWALName = "wal.log"

// dstDiskWorkload drives concurrent durable Cypher writes over a WAL whose file
// is a [sim.SimDisk] handle, at the given per-Sync fault probability.
//
// It is registered TWICE, at faultRate 0 and at [dstDiskFaultRate], and the two
// rows are a controlled pair: identical fixture, identical operation, one
// variable. Reading either alone would be a mistake.
//
// # Why this route, and not the DST's own scenario runner
//
// The DST's full durable stack is opened by [sim.OpenSimStore], which is
// exported but takes the UNEXPORTED type simStoreConfig, whose only constructor
// is unexported too. It is therefore not constructible from this package, and
// making it so would mean changing internal/sim — outside the scope of the task
// that added this file. The route taken instead uses only exported API:
// [sim.NewSimDisk], [sim.SimDisk.OpenFile] (whose *sim.SimFileHandle satisfies
// the exported [wal.WALFile]) and [wal.OpenWith]. Every WAL append, every fsync
// and every injected fault therefore crosses the simulated disk's single global
// mutex, under the real transactional store and the real Cypher engine.
//
// # What the faulted arm actually measures, which is not what it sounds like
//
// Measured, at 5000 sequential commits: at faultRate 0 every commit succeeds and
// the disk records 5000 syncs. At 1/512, 559 commits succeed and the remaining
// 4441 fail — and the disk records 561 syncs in total. The first failure reads
// "wal: durability failed; the un-synced suffix was discarded and this writer is
// poisoned". The writer FAIL-STOPS on the first injected fsync fault and never
// syncs again, exactly as the reliability mandate requires of it.
//
// So there is no steady state of "durable writes with occasional faults" to
// measure. The faulted arm measures a short clean prefix and then the
// fail-stop path under concurrency, which is a real question — an operator
// whose disk has just failed has every writer on that branch at once — but it
// is NOT a throughput number and must never be quoted as one.
//
// # Injected faults are not harness errors
//
// An operation whose commit failed because the simulated disk refused the fsync,
// or because a previous fault had already poisoned the writer, is the fault
// injector working rather than the module failing, so it is not counted in the
// metrics' error column. Anything else is. The distinction is made by
// [errors.Is] against [sim.ErrSimFault], never by matching a message.
func dstDiskWorkload(name string, faultRate float64) Workload {
	w, _ := dstDiskWorkloadWithFaultCount(name, faultRate)
	return w
}

// dstDiskWorkloadWithFaultCount returns a simulated-disk arm together with the
// counter its operations record injected faults in.
//
// The counter exists because the workload SWALLOWS the faults it provokes, so
// the metrics' error column reads 0 whether the injector fired on every commit
// or not at all. That makes the sweep row alone incapable of proving either arm
// is what it claims, and an arm that cannot fail proves nothing. The counter is
// what [TestDiskArmsAreAControlledPair] asserts against, in both directions:
// the faulted arm must fault, and the clean arm must not.
//
// The counter is reset by Setup, so one process may build the workload once and
// observe several windows without inheriting the previous window's tally.
func dstDiskWorkloadWithFaultCount(name string, faultRate float64) (Workload, *atomic.Int64) {
	var faults atomic.Int64
	return Workload{
		Name: name,
		Surface: "internal/sim.SimDisk, store/wal, store/txn, cypher write path" +
			faultSurfaceSuffix(faultRate),
		// The simulated disk's fsync is a memory write, so this path is two
		// orders of magnitude faster than the real-WAL workload beside it:
		// measured 163k commits/s at level 1, where cypher-write-wal's 4000
		// operations would have closed the window in 24 ms.
		Ops: 200000,
		Setup: func(_ string) (Op, func() error, error) {
			g, err := seedGraph(0)
			if err != nil {
				return nil, nil, err
			}
			faults.Store(0)
			disk := sim.NewSimDisk(sim.NewSeed(dstDiskFaultSeed), faultRate)
			// The disk is in memory, so the name is a key rather than a path,
			// and it is deliberately ROOT-LEVEL: SimDisk treats a root-level
			// name as durably linked on creation, which is the model the
			// WAL-only crash path uses. It takes no directory from the
			// harness for the same reason.
			h, err := disk.OpenFile(simWALName, os.O_CREATE|os.O_RDWR)
			if err != nil {
				return nil, nil, fmt.Errorf("sim disk open: %w", err)
			}
			wr, err := wal.OpenWith(h)
			if err != nil {
				return nil, nil, fmt.Errorf("wal.OpenWith: %w", err)
			}
			st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			})
			db := store.New(wr, store.WithQuiesce(st.RunUnderCommitLock))
			eng := cypher.NewEngineWithStore(st)

			op := func(ctx context.Context, worker, iter int) error {
				id := int64(worker)<<40 + int64(iter)
				res, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
					map[string]expr.Value{"id": expr.IntegerValue(id)})
				if err != nil {
					if errors.Is(err, sim.ErrSimFault) {
						faults.Add(1)
						return nil
					}
					return err
				}
				return res.Close()
			}
			teardown := func() error {
				if err := db.Close(); err != nil && !errors.Is(err, sim.ErrSimFault) {
					return err
				}
				return nil
			}
			return op, teardown, nil
		},
	}, &faults
}

// faultSurfaceSuffix labels an arm's Surface with its fault rate, so the sweep's
// surface column distinguishes the two rows instead of showing them identical.
func faultSurfaceSuffix(rate float64) string {
	if rate == 0 {
		return " (no faults: the control arm)"
	}
	return fmt.Sprintf(" (fault rate %g: the fail-stop arm)", rate)
}

// The shape of one dst-mvcc-sessions operation.
//
// Ticks is 60 and Sessions 4. The cost is markedly superlinear in Ticks —
// measured at 1.68 ms for 20 ticks, 7.88 ms for 60 and 48.06 ms for 200, so
// 3.3x the ticks costs 6.1x the time — because the oracle folds a growing
// committed model. Sixty keeps one operation at about 7.9 ms, which is short
// enough that the ladder is affordable and long enough that a run contains
// several complete transactions rather than one.
const (
	dstMVCCTicks    = 60
	dstMVCCSessions = 4
)

// dstMVCCSessionsWorkload drives [sim.RunMVCCSessions], the DST's multi-session
// MVCC scheduler, over its own SimDisk-backed store.
//
// # It shares nothing, and that is the point
//
// Every other workload in this registry puts `level` goroutines onto ONE shared
// fixture, so its scaling column reports how that fixture behaves under
// sharing. This one cannot: [sim.RunMVCCSessions] builds its own [sim.SimDisk],
// its own store and its own engine on every call, and the mode is
// single-goroutine internally, so N concurrent operations are N independent
// simulations that touch no common object.
//
// That makes it useless as a lock probe and valuable as a different one. The
// reliability mandate forbids hidden global state; N shares-nothing simulations
// must therefore scale with the cores available to them, and a curve that falls
// below 1.000x here cannot be explained by any lock in the module — it would
// mean two independent stores are meeting somewhere they were never meant to.
// Read its scaling column as a HIDDEN-GLOBAL-STATE probe, never as a contention
// ranking, and note that it has no ceiling arm for the same reason: there is
// nothing shared to unshare.
//
// # What its oracles assert
//
// Both of the run's own adjudications are checked, and both survive the
// independence: Violations (an invariant broken) and FoldErrors (the engine
// acknowledged a COMMIT whose effects no longer fold over the committed model,
// which the DST documents as an isolation finding or a harness bug, never
// noise). Neither is perturbed by a concurrent caller, because no concurrent
// caller can see this operation's store.
//
// Crash injection is left at its zero value. The fault-injection arm of this
// package is [dstDiskFaultName], whose one variable against its clean twin is
// the fault rate; turning crashes on here as well would put two variables in
// one row.
func dstMVCCSessionsWorkload() Workload {
	return Workload{
		Name: "dst-mvcc-sessions",
		Surface: "internal/sim.RunMVCCSessions, cypher.Session, graph/mvcc, " +
			"store/txn, store/wal over SimDisk",
		// Measured: 127 operations/s at level 1 at these settings, so 300
		// gives a level-1 window of about 2.4 s, in line with the rest of the
		// registry.
		Ops: 300,
		// There is no fixture to build: RunMVCCSessions constructs its own
		// disk, store and engine per call, so the operation IS the workload
		// and there is nothing to tear down.
		Setup: func(_ string) (Op, func() error, error) {
			return dstMVCCSessionsOp, func() error { return nil }, nil
		},
	}
}

// dstMVCCSessionsOp is one dst-mvcc-sessions operation. It is a package-level
// function rather than a closure because it captures nothing.
func dstMVCCSessionsOp(ctx context.Context, worker, iter int) error {
	res, err := sim.RunMVCCSessions(ctx, sim.MVCCSessionsConfig{
		// Disjoint seed per operation, worker in the high bits and iteration
		// in the low, so no two operations replay the same schedule.
		Seed:     uint64(worker)<<40 | uint64(iter), //nolint:gosec // G115: both are non-negative loop indices
		Ticks:    dstMVCCTicks,
		Sessions: dstMVCCSessions,
	})
	if err != nil {
		return fmt.Errorf("RunMVCCSessions: %w", err)
	}
	if n := len(res.Violations); n != 0 {
		return fmt.Errorf("RunMVCCSessions: %d invariant violation(s): %v", n, res.Violations[0])
	}
	if n := len(res.FoldErrors); n != 0 {
		return fmt.Errorf("RunMVCCSessions: %d oracle fold error(s): %s", n, res.FoldErrors[0])
	}
	return nil
}
