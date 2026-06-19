// Example 21_typed_recovery — durable recovery of a typed
// (int64, float64) graph through the canonical recovery.Open[N, W] path.
//
// It builds a seeded, scale-parametrised weighted routing network with
// numeric station IDs (int64) and real-valued edge distances (float64),
// commits it through a typed txn.Store, takes a v2 snapshot, drops every
// in-memory reference, then rebuilds the graph from disk via
// recovery.OpenCtx instantiated with the matching codec pair. It then
// proves the round-trip preserved the data and reports the evidence that
// matters for a persistence subject — on-disk snapshot bytes, recovery
// wall-clock, and live heap before versus after recovery.
//
// # Model
//
//	(:STATION|:HUB {name, zone, elevation_m})           // numeric int64 id
//	(:STATION|:HUB)-[:HIGHWAY|:REGIONAL|:LOCAL          // road class
//	    {distance_km, lanes, toll}]->(:STATION|:HUB)    // float64 weight
//
// Every node is an int64 key drawn from a fixed base; a fraction of the
// nodes are promoted to :HUB and the rest are :STATION. Every node
// carries three typed properties: a name (string), a zone (int64), and an
// elevation_m (float64). Roads are directed out-edges: each node connects
// to a random fan-out of distinct other nodes, the edge weight IS the
// distance in kilometres (float64), and the same value is also stored as
// the edge's distance_km property so it can be read back two ways. Each
// edge carries a road-class label and two further typed properties: lanes
// (int64) and toll (bool).
//
// The float64 weights are drawn to exercise the IEEE-754 round-trip:
// transcendental-scaled values, a guaranteed exact integer, and a
// guaranteed denormal-adjacent value per build, so the bit-exact check
// has something non-trivial to verify. The whole dataset — node count,
// labels, weights, and properties — is reproducible for a fixed -seed.
//
// # What it proves (deterministic facts)
//
//   - the recovered node, edge, and label record counts match what was
//     committed;
//   - every float64 weight survives bit-for-bit through
//     txn.NewFloat64WeightCodec (weights.bit_exact=true plus the count of
//     weights verified by comparing math.Float64bits before and after);
//   - typed properties (string / int64 / float64 / bool) attached before
//     the snapshot survive recovery;
//   - Result.SnapshotSchemaVersion reports the v2 manifest a non-string
//     graph carries, so callers can branch on the on-disk schema without
//     re-opening the manifest.
//
// This is the same flow the production restart path uses; the only thing
// that changes from one (N, W) instantiation to another is the codec pair
// passed to recovery.Options.
//
// # Scale
//
// Run with no flags the example builds a small deterministic default
// (256 stations, fan-out 3..6) that the regression test pins and that
// completes in well under a second. Every dimension is a flag, so the
// same binary scales up to where the recovery cost and on-disk footprint
// become observable:
//
//	go run ./examples/21_typed_recovery -nodes 500000 -fanout-max 12 -seed 7
//
// The deterministic data shape is reproducible for a fixed -seed; only
// the telemetry (lines prefixed with "# ") — durations, on-disk bytes,
// and heap figures — varies between runs and machines. The store is
// written to an os.MkdirTemp directory that is removed on exit and kept
// out of the report so the deterministic output stays byte-stable.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Node labels and relationship types, plus the property keys. Centralised
// so the model is described in exactly one place and a rename surfaces as
// a compile error everywhere it is used.
const (
	labelStation = "STATION"
	labelHub     = "HUB"

	relHighway  = "HIGHWAY"
	relRegional = "REGIONAL"
	relLocal    = "LOCAL"

	// Node properties.
	propName      = "name"        // string
	propZone      = "zone"        // int64
	propElevation = "elevation_m" // float64

	// Edge properties. distance_km mirrors the float64 weight, so it is
	// the property-tier echo of the bit-exact subject.
	propDistance = "distance_km" // float64 (== edge weight)
	propLanes    = "lanes"       // int64
	propToll     = "toll"        // bool

	// stationBase is the fixed offset every node id counts up from, so the
	// keys are genuinely large int64 values (not 0..n) and the int64 codec
	// is exercised on realistic identifiers.
	stationBase int64 = 10_000
)

