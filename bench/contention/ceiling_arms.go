package contention

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/generation"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// # Ceiling arms
//
// A share is not a prize. Round 1 measured a site holding 98.67% of all mutex
// delay with about 4% of achievable throughput behind it, which is why this
// package now refuses to propose a fix for a site whose ceiling has not been
// measured first. A ceiling arm answers one question and only one:
//
//	if this shared structure cost its callers NOTHING to share, how much
//	faster would this workload go?
//
// # How the ceiling is built, and what it is not
//
// The arms here do not delete a lock — module code is not theirs to change, and
// a deleted lock would measure a program that does not exist. They REMOVE THE
// SHARING instead: the arm builds several independent copies of the fixture the
// base workload shares, and routes each worker to one of them. The code path
// through the module is byte-identical; only the number of goroutines meeting
// on one object changes.
//
// The replica count is [ceilingReplicas], which is GOMAXPROCS. That is a
// deliberate choice and it bounds what the number means. With GOMAXPROCS
// replicas, at most one goroutine per replica can be running at any instant, so
// the arm approximates "perfectly partitioned across the cores this machine
// has" — not "infinitely partitioned". Above GOMAXPROCS goroutines the arm's
// replicas are still time-shared, exactly as the hardware time-shares the
// cores, so the number stays a ceiling on what partitioning could buy HERE
// rather than an abstract upper bound.
//
// A ratio near 1.00x therefore says the sharing is not what limits the
// workload, and no amount of sharding will repay the effort. A large ratio says
// the sharing is the limit and how much is behind it.
//
// # They are not part of the sweep
//
// [All] does not return them, so a sweep does not walk them and the inventory's
// scaling table stays a table of things the module actually does. They are
// reachable by name through [ByName], which is how the ceiling probe's child
// processes address them.

// ceilingReplicas is how many independent copies of a shared fixture a ceiling
// arm builds. See the note above for why it is GOMAXPROCS.
func ceilingReplicas() int { return runtime.GOMAXPROCS(0) }

// ceilingSuffix is appended to a base workload's name to form its ceiling arm's
// name, so the pairing is legible in a probe's output without a lookup table.
const ceilingSuffix = "-ceiling"

// ceilingArms returns the ceiling arms, which are addressable by name but are
// deliberately absent from [All].
func ceilingArms() []Workload {
	return []Workload{
		cypherWriteMemCeiling(),
		cypherReadLabelSmallCeiling(),
		cypherMixedRWCeiling(),
		lpgNeighboursReadCeiling(),
		mvccSessionWriteCeiling(),
		indexHashRWCeiling(),
		indexBtreeRWCeiling(),
		indexCountHotCeiling(),
		indexManagerFanoutCeiling(),
		generationCeiling(),
		metricsCeiling(),
		dstConcurrentCeiling(),
		dstDiskCeiling(),
	}
}

// replicaSet holds the independent fixtures a ceiling arm routes workers to.
type replicaSet[T any] struct {
	items   []T
	release func(T) error
}

// pick returns the replica a given worker uses. Routing is by worker index, so
// one worker always meets the same replica and the arm measures partitioning
// rather than migration.
func (r *replicaSet[T]) pick(worker int) T { return r.items[worker%len(r.items)] }

