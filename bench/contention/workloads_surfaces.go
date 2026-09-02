package contention

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/internal/sim"
	"github.com/FlavioCFOliveira/GoGraph/search"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// maxWorkers bounds the per-worker resource tables the surface workloads use.
// It is larger than the published ladder's top rung (1024) so that a sweep
// narrowed with GOGRAPH_CONTENTION_LEVELS can still ask for more, and a
// workload that is asked for more than this fails loudly rather than sharing a
// connection between two workers and quietly measuring the wrong thing.
const maxWorkers = 4096

// errTooManyWorkers is returned by an Op whose worker index exceeds
// [maxWorkers].
var errTooManyWorkers = errors.New("contention: worker index exceeds maxWorkers")

// perWorker is a lock-free table of per-worker resources.
//
// Every slot is padded to two cache lines, because the point of these
// workloads is to find the module's false sharing, not to manufacture the
// harness's own. A slot is written and read only by its own worker, and the
// harness's WaitGroup establishes the happens-before edge that lets a teardown
// running after the workers read every slot; no atomic is needed for either.
type perWorker[T any] struct {
	slots []paddedSlot[T]
}

type paddedSlot[T any] struct {
	v T
	_ [16]uint64
}

func newPerWorker[T any]() *perWorker[T] {
	return &perWorker[T]{slots: make([]paddedSlot[T], maxWorkers)}
}

// get returns worker's slot, or an error when the sweep asked for more workers
// than the table holds.
func (p *perWorker[T]) get(worker int) (*T, error) {
	if worker < 0 || worker >= len(p.slots) {
		return nil, fmt.Errorf("%w: %d >= %d", errTooManyWorkers, worker, len(p.slots))
	}
	return &p.slots[worker].v, nil
}

// each calls fn for every slot. It is for teardown only, and is safe solely
// because the caller has already joined every worker.
func (p *perWorker[T]) each(fn func(*T)) {
	for i := range p.slots {
		fn(&p.slots[i].v)
	}
}

// seedCSR builds an immutable CSR ring-plus-chord graph of n nodes for the
// read-only search workloads. The chords give BFS and Dijkstra a frontier
// wider than one node, so the traversal actually costs something.
func seedCSR(n int) *csr.CSR[float64] {
	a := adjlist.New[int, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := range n {
		// The ring guarantees a single strongly connected component; the two
		// chords give an average out-degree of three.
		_ = a.AddEdge(i, (i+1)%n, 1)
		_ = a.AddEdge(i, (i+7)%n, 3)
		_ = a.AddEdge(i, (i*31+11)%n, 5)
	}
	return csr.BuildFromAdjList(a)
}

// walEngine wires the durable stack a write workload needs: graph, WAL,
// transactional store and Cypher engine, plus the store.DB that owns shutdown.
func walEngine(dir string, cpCfg *checkpoint.Config) (*cypher.Engine, func() error, error) {
	g, err := seedGraph(0)
	if err != nil {
		return nil, nil, err
	}
	wr, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		return nil, nil, fmt.Errorf("wal.Open: %w", err)
	}
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)

	opts := []store.Option{store.WithQuiesce(st.RunUnderCommitLock)}
	stop := func() {}
	if cpCfg != nil {
		// The unused mutex is the constructor's legacy parameter; the real
		// exclusion is the commit serialiser, exactly as store/example_test.go
		// and internal/sim/durable_scenarios.go wire it.
		var unusedMu sync.Mutex
		cp := checkpoint.New(*cpCfg, g, wr, &unusedMu,
			checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
			checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
			checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
		ctx, cancel := context.WithCancel(context.Background())
		cp.Start(ctx)
		stop = cancel
		opts = append(opts, store.WithCheckpointer(cp))
	}
	db := store.New(wr, opts...)
	return eng, func() error {
		err := db.Close()
		stop()
		return err
	}, nil
}