// config captures every scale and shape knob of the example. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	nodes     int     // number of nodes (STATION + HUB)
	fanoutMin int     // minimum out-degree per node (inclusive)
	fanoutMax int     // maximum out-degree per node (inclusive)
	hubFrac   float64 // fraction of nodes promoted to :HUB, in [0,1]
	seed      int64   // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns the small, deterministic default the regression
// test pins. It is intentionally tiny so `go test` stays far under the
// 60 s short-layer budget while still exercising every code path —
// labels, four property types, and the bit-exact weight round-trip — over
// hundreds of nodes and roughly a thousand edges.
func defaultConfig() config {
	return config{
		nodes:     256,
		fanoutMin: 3,
		fanoutMax: 6,
		hubFrac:   0.1,
		seed:      1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape — for instance more out-edges than there are other nodes to
// connect to. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.nodes <= 0:
		return fmt.Errorf("nodes must be > 0, got %d", c.nodes)
	case c.fanoutMin < 0 || c.fanoutMax < c.fanoutMin:
		return fmt.Errorf("require 0 <= fanoutMin <= fanoutMax, got [%d,%d]", c.fanoutMin, c.fanoutMax)
	case c.fanoutMax >= c.nodes:
		return fmt.Errorf("fanoutMax (%d) exceeds nodes-1 (%d): not enough distinct targets", c.fanoutMax, c.nodes-1)
	case c.hubFrac < 0 || c.hubFrac > 1:
		return fmt.Errorf("hubFrac must be in [0,1], got %g", c.hubFrac)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of nodes (STATION + HUB)")
	flag.IntVar(&cfg.fanoutMin, "fanout-min", cfg.fanoutMin, "minimum out-degree per node")
	flag.IntVar(&cfg.fanoutMax, "fanout-max", cfg.fanoutMax, "maximum out-degree per node")
	flag.Float64Var(&cfg.hubFrac, "hub-frac", cfg.hubFrac, "fraction of nodes promoted to :HUB, in [0,1]")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run builds the routing network described by cfg, persists it to a v2
// snapshot, recovers it through recovery.OpenCtx, verifies the round-trip,
// and writes a report to w. Bare lines carry deterministic facts (counts,
// the bit-exact verdict, sampled property values — reproducible for a
// fixed seed); lines prefixed with "# " carry volatile telemetry
// (durations, on-disk bytes, heap figures) that vary per run and per
// machine. All output goes to w so a test can capture and assert on the
// deterministic lines.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.nodes=%d\n", cfg.nodes)
	fmt.Fprintf(w, "config.fanout=[%d,%d]\n", cfg.fanoutMin, cfg.fanoutMax)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	dir, err := os.MkdirTemp("", "gograph-ex21-")
	if err != nil {
		return fmt.Errorf("MkdirTemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// === Phase 1: build + commit the typed graph through a v2 snapshot ===
	gen, err := buildAndPersist(ctx, dir, cfg, w)
	if err != nil {
		return err
	}

	// === Phase 2: drop in-memory references and recover from disk ===
	if err := ctx.Err(); err != nil {
		return err
	}
	base := readMem()
	start := time.Now()
	res, err := recovery.OpenCtx[int64, float64](ctx, dir, recovery.Options[int64, float64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		return fmt.Errorf("recovery.OpenCtx: %w", err)
	}
	elapsed := time.Since(start)
	recovered := readMem()

	// A corrupt WAL is fail-stop: recovery returns a non-nil error (handled
	// above) and res.IsClean() reports false. Refuse to proceed onto a
	// corrupt log rather than silently building on a damaged prefix.
	if !res.IsClean() {
		return fmt.Errorf("recovery: refusing to use a corrupt WAL: %w", res.TailErr)
	}

	// === Phase 3: deterministic facts about the recovered graph ===
	adj := res.Graph.AdjList()
	fmt.Fprintf(w, "recovered.nodes=%d\n", adj.Order())
	fmt.Fprintf(w, "recovered.edges=%d\n", adj.Size())
	fmt.Fprintf(w, "recovered.label_records=%d\n", res.SnapshotLabels)
	fmt.Fprintf(w, "recovered.property_records=%d\n", res.SnapshotProperties)
	fmt.Fprintf(w, "recovered.schema_version=v%d\n", res.SnapshotSchemaVersion)

	// === Phase 4: bit-exact float64 weight verification ===
	// Compare math.Float64bits of every weight captured at generation time
	// against the weight read back from the recovered graph. A single
	// mismatch makes the verdict false; the count records how many weights
	// were checked. Using the weights captured during the build (rather than
	// re-deriving them from the seed) makes the check robust: it asserts the
	// on-disk codec preserved exactly what was committed.
	verified, allExact, err := verifyWeights(ctx, res.Graph, gen.expectedWeights)
	if err != nil {
		return fmt.Errorf("verify weights: %w", err)
	}
	fmt.Fprintf(w, "weights.verified=%d\n", verified)
	fmt.Fprintf(w, "weights.bit_exact=%t\n", allExact)

	// === Phase 5: sampled typed-property values (one per type) ===
	if err := reportSampleProperties(w, res.Graph, gen); err != nil {
		return err
	}

	// === Phase 6: volatile telemetry (subject = persistence/recovery) ===
	snapBytes, err := dirSize(filepath.Join(dir, "snapshot"))
	if err != nil {
		return fmt.Errorf("snapshot size: %w", err)
	}
	walBytes, err := fileSize(filepath.Join(dir, "wal"))
	if err != nil {
		return fmt.Errorf("wal size: %w", err)
	}
	fmt.Fprintf(w, "# build.elapsed=%s\n", gen.buildElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# snapshot.write_elapsed=%s\n", gen.snapElapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# disk.snapshot_bytes=%s\n", humanBytes(snapBytes))
	fmt.Fprintf(w, "# disk.wal_bytes=%s\n", humanBytes(walBytes))
	fmt.Fprintf(w, "# recovery.elapsed=%s\n", elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# recovery.ops=%d\n", res.WALOps)
	fmt.Fprintf(w, "# mem.heap_before_recovery=%s\n", humanBytes(base.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_after_recovery=%s\n", humanBytes(recovered.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n", humanBytes(saturatingSub(recovered.HeapAlloc, base.HeapAlloc)))
	return nil
}

// edgeKey packs a directed (src, dst) pair into a single comparable map
// key so the captured weights can be looked up by endpoint pair.
type edgeKey struct {
	src, dst int64
}

// edgeSpec is one fully-drawn out-edge from a source node: its destination,
// float64 weight (== distance_km), road-class label, and the two further
// typed edge properties. All fields are drawn from the seeded RNG up front
// so the dataset is fixed by the seed, then written in two passes (the WAL
// commit, then the post-commit property attach).
type edgeSpec struct {
	dst    int64
	weight float64
	class  string
	lanes  int64
	toll   bool
}

// genResult carries the realised shape of a build (the random fan-out
// means the edge total is not known until the graph is materialised) plus
// the wall-clock costs, the weights captured at generation time (the
// bit-exact verification oracle), and a sample node/edge to anchor the
// property report.
type genResult struct {
	nodes           int
	edges           int
	hubs            int
	expectedWeights map[edgeKey]float64 // weight committed for each edge, the verify oracle
	sampleNode      int64               // a fixed node whose typed properties are reported
	sampleSrc       int64               // a fixed edge's endpoints, for the edge-property sample
	sampleDst       int64
	buildElapsed    time.Duration
	snapElapsed     time.Duration
}

// buildAndPersist materialises the routing network described by cfg into a
// fresh typed graph behind a WAL-backed store, commits every node and edge
// through transactions, attaches typed properties on the in-memory graph
// (properties flush through the snapshot, not the WAL), writes a v2
// snapshot, and closes the WAL. It returns the realised shape so the
// caller can verify the recovery against it. ctx cancellation is honoured
// between phases and on a periodic check inside the loops.
func buildAndPersist(ctx context.Context, dir string, cfg config, _ io.Writer) (genResult, error) {
	walPath := filepath.Join(dir, "wal")
	wl, err := wal.Open(walPath)
	if err != nil {
		return genResult{}, fmt.Errorf("wal.Open: %w", err)
	}
	g := lpg.New[int64, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[int64, float64](g, wl, txn.Options[int64, float64]{
		Codec:       txn.NewInt64Codec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	//nolint:gosec // G404: a seeded math/rand is intentional here — the example
	// must reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	// Pre-compute which nodes are hubs, deterministically from the seed.
	hubs := 0
	isHub := make([]bool, cfg.nodes)
	for i := 0; i < cfg.nodes; i++ {
		if rng.Float64() < cfg.hubFrac {
			isHub[i] = true
			hubs++
		}
	}

	// Nodes. Every node and its label is committed in one transaction so all
	// edge targets exist before the edge phase references them. Batching the
	// node creation into a single atomic commit (rather than one fsync per
	// node) is both realistic — "load the full station list" — and keeps the
	// build fast. The typed node properties are then set on the in-memory
	// graph; they flush through the snapshot, not the WAL.
	if err := commitTx(store, func(tx *txn.Tx[int64, float64]) error {
		for i := 0; i < cfg.nodes; i++ {
			if i%checkEvery == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			id := stationBase + int64(i)
			label := labelStation
			if isHub[i] {
				label = labelHub
			}
			if err := tx.AddNode(id); err != nil {
				return fmt.Errorf("AddNode %d: %w", id, err)
			}
			if err := tx.SetNodeLabel(id, label); err != nil {
				return fmt.Errorf("SetNodeLabel %d/%s: %w", id, label, err)
			}
		}
		return nil
	}); err != nil {
		return genResult{}, err
	}
	for i := 0; i < cfg.nodes; i++ {
		if err := setNodeProps(g, stationBase+int64(i), rng, isHub[i]); err != nil {
			return genResult{}, err
		}
	}

	// Edges. Each node gets a random out-degree in [fanoutMin, fanoutMax] to
	// distinct other nodes; the edge weight is the float64 distance and is
	// echoed as the distance_km property. A node and all its out-edges are
	// committed in a single transaction — "wire this station's roads
	// atomically" — and each generated weight is recorded in expected so the
	// recovery can be verified bit-for-bit without re-deriving it from the
	// seed.
	edges := 0
	expected := make(map[edgeKey]float64, cfg.nodes*cfg.fanoutMax)
	targets := make(map[int]struct{}, cfg.fanoutMax)
	specs := make([]edgeSpec, 0, cfg.fanoutMax)
	for i := 0; i < cfg.nodes; i++ {
		if i%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return genResult{}, err
			}
		}
		degree := cfg.fanoutMin + rng.Intn(cfg.fanoutMax-cfg.fanoutMin+1)
		clear(targets)
		for len(targets) < degree {
			j := rng.Intn(cfg.nodes)
			if j != i {
				targets[j] = struct{}{}
			}
		}
		src := stationBase + int64(i)
		// Shape this source's edges in ascending target-index order so the
		// WAL/commit order is deterministic regardless of Go's randomised map
		// iteration. All RNG draws happen here, so the dataset is fixed by the
		// seed; the actual writes follow below.
		specs := specs[:0]
		for j := 0; j < cfg.nodes; j++ {
			if _, ok := targets[j]; !ok {
				continue
			}
			specs = append(specs, edgeSpec{
				dst:    stationBase + int64(j),
				weight: edgeWeight(rng),
				class:  roadClass(rng),
				lanes:  int64(1 + rng.Intn(maxLanes)),
				toll:   rng.Float64() < tollFrac,
			})
		}
		// Commit this source's edges + labels atomically.
		err := commitTx(store, func(tx *txn.Tx[int64, float64]) error {
			for _, s := range specs {
				if err := tx.AddEdge(src, s.dst, s.weight); err != nil {
					return fmt.Errorf("AddEdge %d->%d: %w", src, s.dst, err)
				}
				if err := tx.SetEdgeLabel(src, s.dst, s.class); err != nil {
					return fmt.Errorf("SetEdgeLabel %d->%d: %w", src, s.dst, err)
				}
			}
			return nil
		})
		if err != nil {
			return genResult{}, err
		}
		// Edge properties are set on the in-memory graph after the edges exist
		// (the transaction has committed); they flush through the snapshot.
		for _, s := range specs {
			if err := setEdgeProps(g, src, s.dst, s.weight, s.lanes, s.toll); err != nil {
				return genResult{}, err
			}
			expected[edgeKey{src, s.dst}] = s.weight
			edges++
		}
	}
	_ = g.AdjList() // touch the adjacency list to materialise the mapper
	buildElapsed := time.Since(start)

	// Persist a v2 snapshot. For an int64-keyed graph WriteSnapshotFull
	// emits no mapper.bin and stamps the manifest v2 (a string-keyed graph
	// would add mapper.bin and be stamped v3).
	if err := ctx.Err(); err != nil {
		return genResult{}, err
	}
	snapStart := time.Now()
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := snapshot.WriteSnapshotFullCtx(ctx, filepath.Join(dir, "snapshot"), cs, g); err != nil {
		return genResult{}, fmt.Errorf("snapshot.WriteSnapshotFullCtx: %w", err)
	}
	snapElapsed := time.Since(snapStart)
	if err := wl.Close(); err != nil {
		return genResult{}, fmt.Errorf("wal.Close: %w", err)
	}

	// A fixed sample edge: the first out-edge committed from node 0, which
	// for fanoutMin >= 1 always exists. Its endpoints anchor the
	// edge-property sample in the report.
	sampleSrc, sampleDst, err := firstOutEdge(g, stationBase)
	if err != nil {
		return genResult{}, err
	}
	return genResult{
		nodes:           cfg.nodes,
		edges:           edges,
		hubs:            hubs,
		expectedWeights: expected,
		sampleNode:      stationBase,
		sampleSrc:       sampleSrc,
		sampleDst:       sampleDst,
		buildElapsed:    buildElapsed,
		snapElapsed:     snapElapsed,
	}, nil
}

// checkEvery bounds how often the build polls ctx for cancellation: often
// enough that a cancelled large build stops promptly, rare enough that the
// check is free relative to the surrounding work.
const checkEvery = 4096

// commitTx runs fn inside a fresh transaction and commits it, rolling the
// transaction back if fn fails so a partial write never reaches the WAL
// (atomicity). The commit fsyncs the WAL, so grouping many operations per
// transaction amortises that cost — the reason the build batches a node's
// out-edges into one commit. The float64 edge weights it writes are encoded
// by txn.NewFloat64WeightCodec, whose bit-exact survival the example
// verifies after recovery.
func commitTx(store *txn.Store[int64, float64], fn func(tx *txn.Tx[int64, float64]) error) error {
	tx := store.Begin()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// setNodeProps attaches the three typed node properties on the in-memory
// graph: name (string), zone (int64), and elevation_m (float64). These
// flush through the snapshot.
func setNodeProps(g *lpg.Graph[int64, float64], id int64, rng *rand.Rand, hub bool) error {
	name := stationName(rng, hub)
	if err := g.SetNodeProperty(id, propName, lpg.StringValue(name)); err != nil {
		return fmt.Errorf("SetNodeProperty %s %d: %w", propName, id, err)
	}
	zone := int64(1 + rng.Intn(zoneCount))
	if err := g.SetNodeProperty(id, propZone, lpg.Int64Value(zone)); err != nil {
		return fmt.Errorf("SetNodeProperty %s %d: %w", propZone, id, err)
	}
	elevation := math.Round(rng.Float64()*maxElevationM*100) / 100 // two decimals
	if err := g.SetNodeProperty(id, propElevation, lpg.Float64Value(elevation)); err != nil {
		return fmt.Errorf("SetNodeProperty %s %d: %w", propElevation, id, err)
	}
	return nil
}

// setEdgeProps attaches the three typed edge properties on the in-memory
// graph: distance_km (float64, the same value as the weight), lanes
// (int64), and toll (bool). distance_km is stored bit-for-bit identical to
// the weight so a reader can recover the distance from either tier. It must
// run after the edge exists on g (i.e. after the transaction that added it
// has committed), since properties are attached to the live graph and flush
// through the snapshot.
func setEdgeProps(g *lpg.Graph[int64, float64], src, dst int64, weight float64, lanes int64, toll bool) error {
	if err := g.SetEdgeProperty(src, dst, propDistance, lpg.Float64Value(weight)); err != nil {
		return fmt.Errorf("SetEdgeProperty %s %d->%d: %w", propDistance, src, dst, err)
	}
	if err := g.SetEdgeProperty(src, dst, propLanes, lpg.Int64Value(lanes)); err != nil {
		return fmt.Errorf("SetEdgeProperty %s %d->%d: %w", propLanes, src, dst, err)
	}
	if err := g.SetEdgeProperty(src, dst, propToll, lpg.BoolValue(toll)); err != nil {
		return fmt.Errorf("SetEdgeProperty %s %d->%d: %w", propToll, src, dst, err)
	}
	return nil
}

// verifyWeights confirms every weight captured at generation time survived
// the snapshot+recovery round-trip bit-for-bit. For each recorded edge it
// reads the weight back from the recovered graph and compares
// math.Float64bits of the two. It returns how many weights it checked
// (every recorded edge) and whether all of them matched exactly. Because
// the comparison is against the values committed — not a re-derivation from
// the seed — it directly asserts that txn.NewFloat64WeightCodec preserved
// what was written.
func verifyWeights(ctx context.Context, g *lpg.Graph[int64, float64], expected map[edgeKey]float64) (int, bool, error) {
	// Index the recovered weights once: iterate each source's neighbours and
	// record the weight per (src, dst). This is O(edges) total, rather than
	// an O(degree) scan per expected edge.
	got := make(map[edgeKey]float64, len(expected))
	checked := 0
	for k := range expected {
		if checked%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		checked++
		if _, seen := got[k]; seen {
			continue
		}
		for n, wgt := range g.AdjList().Neighbours(k.src) {
			got[edgeKey{k.src, n}] = wgt
		}
	}

	verified := 0
	allExact := true
	for k, want := range expected {
		w, ok := got[k]
		if !ok {
			return verified, false, fmt.Errorf("recovered graph missing edge %d->%d", k.src, k.dst)
		}
		if !bitsEqual(w, want) {
			allExact = false
		}
		verified++
	}
	return verified, allExact, nil
}

// reportSampleProperties prints one recovered property per type — string,
// int64, float64, and bool — so the test can pin a concrete value for each
// without depending on the volatile telemetry. The chosen node and edge
// are fixed by the seed (node 0 and its first out-edge).
func reportSampleProperties(w io.Writer, g *lpg.Graph[int64, float64], gen genResult) error {
	name, ok := g.GetNodeProperty(gen.sampleNode, propName)
	if !ok {
		return fmt.Errorf("recovered graph missing node %d.%s", gen.sampleNode, propName)
	}
	ns, _ := name.String()
	fmt.Fprintf(w, "sample.node_name=%s\n", ns)

	zone, ok := g.GetNodeProperty(gen.sampleNode, propZone)
	if !ok {
		return fmt.Errorf("recovered graph missing node %d.%s", gen.sampleNode, propZone)
	}
	zi, _ := zone.Int64()
	fmt.Fprintf(w, "sample.node_zone=%d\n", zi)

	dist, ok := g.GetEdgeProperty(gen.sampleSrc, gen.sampleDst, propDistance)
	if !ok {
		return fmt.Errorf("recovered graph missing edge (%d,%d).%s", gen.sampleSrc, gen.sampleDst, propDistance)
	}
	df, _ := dist.Float64()
	// Echo the distance as its raw IEEE-754 bits so the value is pinned
	// exactly (a decimal rendering could round); this is a deterministic
	// fact, not telemetry.
	fmt.Fprintf(w, "sample.edge_distance_bits=%#016x\n", math.Float64bits(df))

	toll, ok := g.GetEdgeProperty(gen.sampleSrc, gen.sampleDst, propToll)
	if !ok {
		return fmt.Errorf("recovered graph missing edge (%d,%d).%s", gen.sampleSrc, gen.sampleDst, propToll)
	}
	tb, _ := toll.Bool()
	fmt.Fprintf(w, "sample.edge_toll=%t\n", tb)
	return nil
}

// firstOutEdge returns the first neighbour of src in the in-memory graph,
// used to fix a deterministic sample edge for the property report.
func firstOutEdge(g *lpg.Graph[int64, float64], src int64) (int64, int64, error) {
	best := int64(-1)
	for n := range g.AdjList().Neighbours(src) {
		if best == -1 || n < best {
			best = n
		}
	}
	if best == -1 {
		return 0, 0, fmt.Errorf("node %d has no out-edge to sample", src)
	}
	return src, best, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Deterministic data shaping
// ─────────────────────────────────────────────────────────────────────────────

// Property-range constants. Fixed so the dataset is reproducible.
const (
	zoneCount     = 9      // zone in [1, zoneCount]
	maxElevationM = 2500.0 // elevation_m in [0, maxElevationM]
	maxLanes      = 6      // lanes in [1, maxLanes]
	tollFrac      = 0.25   // probability an edge is a toll road
)

// edgeWeight returns a deterministic float64 distance in kilometres drawn
// from rng. The draw is engineered so the bit-exact round-trip is
// non-trivial: most weights are transcendental-scaled (a product of an
// irrational constant and a random fraction, so they have a full 52-bit
// mantissa), one in every sampleExactEvery draws is a guaranteed exact
// integer, and one in every sampleTinyEvery draws is a guaranteed
// denormal-adjacent tiny value. All three classes must survive the codec
// bit-for-bit.
func edgeWeight(rng *rand.Rand) float64 {
	switch n := rng.Intn(sampleTinyEvery); {
	case n == 0:
		return 1e-300 // denormal-adjacent
	case n%sampleExactEvery == 1:
		return float64(1 + rng.Intn(500)) // exact integer
	default:
		return math.Pi * rng.Float64() * 100 // transcendental-scaled, full mantissa
	}
}

const (
	sampleExactEvery = 7  // ~1/7 of edges get an exact-integer weight
	sampleTinyEvery  = 50 // ~1/50 of edges get a denormal-adjacent weight
)

// roadClass returns a deterministic road-class label drawn from rng.
func roadClass(rng *rand.Rand) string {
	switch rng.Intn(3) {
	case 0:
		return relHighway
	case 1:
		return relRegional
	default:
		return relLocal
	}
}

// stationName assembles a plausible place name from fixed word lists. Hubs
// draw from a separate suffix list so their names read as interchanges.
func stationName(rng *rand.Rand, hub bool) string {
	base := placePrefixes[rng.Intn(len(placePrefixes))] + placeSuffixes[rng.Intn(len(placeSuffixes))]
	if hub {
		return base + " " + hubSuffixes[rng.Intn(len(hubSuffixes))]
	}
	return base
}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry and filesystem helpers
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// saturatingSub returns a-b, or 0 when b > a (a GC between the two
// snapshots can leave the "after" heap smaller than the "before").
func saturatingSub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// dirSize returns the total size in bytes of every regular file under dir
// (recursively). Used to report the on-disk snapshot footprint. A regular
// file's size is non-negative, so the running total is accumulated as a
// uint64; any (impossible) negative size is treated as zero.
func dirSize(dir string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if sz := info.Size(); sz > 0 {
			total += uint64(sz)
		}
		return nil
	})
	return total, err
}

// fileSize returns the size in bytes of a single file. A file's size is
// non-negative, so it is returned as a uint64; an (impossible) negative
// size is reported as zero.
func fileSize(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if sz := info.Size(); sz > 0 {
		return uint64(sz), nil
	}
	return 0, nil
}

// humanBytes formats a byte count with a binary (KiB/MiB/GiB) suffix.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// bitsEqual returns true iff a and b have identical IEEE-754
// representations. Used to confirm a float weight survived the
// snapshot+recovery round-trip without rounding.
func bitsEqual(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

// ─────────────────────────────────────────────────────────────────────────────
// Realistic-data word lists. Fixed so the dataset is reproducible.
// ─────────────────────────────────────────────────────────────────────────────

var placePrefixes = []string{
	"North", "South", "East", "West", "Upper", "Lower", "Old", "New",
	"Great", "Little", "High", "Low", "Mill", "Stone", "Ash", "Oak",
}

var placeSuffixes = []string{
	"ford", "bridge", "haven", "field", "wood", "dale", "moor", "gate",
	"port", "hill", "vale", "cross", "bourne", "wick", "thorpe", "mere",
}

var hubSuffixes = []string{
	"Interchange", "Junction", "Central", "Terminal", "Exchange",
}