// close releases every replica, joining the first error.
func (r *replicaSet[T]) close() error {
	if r.release == nil {
		return nil
	}
	var errs []error
	for _, it := range r.items {
		if err := r.release(it); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildReplicas constructs [ceilingReplicas] fixtures, releasing whatever it
// built if one of them fails so a partial arm never leaks.
func buildReplicas[T any](build func(i int) (T, error), release func(T) error) (*replicaSet[T], error) {
	n := ceilingReplicas()
	set := &replicaSet[T]{items: make([]T, 0, n), release: release}
	for i := range n {
		item, err := build(i)
		if err != nil {
			_ = set.close()
			return nil, err
		}
		set.items = append(set.items, item)
	}
	return set, nil
}

// ceilingArm assembles a ceiling arm from a base workload's identity and a
// per-replica fixture.
func ceilingArm[T any](
	baseName string,
	build func(i int, dir string) (T, error),
	release func(T) error,
	run func(ctx context.Context, t T, worker, iter int) error,
) Workload {
	base, ok := lookupBase(baseName)
	if !ok {
		// An arm naming a workload the registry no longer has is a defect, but
		// it is not one worth a panic: it surfaces as a failing Setup, which
		// the probe reports with the arm's name attached.
		// [TestCeilingArmsNameRealWorkloads] catches it before a probe does.
		return unresolvedArm(baseName)
	}
	return Workload{
		Name:    base.Name + ceilingSuffix,
		Surface: base.Surface + " [ceiling arm: fixture replicated per core]",
		Ops:     base.Ops,
		Setup: func(dir string) (Op, func() error, error) {
			set, err := buildReplicas(
				func(i int) (T, error) { return build(i, filepath.Join(dir, fmt.Sprintf("r%d", i))) },
				release)
			if err != nil {
				return nil, nil, err
			}
			op := func(ctx context.Context, worker, iter int) error {
				return run(ctx, set.pick(worker), worker, iter)
			}
			return op, set.close, nil
		},
	}
}

// unresolvedArm is what a ceiling arm becomes when the workload it names is no
// longer in the registry. It is not a panic: it surfaces as a failing Setup
// carrying the missing name, and [TestCeilingArmsNameRealWorkloads] catches it
// before any probe can.
func unresolvedArm(baseName string) Workload {
	return Workload{
		Name:    baseName + ceilingSuffix,
		Surface: "unresolved ceiling arm",
		Ops:     1,
		Setup: func(string) (Op, func() error, error) {
			return nil, nil, fmt.Errorf("contention: ceiling arm names unknown base workload %q", baseName)
		},
	}
}

// lookupBase finds a workload in [All] only. It deliberately does NOT consult
// [ceilingArms], because ceilingArms calls it: a lookup that searched both
// would recurse.
func lookupBase(name string) (Workload, bool) {
	for _, w := range All() {
		if w.Name == name {
			return w, true
		}
	}
	return Workload{}, false
}

// --- Cypher arms ---------------------------------------------------------

// cypherEngineReplica builds one private graph plus its engine.
func cypherEngineReplica(nodes int) func(int, string) (*cypher.Engine, error) {
	return func(_ int, _ string) (*cypher.Engine, error) {
		g, err := seedGraph(nodes)
		if err != nil {
			return nil, err
		}
		return cypher.NewEngine(g), nil
	}
}

func noRelease[T any](T) error { return nil }

func cypherWriteMemCeiling() Workload {
	return ceilingArm("cypher-write-mem", cypherEngineReplica(0), noRelease[*cypher.Engine],
		func(ctx context.Context, eng *cypher.Engine, worker, iter int) error {
			id := int64(worker)<<40 + int64(iter)
			_, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
				map[string]expr.Value{"id": expr.IntegerValue(id)})
			return err
		})
}

func cypherReadLabelSmallCeiling() Workload {
	return ceilingArm("cypher-read-label-small", cypherEngineReplica(2000), noRelease[*cypher.Engine],
		func(ctx context.Context, eng *cypher.Engine, _, _ int) error {
			_, err := eng.Run(ctx, "MATCH (n:N) RETURN count(n)", nil)
			return err
		})
}

func cypherMixedRWCeiling() Workload {
	return ceilingArm("cypher-mixed-rw", cypherEngineReplica(2000), noRelease[*cypher.Engine],
		func(ctx context.Context, eng *cypher.Engine, worker, iter int) error {
			if mixedIsWrite(worker, iter) {
				id := int64(worker)<<40 + int64(iter)
				_, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
					map[string]expr.Value{"id": expr.IntegerValue(id)})
				return err
			}
			_, err := eng.Run(ctx, "MATCH (n:N) RETURN count(n)", nil)
			return err
		})
}

func mvccSessionWriteCeiling() Workload {
	// One session PER WORKER is what the base workload already does, so the
	// only variable this arm changes is the engine each session runs against.
	type replica struct {
		eng      *cypher.Engine
		sessions *perWorker[*cypher.Session]
	}
	return ceilingArm("mvcc-session-write",
		func(_ int, _ string) (*replica, error) {
			g, err := seedGraph(0)
			if err != nil {
				return nil, err
			}
			return &replica{eng: cypher.NewEngine(g), sessions: newPerWorker[*cypher.Session]()}, nil
		},
		noRelease[*replica],
		func(ctx context.Context, r *replica, worker, iter int) error {
			slot, err := r.sessions.get(worker)
			if err != nil {
				return err
			}
			if *slot == nil {
				*slot = r.eng.NewSession()
			}
			id := int64(worker)<<40 + int64(iter)
			res, err := (*slot).RunInTx(ctx, "CREATE (:Account {id: $id})",
				map[string]expr.Value{"id": expr.IntegerValue(id)})
			if err != nil {
				return err
			}
			return res.Close()
		})
}

