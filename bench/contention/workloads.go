package contention

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// seedGraph builds a directed multigraph of n labelled nodes, each carrying an
// Int64 property "v" and wired to its successor. Labels exercise the label
// registry, properties the property-key registry, and the edges give the
// traversal workloads something to walk.
//
// It is a multigraph so the engine does not emit its non-multigraph warning,
// matching bench/mvccwrite's rig.
func seedGraph(n int) (*lpg.Graph[string, float64], error) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.AddNode(id); err != nil {
			return nil, fmt.Errorf("AddNode %s: %w", id, err)
		}
		if err := g.SetNodeLabel(id, "N"); err != nil {
			return nil, fmt.Errorf("SetNodeLabel %s: %w", id, err)
		}
		if err := g.SetNodeProperty(id, "v", lpg.Int64Value(int64(i))); err != nil {
			return nil, fmt.Errorf("SetNodeProperty %s: %w", id, err)
		}
	}
	for i := 0; i+1 < n; i++ {
		if err := g.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), 1); err != nil {
			return nil, fmt.Errorf("AddEdge %d: %w", i, err)
		}
	}
	return g, nil
}

// readWorkload builds a read-only Cypher workload over a graph of n nodes.
func readWorkload(name, surface string, n, ops int, query string) Workload {
	return Workload{
		Name:    name,
		Surface: surface,
		Ops:     ops,
		Setup: func(_ string) (Op, func() error, error) {
			g, err := seedGraph(n)
			if err != nil {
				return nil, nil, err
			}
			eng := cypher.NewEngine(g)
			op := func(ctx context.Context, _, _ int) error {
				_, err := eng.Run(ctx, query, nil)
				return err
			}
			return op, func() error { return nil }, nil
		},
	}
}

// All is the registry of workloads the sweep drives. Each names the module
// surface it is meant to reach, so a sweep can state its coverage honestly
// rather than implying it touched everything.
//
// Op counts are sized so that every measured window lasts of the order of a
// second at the SLOWEST rung of the ladder. The first counts committed with
// this package were far smaller, and the sweep for rmp #2679 measured what that
// cost: at level 8 cypher-read-label-small closed in 18 ms, cypher-write-mem in
// 30 ms and lpg-neighbours-read in 13 ms. In an 18 ms window the one-shot plan
// compile is 30.51% of all blocked nanoseconds, so the profile ranked a
// start-up cost above every steady-state lock; and because a fixed cold cost
// falls on every rung alike, it dragged scaling_vs_1 towards 1.0 and made the
// module look as though it scaled better than it does.
//
// All is safe for concurrent use: every call builds a fresh slice of fresh
// [Workload] values, so no caller can mutate the registry another caller sees.
func All() []Workload {
	all := coreWorkloads()
	all = append(all, surfaceWorkloads()...)
	all = append(all, dstWorkloads()...)
	all = append(all, unreachedWorkloads()...)
	all = append(all, boltWorkloads()...)
	return all
}