// surfaceWorkloads are the workloads added for rmp #2679 to reach the module
// surfaces the #2678 registry left untouched: search, search/centrality, the
// three secondary index kinds, the MVCC session API, bolt/server over the wire,
// and store/checkpoint.
//
// They are separated from the core registry only for readability; [All]
// concatenates the two and nothing distinguishes them at run time.
func surfaceWorkloads() []Workload {
	return append(append(append(
		searchWorkloads(),
		indexWorkloads()...),
		mvccWorkloads()...),
		serviceWorkloads()...)
}

// searchWorkloads drive search/ and search/centrality over a shared immutable
// CSR. The CSR itself is documented lock-free for readers, so anything these
// find is allocator, pool or scheduler contention rather than a data lock —
// which is precisely the claim worth testing.
func searchWorkloads() []Workload {
	const bfsNodes = 20000

	return []Workload{
		{
			Name:    "search-bfs-csr",
			Surface: "search, graph/csr",
			Ops:     300000,
			Setup: func(_ string) (Op, func() error, error) {
				c := seedCSR(bfsNodes)
				op := func(_ context.Context, worker, iter int) error {
					src := graph.NodeID((worker*7919 + iter) % bfsNodes)
					seen := 0
					search.BFS(c, src, func(_ graph.NodeID, _ int) bool {
						seen++
						return seen < 512 // bound the walk so every op costs the same
					})
					if seen == 0 {
						return fmt.Errorf("BFS(%d) visited nothing", src)
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
		{
			Name:    "search-sssp-shared",
			Surface: "search Dijkstra, its sync.Pool heap",
			Ops:     5000,
			Setup: func(_ string) (Op, func() error, error) {
				const n = 5000
				c := seedCSR(n)
				s, err := search.NewSSSP(c)
				if err != nil {
					return nil, nil, fmt.Errorf("new SSSP: %w", err)
				}
				op := func(_ context.Context, worker, iter int) error {
					src := graph.NodeID((worker*7919 + iter) % n)
					d, err := s.From(src)
					if err != nil {
						return err
					}
					if d.Source() != src {
						return fmt.Errorf("SSSP.From(%d) returned source %d", src, d.Source())
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
		{
			Name:    "centrality-pagerank",
			Surface: "search/centrality parallel SpMV worker pool",
			Ops:     12000,
			Setup: func(_ string) (Op, func() error, error) {
				// Above centrality's 2048-live-node parallel threshold, so each
				// call fans out to its own GOMAXPROCS workers: at level 64 and
				// above this is deliberate oversubscription of the scheduler.
				c := seedCSR(5000)
				opts := centrality.DefaultPageRankOptions()
				opts.MaxIterations = 12
				op := func(ctx context.Context, _, _ int) error {
					ranks, iters, err := centrality.PageRankCtx(ctx, c, opts)
					if err != nil {
						return err
					}
					if iters == 0 || len(ranks) == 0 {
						return fmt.Errorf("PageRank returned %d iterations, %d ranks", iters, len(ranks))
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
	}
}

// indexWorkloads drive the three secondary-index kinds. They are written to be
// COMPARABLE: the same read/write ratio and the same key space, so the only
// variable is how each index arbitrates concurrent access — a lock-free
// copy-on-write snapshot with one lock per KEY (btree, since task #2683; it
// held one global RWMutex before), 256 value-hashed shards (hash), 64 padded
// shards (count).
func indexWorkloads() []Workload {
	const (
		keys       = 100000
		writeEvery = 10 // one operation in ten mutates
	)

	return []Workload{
		{
			Name:    "index-btree-rw",
			Surface: "graph/index/btree (COW snapshot, per-key locks)",
			Ops:     24000000,
			Setup: func(_ string) (Op, func() error, error) {
				idx := btree.New[int64]()
				for i := range keys {
					idx.Insert(int64(i), graph.NodeID(uint64(i)))
				}
				op := func(_ context.Context, worker, iter int) error {
					k := int64((worker*7919 + iter) % keys)
					if (worker+iter)%writeEvery == 0 {
						idx.Insert(k, graph.NodeID(uint64(keys+worker)))
						return nil
					}
					if got := idx.Cardinality(k); got == 0 {
						return fmt.Errorf("btree.Cardinality(%d) == 0", k)
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
		{
			Name:    "index-hash-rw",
			Surface: "graph/index/hash (256 value-hashed shards)",
			Ops:     60000000,
			Setup: func(_ string) (Op, func() error, error) {
				idx := hash.New[int64]()
				for i := range keys {
					idx.Insert(int64(i), graph.NodeID(uint64(i)))
				}
				op := func(_ context.Context, worker, iter int) error {
					k := int64((worker*7919 + iter) % keys)
					if (worker+iter)%writeEvery == 0 {
						idx.Insert(k, graph.NodeID(uint64(keys+worker)))
						return nil
					}
					if got := idx.Cardinality(k); got == 0 {
						return fmt.Errorf("hash.Cardinality(%d) == 0", k)
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
		{
			Name:    "index-count-hot",
			Surface: "graph/index/count, ONE relationship type (single shard)",
			Ops:     160000000,
			Setup: func(_ string) (Op, func() error, error) {
				cs := count.New(0)
				cs.Apply(count.EDelta(1, 1))
				op := func(_ context.Context, worker, iter int) error {
					if (worker+iter)%writeEvery == 0 {
						cs.Apply(count.EDelta(1, 1))
						return nil
					}
					if cs.CountE(1) <= 0 {
						return errors.New("count.CountE(1) <= 0")
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
		{
			Name:    "index-count-spread",
			Surface: "graph/index/count, 4096 relationship types (64 shards)",
			Ops:     100000000,
			Setup: func(_ string) (Op, func() error, error) {
				const types = 4096
				cs := count.New(0)
				for rt := range uint32(types) {
					cs.Apply(count.EDelta(rt, 1))
				}
				op := func(_ context.Context, worker, iter int) error {
					rt := uint32((worker*7919 + iter) % types)
					if (worker+iter)%writeEvery == 0 {
						cs.Apply(count.EDelta(rt, 1))
						return nil
					}
					if cs.CountE(rt) <= 0 {
						return fmt.Errorf("count.CountE(%d) <= 0", rt)
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
	}
}

// mvccWorkloads drive the MVCC session and explicit-transaction APIs directly.
//
// internal/sim.RunMVCCSessions is NOT used: its own godoc states it "runs
// entirely on the calling goroutine and spawns none", so it interleaves
// transactions deterministically rather than running them in parallel. It is a
// determinism instrument, not a contention one.
func mvccWorkloads() []Workload {
	return []Workload{
		{
			Name:    "mvcc-session-write",
			Surface: "cypher.Session, graph/lpg session, graph/mvcc",
			Ops:     500000,
			Setup: func(_ string) (Op, func() error, error) {
				g, err := seedGraph(0)
				if err != nil {
					return nil, nil, err
				}
				eng := cypher.NewEngine(g)
				sessions := newPerWorker[*cypher.Session]()
				op := func(ctx context.Context, worker, iter int) error {
					slot, err := sessions.get(worker)
					if err != nil {
						return err
					}
					if *slot == nil {
						*slot = eng.NewSession()
					}
					id := int64(worker)<<40 + int64(iter)
					res, err := (*slot).RunInTx(ctx, "CREATE (:Account {id: $id})",
						map[string]expr.Value{"id": expr.IntegerValue(id)})
					if err != nil {
						return err
					}
					return res.Close()
				}
				return op, func() error { return nil }, nil
			},
		},
		{
			Name:    "mvcc-explicit-tx",
			Surface: "cypher.ExplicitTx, graph/mvcc, store/txn",
			Ops:     150000,
			Setup: func(_ string) (Op, func() error, error) {
				g, err := seedGraph(0)
				if err != nil {
					return nil, nil, err
				}
				eng := cypher.NewEngine(g)
				op := func(ctx context.Context, worker, iter int) error {
					tx, err := eng.BeginTx(ctx)
					if err != nil {
						return err
					}
					for k := range 3 {
						id := int64(worker)<<40 + int64(iter)*4 + int64(k)
						res, err := tx.Exec("CREATE (:Account {id: $id})",
							map[string]expr.Value{"id": expr.IntegerValue(id)})
						if err != nil {
							_ = tx.Rollback()
							return err
						}
						if err := res.Close(); err != nil {
							_ = tx.Rollback()
							return err
						}
					}
					return tx.Commit()
				}
				return op, func() error { return nil }, nil
			},
		},
	}
}

// serviceWorkloads drive the two surfaces that own their own goroutines: the
// Bolt server and the checkpointer.
func serviceWorkloads() []Workload {
	return []Workload{
		{
			Name:    "bolt-wire-read",
			Surface: "bolt/server, bolt/proto, cypher read path",
			Ops:     100000,
			Setup: func(_ string) (Op, func() error, error) {
				srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
				if err != nil {
					return nil, nil, fmt.Errorf("new sim server: %w", err)
				}
				clients := newPerWorker[*sim.WireClient]()
				op := func(ctx context.Context, worker, _ int) error {
					slot, err := clients.get(worker)
					if err != nil {
						return err
					}
					if *slot == nil {
						c, err := srv.Dial()
						if err != nil {
							return fmt.Errorf("dial: %w", err)
						}
						if err := c.Connect(ctx); err != nil {
							return fmt.Errorf("connect: %w", err)
						}
						*slot = c
					}
					if _, err := (*slot).Run("MATCH (n) RETURN count(n)", nil); err != nil {
						return err
					}
					_, _, err = (*slot).PullAll()
					return err
				}
				teardown := func() error {
					clients.each(func(c **sim.WireClient) {
						if *c != nil {
							_ = (*c).Close()
						}
					})
					return srv.Close()
				}
				return op, teardown, nil
			},
		},
		{
			Name:    "bolt-connect-churn",
			Surface: "bolt/server accept loop, session and tx registries",
			Ops:     30000,
			Setup: func(_ string) (Op, func() error, error) {
				srv, err := sim.NewSimServer(sim.SimEngineForServer(), clock.Real())
				if err != nil {
					return nil, nil, fmt.Errorf("new sim server: %w", err)
				}
				op := func(ctx context.Context, _, _ int) error {
					c, err := srv.Dial()
					if err != nil {
						return fmt.Errorf("dial: %w", err)
					}
					defer func() { _ = c.Close() }()
					if err := c.Connect(ctx); err != nil {
						return fmt.Errorf("connect: %w", err)
					}
					if _, err := c.Run("RETURN 1", nil); err != nil {
						return err
					}
					_, _, err = c.PullAll()
					return err
				}
				return op, srv.Close, nil
			},
		},
		{
			Name:    "store-checkpoint-write",
			Surface: "store/checkpoint, store/wal, store/txn under concurrent writers",
			Ops:     1000,
			Setup: func(dir string) (Op, func() error, error) {
				// MaxAge and Interval are deliberately tiny so the checkpointer
				// fires repeatedly DURING the measured window: a checkpointer
				// that never runs would make this workload a duplicate of
				// cypher-write-wal.
				cfg := checkpoint.Config{
					Dir:      dir,
					MaxAge:   20 * time.Millisecond,
					Interval: 5 * time.Millisecond,
				}
				eng, teardown, err := walEngine(dir, &cfg)
				if err != nil {
					return nil, nil, err
				}
				op := func(ctx context.Context, worker, iter int) error {
					id := int64(worker)<<40 + int64(iter)
					res, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
						map[string]expr.Value{"id": expr.IntegerValue(id)})
					if err != nil {
						return err
					}
					return res.Close()
				}
				return op, teardown, nil
			},
		},
	}
}