func lpgNeighboursReadCeiling() Workload {
	const n = 20000
	return ceilingArm("lpg-neighbours-read",
		func(_ int, _ string) (*lpg.Graph[string, float64], error) { return seedGraph(n) },
		noRelease[*lpg.Graph[string, float64]],
		func(_ context.Context, g *lpg.Graph[string, float64], worker, iter int) error {
			id := fmt.Sprintf("n%d", (worker*7919+iter)%n)
			if _, ok := g.OutDegree(id); !ok {
				return fmt.Errorf("OutDegree(%s): node absent", id)
			}
			g.NodeLabels(id)
			if _, ok := g.GetNodeProperty(id, "v"); !ok {
				return fmt.Errorf("GetNodeProperty(%s): absent", id)
			}
			return nil
		})
}

// --- Index arms ----------------------------------------------------------

const (
	ceilingIndexKeys  = 100000
	ceilingWriteEvery = 10
)

func indexHashRWCeiling() Workload {
	return ceilingArm("index-hash-rw",
		func(_ int, _ string) (*hash.Index[int64], error) {
			idx := hash.New[int64]()
			for i := range ceilingIndexKeys {
				idx.Insert(int64(i), graph.NodeID(uint64(i))) // G115: i is a bounded loop index
			}
			return idx, nil
		},
		noRelease[*hash.Index[int64]],
		func(_ context.Context, idx *hash.Index[int64], worker, iter int) error {
			k := int64((worker*7919 + iter) % ceilingIndexKeys)
			if (worker+iter)%ceilingWriteEvery == 0 {
				idx.Insert(k, graph.NodeID(uint64(ceilingIndexKeys+worker))) //nolint:gosec // G115: both terms are non-negative
				return nil
			}
			if got := idx.Cardinality(k); got == 0 {
				return fmt.Errorf("hash.Cardinality(%d) == 0", k)
			}
			return nil
		})
}

func indexBtreeRWCeiling() Workload {
	return ceilingArm("index-btree-rw",
		func(_ int, _ string) (*btree.Index[int64], error) {
			idx := btree.New[int64]()
			for i := range ceilingIndexKeys {
				idx.Insert(int64(i), graph.NodeID(uint64(i))) // G115: i is a bounded loop index
			}
			return idx, nil
		},
		noRelease[*btree.Index[int64]],
		func(_ context.Context, idx *btree.Index[int64], worker, iter int) error {
			k := int64((worker*7919 + iter) % ceilingIndexKeys)
			if (worker+iter)%ceilingWriteEvery == 0 {
				idx.Insert(k, graph.NodeID(uint64(ceilingIndexKeys+worker))) //nolint:gosec // G115: both terms are non-negative
				return nil
			}
			if got := idx.Cardinality(k); got == 0 {
				return fmt.Errorf("btree.Cardinality(%d) == 0", k)
			}
			return nil
		})
}

func indexCountHotCeiling() Workload {
	return ceilingArm("index-count-hot",
		func(_ int, _ string) (*count.Store, error) {
			cs := count.New(0)
			cs.Apply(count.EDelta(1, 1))
			return cs, nil
		},
		noRelease[*count.Store],
		func(_ context.Context, cs *count.Store, worker, iter int) error {
			if (worker+iter)%ceilingWriteEvery == 0 {
				cs.Apply(count.EDelta(1, 1))
				return nil
			}
			if cs.CountE(1) <= 0 {
				return errors.New("count.CountE(1) <= 0")
			}
			return nil
		})
}

func indexManagerFanoutCeiling() Workload {
	return ceilingArm("index-manager-fanout",
		func(_ int, _ string) (*index.Manager, error) {
			m := index.NewManager()
			for i := range indexManagerSubscribers {
				if err := m.CreateIndex(fmt.Sprintf("label-%d", i), label.NewNodeIndex()); err != nil {
					return nil, fmt.Errorf("CreateIndex: %w", err)
				}
			}
			return m, nil
		},
		noRelease[*index.Manager],
		func(_ context.Context, m *index.Manager, worker, iter int) error {
			return indexManagerOp(m, worker, iter)
		})
}