// coreWorkloads is the registry as it stood at rmp #2678: Cypher read and
// write paths, a mixed read/write population, and the raw graph API.
func coreWorkloads() []Workload {
	return []Workload{
		// --- Cypher read paths -------------------------------------------
		// Small graph: below the parallel-scan threshold, so every query walks
		// the serial path and hammers the label/property registries. This is
		// the shape most likely to expose registry-level shared state.
		readWorkload("cypher-read-label-small", "cypher, cypher/exec, graph/lpg",
			2000, 800000, "MATCH (n:N) RETURN count(n)"),

		// Large graph: above the 50k parallel-scan threshold, so the parallel
		// governor and its worker pool are in play — a scheduler-shaped
		// contention surface rather than a registry-shaped one.
		readWorkload("cypher-read-scan-large", "cypher/exec parallel scan, graph/lpg",
			60000, 2000, "MATCH (n) WHERE n.v >= 0 RETURN count(n)"),

		// Property projection: materialises a value per row, exercising the
		// property store and the pooled buffers on the read path.
		readWorkload("cypher-read-project", "cypher/exec, graph/lpg property store",
			2000, 20000, "MATCH (n) WHERE n.v >= 0 RETURN n.v"),

		// --- Cypher write paths ------------------------------------------
		// In-memory autocommit writes. bench/mvccwrite names two suspects on
		// this path at an older head: cypher.Engine.writeMu and
		// lpg.Graph.visMu. Whether either still serialises at HEAD is exactly
		// what the profile has to say.
		{
			Name:    "cypher-write-mem",
			Surface: "cypher write barrier, graph/lpg, graph/mvcc",
			Ops:     400000,
			Setup: func(_ string) (Op, func() error, error) {
				g, err := seedGraph(0)
				if err != nil {
					return nil, nil, err
				}
				eng := cypher.NewEngine(g)
				op := func(ctx context.Context, worker, iter int) error {
					// Disjoint key spaces: this measures the mechanism, not
					// data conflicts between writers.
					id := int64(worker)<<40 + int64(iter)
					_, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
						map[string]expr.Value{"id": expr.IntegerValue(id)})
					return err
				}
				return op, func() error { return nil }, nil
			},
		},

		// WAL-backed autocommit writes: the durable path, where the store's
		// single-writer semaphore and the WAL's group-commit fsync live.
		{
			Name:    "cypher-write-wal",
			Surface: "store/txn, store/wal group commit, cypher write path",
			Ops:     4000,
			Setup: func(dir string) (Op, func() error, error) {
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
				db := store.New(wr, store.WithQuiesce(st.RunUnderCommitLock))
				eng := cypher.NewEngineWithStore(st)
				op := func(ctx context.Context, worker, iter int) error {
					id := int64(worker)<<40 + int64(iter)
					_, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
						map[string]expr.Value{"id": expr.IntegerValue(id)})
					return err
				}
				return op, db.Close, nil
			},
		},

		// --- Mixed read/write --------------------------------------------
		// Readers and writers on the same graph at once. A design where
		// readers never block writers should show this as two independent
		// populations; a reader-writer lock spanning both shows up here and
		// nowhere else.
		//
		// The mix is a property of the OPERATION, not of the worker; see
		// [mixedIsWrite] for why that distinction decides whether this
		// workload's scaling column means anything.
		{
			Name:    "cypher-mixed-rw",
			Surface: "graph/mvcc visibility, graph/lpg, cypher",
			Ops:     200000,
			Setup: func(_ string) (Op, func() error, error) {
				g, err := seedGraph(2000)
				if err != nil {
					return nil, nil, err
				}
				eng := cypher.NewEngine(g)
				op := func(ctx context.Context, worker, iter int) error {
					if mixedIsWrite(worker, iter) {
						id := int64(worker)<<40 + int64(iter)
						_, err := eng.RunInTx(ctx, "CREATE (:Account {id: $id})",
							map[string]expr.Value{"id": expr.IntegerValue(id)})
						return err
					}
					_, err := eng.Run(ctx, "MATCH (n:N) RETURN count(n)", nil)
					return err
				}
				return op, func() error { return nil }, nil
			},
		},

		// --- Raw graph API ------------------------------------------------
		// No Cypher, no planner: direct adjacency reads. Isolates the storage
		// engine's own read path from everything the query layer adds.
		{
			Name:    "lpg-neighbours-read",
			Surface: "graph/lpg, graph/adjlist",
			Ops:     10000000,
			Setup: func(_ string) (Op, func() error, error) {
				const n = 20000
				g, err := seedGraph(n)
				if err != nil {
					return nil, nil, err
				}
				op := func(_ context.Context, worker, iter int) error {
					// 7919 is prime and coprime with n, so successive
					// iterations stride the whole node space instead of
					// re-reading one cache-hot node.
					id := fmt.Sprintf("n%d", (worker*7919+iter)%n)
					if _, ok := g.OutDegree(id); !ok {
						return fmt.Errorf("OutDegree(%s): node absent", id)
					}
					g.NodeLabels(id)
					if _, ok := g.GetNodeProperty(id, "v"); !ok {
						return fmt.Errorf("GetNodeProperty(%s): absent", id)
					}
					return nil
				}
				return op, func() error { return nil }, nil
			},
		},
	}
}

// mixedWriteEvery is the reciprocal of the cypher-mixed-rw write fraction:
// one operation in four is a write.
const mixedWriteEvery = 4

// mixedIsWrite decides whether one cypher-mixed-rw operation is a write.
//
// The decision is a function of the OPERATION - worker and iteration together -
// never of the worker alone. A worker-only split (worker%4 == 0) silently
// changed the workload with the level: at level 1 the single worker is worker
// 0, so the whole run was writes, while every other rung ran a quarter writes.
// Writes are far the slower of the two, so the level-1 baseline was depressed
// and scaling_vs_1 reported a superlinear speed-up that was nothing but the mix
// changing underneath it. A workload must measure the same thing at every rung,
// or the ladder compares two different experiments.
func mixedIsWrite(worker, iter int) bool {
	return (worker+iter)%mixedWriteEvery == 0
}

// ByName returns the workload with the given name.
//
// ByName is safe for concurrent use: it allocates a fresh [Workload] per call
// and shares no state between callers.
func ByName(name string) (Workload, bool) {
	if w, ok := lookupBase(name); ok {
		return w, true
	}
	// Ceiling arms are addressable by name but are deliberately not part of
	// [All], so a sweep never walks them; see the note at the head of
	// ceiling_arms.go.
	for _, w := range ceilingArms() {
		if w.Name == name {
			return w, true
		}
	}
	return Workload{}, false
}