// --- Round-2 surface arms ------------------------------------------------

func generationCeiling() Workload {
	type replica struct {
		p *generation.Publisher[float64]
		c *csr.CSR[float64]
	}
	return ceilingArm("generation-publish-read",
		func(_ int, _ string) (*replica, error) {
			c := seedCSR(generationCSRNodes)
			return &replica{p: generation.New(c), c: c}, nil
		},
		func(r *replica) error { r.p.Close(); return nil },
		func(_ context.Context, r *replica, worker, iter int) error {
			return generationOp(r.p, r.c, worker, iter)
		})
}

func metricsCeiling() Workload {
	base, ok := lookupBase("metrics-emit")
	if !ok {
		return unresolvedArm("metrics-emit")
	}
	// This arm is written out rather than assembled from [ceilingArm] because
	// its shared resource is PROCESS-GLOBAL: the backend is installed once per
	// window, not once per replica, and must be restored once at teardown.
	//
	// The BACKEND therefore stays shared, deliberately. A per-replica registry
	// would measure a program the module cannot run, since internal/metrics
	// holds exactly one. What the arm unshares is the metric NAME, which is
	// what selects the counter, so each replica's operations land on their own
	// counter, gauge and histogram inside the one shared registry. That
	// separates the counter's cache-line coherence cost, which sharding a name
	// could fix, from the registry's lookup cost, which it could not.
	//
	// Names are precomputed: fmt.Sprintf inside the measured operation would
	// put an allocation and a format parse in the hot loop and the arm would
	// then be measuring strconv.
	suffixes := make([]string, ceilingReplicas())
	for i := range suffixes {
		suffixes[i] = fmt.Sprintf(".r%d", i)
	}
	return Workload{
		Name:    base.Name + ceilingSuffix,
		Surface: base.Surface + " [ceiling arm: metric names replicated per core]",
		Ops:     base.Ops,
		Setup: func(_ string) (Op, func() error, error) {
			metrics.SetBackend(prometheus.New())
			op := func(_ context.Context, worker, iter int) error {
				metricsOp(suffixes[worker%len(suffixes)], worker, iter)
				return nil
			}
			teardown := func() error {
				metrics.SetBackend(nil)
				return nil
			}
			return op, teardown, nil
		},
	}
}

func dstConcurrentCeiling() Workload {
	return ceilingArm("dst-concurrent-bolt",
		func(_ int, _ string) (*sim.SimServer, error) {
			srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
			if err != nil {
				return nil, fmt.Errorf("new sim server: %w", err)
			}
			return srv, nil
		},
		func(srv *sim.SimServer) error { return srv.Close() },
		dstConcurrentOp)
}

func dstDiskCeiling() Workload {
	type replica struct {
		eng *cypher.Engine
		db  *store.DB
	}
	return ceilingArm(dstDiskCleanName,
		func(i int, _ string) (*replica, error) {
			g, err := seedGraph(0)
			if err != nil {
				return nil, err
			}
			disk := sim.NewSimDisk(sim.NewSeed(dstDiskFaultSeed+uint64(i)), 0) //nolint:gosec // G115: i is a bounded loop index
			h, err := disk.OpenFile(simWALName, os.O_CREATE|os.O_RDWR)
			if err != nil {
				return nil, fmt.Errorf("sim disk open: %w", err)
			}
			wr, err := wal.OpenWith(h)
			if err != nil {
				return nil, fmt.Errorf("wal.OpenWith: %w", err)
			}
			st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			})
			return &replica{
				eng: cypher.NewEngineWithStore(st),
				db:  store.New(wr, store.WithQuiesce(st.RunUnderCommitLock)),
			}, nil
		},
		func(r *replica) error { return r.db.Close() },
		func(ctx context.Context, r *replica, worker, iter int) error {
			id := int64(worker)<<40 + int64(iter)
			res, err := r.eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
				map[string]expr.Value{"id": expr.IntegerValue(id)})
			if err != nil {
				return err
			}
			return res.Close()
		})
}
