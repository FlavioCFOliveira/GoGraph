// Example 04_persistence — the full GoGraph durability path on a real
// directory, driven at a configurable, reproducible scale.
//
// It builds a seeded software-supply-chain graph entirely through
// WAL-committed transactions, takes a v2 snapshot (CSR + labels.bin +
// properties.bin), drops every in-memory reference, then rebuilds the
// graph from disk with [recovery.Open] and verifies the data survived.
//
//  1. Every mutation — nodes, labels, edges, edge labels, typed node
//     properties, typed edge properties — is appended to the WAL and
//     applied to the in-memory LPG inside a committed transaction, so
//     the whole graph is durable, not just its topology.
//  2. snapshot.WriteSnapshotFull persists the CSR view, labels.bin and
//     properties.bin atomically alongside the WAL — a checkpoint of the
//     label and typed-property state.
//  3. The process "restarts": every in-memory reference is dropped and
//     recovery.Open rebuilds the graph from the snapshot plus the WAL
//     tail. The recovered graph is then queried back through the LPG
//     read API to confirm counts and sample property values round-trip.
//
// # Model
//
//	(:Package {name, language, downloads})       // a published library
//	(:Release {coord, version, published})        // one version of a package
//	(:Package)-[:PUBLISHED {weight}]->(:Release)   // package owns its releases
//	(:Release)-[:DEPENDS_ON {constraint, weight}]->(:Package)
//
// Every Package owns exactly one Release in this model (coord = name@version),
// and each Release declares a random number of DEPENDS_ON edges to other
// packages, each carrying a semver-style version constraint string and an
// int64 weight (the declared dependency rank). PUBLISHED and DEPENDS_ON edges
// both carry a durable int64 weight, so the WAL exercises OpAddEdgeWeighted
// and the typed-property path carries strings, int64s and a timestamp.
//
// All node and edge properties are written inside the transaction, so they
// travel through the WAL (OpSetNodeProperty / OpSetEdgeProperty) and are
// replayed on recovery — the v2 snapshot's labels.bin / properties.bin is the
// checkpoint that lets recovery start from a compacted base rather than
// replaying the whole log.
//
// # Scale
//
// Run with no flags, the example builds a small, deterministic default
// (300 packages) that persists, snapshots and recovers well under a second,
// so `go test` stays comfortably inside the short-layer budget. Every
// dimension is a flag, so the same binary scales up to a size where the
// persistence cost is observable:
//
//	go run ./examples/04_persistence -packages 200000 -seed 7
//
// The deterministic data shape — recovered node, edge and label counts and
// the sampled property values — is reproducible for a fixed -seed; only the
// telemetry (lines prefixed with "# ": throughput, on-disk bytes, recovery
// wall-clock and live heap) varies between runs and machines. The store is
// written to a directory created with [os.MkdirTemp] whose path differs every
// run and is deliberately never printed, so the report stays stable.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
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

// Node labels and relationship types. Centralised so the model is
// described in exactly one place and a rename surfaces as a compile
// error everywhere it is used.
const (
	labelPackage = "Package"
	labelRelease = "Release"

	relPublished = "PUBLISHED"  // (:Package)-[:PUBLISHED]->(:Release)
	relDependsOn = "DEPENDS_ON" // (:Release)-[:DEPENDS_ON]->(:Package)

	// Typed node properties.
	propName       = "name"      // Package.name      (string)
	propLanguage   = "language"  // Package.language  (string)
	propDownloads  = "downloads" // Package.downloads (int64)
	propCoord      = "coord"     // Release.coord     (string, "name@version")
	propVersion    = "version"   // Release.version   (string)
	propPublishedt = "published" // Release.published (timestamp)

	// Typed edge properties.
	propConstraint = "constraint" // DEPENDS_ON.constraint (string, e.g. "^1.4.0")
)

// config captures every scale and shape knob of the example. The zero
// value is not valid; build one with defaultConfig and override fields
// from flags (see main) or construct one directly (see the regression
// test).
type config struct {
	packages int   // number of :Package nodes (each owns one :Release)
	depsMin  int   // minimum DEPENDS_ON out-degree per release (inclusive)
	depsMax  int   // maximum DEPENDS_ON out-degree per release (inclusive)
	batch    int   // packages processed between context-cancellation checks
	seed     int64 // RNG seed; fixes the deterministic data shape
}

// defaultConfig returns a small, deterministic configuration whose
// recovered facts the regression test pins. Persistence I/O makes a run
// markedly slower than an in-memory build, so the default is kept modest:
// it persists, snapshots and recovers in well under a second, leaving the
// short-layer 60 s package budget untouched.
func defaultConfig() config {
	return config{
		packages: 300,
		depsMin:  2,
		depsMax:  6,
		batch:    50,
		seed:     1,
	}
}

// validate rejects a configuration that cannot produce the requested
// shape — for instance more dependencies than there are other packages
// to depend on. It is checked once, at the boundary, before any work.
func (c config) validate() error {
	switch {
	case c.packages <= 0:
		return fmt.Errorf("packages must be > 0, got %d", c.packages)
	case c.depsMin < 0 || c.depsMax < c.depsMin:
		return fmt.Errorf("require 0 <= depsMin <= depsMax, got [%d,%d]", c.depsMin, c.depsMax)
	case c.depsMax >= c.packages:
		return fmt.Errorf("depsMax (%d) exceeds packages-1 (%d): not enough distinct dependencies", c.depsMax, c.packages-1)
	case c.batch <= 0:
		return fmt.Errorf("batch must be > 0, got %d", c.batch)
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.packages, "packages", cfg.packages, "number of Package nodes (each owns one Release)")
	flag.IntVar(&cfg.depsMin, "deps-min", cfg.depsMin, "minimum DEPENDS_ON out-degree per release")
	flag.IntVar(&cfg.depsMax, "deps-max", cfg.depsMax, "maximum DEPENDS_ON out-degree per release")
	flag.IntVar(&cfg.batch, "batch", cfg.batch, "packages processed between context-cancellation checks")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the deterministic data shape)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run drives the full persistence walk-through — WAL-committed
// transactions, a v2 snapshot, then recovery from disk — and writes a
// report to w. Bare lines carry deterministic facts (recovered counts and
// sampled property values, reproducible for a fixed seed); lines prefixed
// with "# " carry volatile telemetry (throughput, on-disk bytes, recovery
// wall-clock and heap figures) that varies per run and per machine. All
// output goes to w so a test can capture and assert the deterministic
// lines; run returns wrapped errors rather than terminating the process.
// The temp directory it persists to is never written to w, so the report
// stays deterministic.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	fmt.Fprintf(w, "config.packages=%d\n", cfg.packages)
	fmt.Fprintf(w, "config.deps=[%d,%d]\n", cfg.depsMin, cfg.depsMax)
	fmt.Fprintf(w, "config.batch=%d\n", cfg.batch)
	fmt.Fprintf(w, "config.seed=%d\n", cfg.seed)

	dir, err := os.MkdirTemp("", "gograph-ex04-")
	if err != nil {
		return fmt.Errorf("MkdirTemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Phase 1: build the graph through WAL-committed transactions.
	stats, err := commit(ctx, dir, cfg, w)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 2: drop every in-memory reference and rebuild from disk.
	if err := restore(ctx, dir, &stats, w); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// commitStats reports the realised shape of the write phase (the random
// degrees mean the edge total is not known until the graph is built) plus
// the wall-clock cost, the on-disk footprint, and a sample package coord
// used to anchor the recovered-property assertions.
type commitStats struct {
	packages    int
	releases    int
	publishedE  int
	dependsOnE  int
	commits     int
	elapsed     time.Duration
	walBytes    uint64
	snapBytes   uint64
	sampleCoord string // coord of release[0], anchors the recovered-value facts
	sampleDls   int64  // package[0].downloads, asserted after recovery
	samplePub   time.Time
}

// commit builds the seeded supply-chain graph entirely through WAL
// transactions -- one committed transaction per package (package node,
// release node, and their PUBLISHED/DEPENDS_ON edges) -- and takes a v2
// snapshot. It returns the realised shape and the on-disk byte footprint.
// The write loop polls ctx for cancellation every cfg.batch packages.
//
//nolint:gocyclo // one linear build pipeline: nodes+labels+props, then edges+props, one tx per package.
func commit(ctx context.Context, dir string, cfg config, w io.Writer) (commitStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional — the example must
	// reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	walPath := filepath.Join(dir, "wal")
	wl, err := wal.Open(walPath)
	if err != nil {
		return commitStats{}, fmt.Errorf("wal.Open: %w", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions(g, wl, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Pre-draw the package facts so dependency edges (added in the same
	// pass) can reference any other package's stable key by index.
	pkgKeys := make([]string, cfg.packages)
	relKeys := make([]string, cfg.packages)
	downloads := make([]int64, cfg.packages)
	for i := 0; i < cfg.packages; i++ {
		name := packageName(rng, i)
		version := semver(rng)
		pkgKeys[i] = name
		relKeys[i] = name + "@" + version
		downloads[i] = int64(rng.Intn(50_000_000))
	}

	st := commitStats{packages: cfg.packages, releases: cfg.packages}
	st.sampleCoord = relKeys[0]
	st.sampleDls = downloads[0]

	deps := make(map[int]struct{}, cfg.depsMax)
	for i := 0; i < cfg.packages; i++ {
		if i%cfg.batch == 0 {
			if err := ctx.Err(); err != nil {
				_ = wl.Close()
				return commitStats{}, err
			}
		}
		tx := store.Begin()

		// Package node, its label and typed properties.
		pkg, rel := pkgKeys[i], relKeys[i]
		if err := txSetNode(tx, pkg, labelPackage); err != nil {
			return commitStats{}, abort(tx, wl, err)
		}
		if err := tx.SetNodeProperty(pkg, propName, lpg.StringValue(pkg)); err != nil {
			return commitStats{}, abort(tx, wl, fmt.Errorf("set %s.name: %w", pkg, err))
		}
		if err := tx.SetNodeProperty(pkg, propLanguage, lpg.StringValue(languages[i%len(languages)])); err != nil {
			return commitStats{}, abort(tx, wl, fmt.Errorf("set %s.language: %w", pkg, err))
		}
		if err := tx.SetNodeProperty(pkg, propDownloads, lpg.Int64Value(downloads[i])); err != nil {
			return commitStats{}, abort(tx, wl, fmt.Errorf("set %s.downloads: %w", pkg, err))
		}

		// Release node, its label and typed properties.
		published := isoPublish(rng)
		if i == 0 {
			st.samplePub = published
		}
		if err := txSetNode(tx, rel, labelRelease); err != nil {
			return commitStats{}, abort(tx, wl, err)
		}
		if err := tx.SetNodeProperty(rel, propCoord, lpg.StringValue(rel)); err != nil {
			return commitStats{}, abort(tx, wl, fmt.Errorf("set %s.coord: %w", rel, err))
		}
		if err := tx.SetNodeProperty(rel, propVersion, lpg.StringValue(versionOf(rel))); err != nil {
			return commitStats{}, abort(tx, wl, fmt.Errorf("set %s.version: %w", rel, err))
		}
		if err := tx.SetNodeProperty(rel, propPublishedt, lpg.TimeValue(published)); err != nil {
			return commitStats{}, abort(tx, wl, fmt.Errorf("set %s.published: %w", rel, err))
		}

		// PUBLISHED edge: the package owns its release (weight = 1).
		if err := txAddLabeledEdge(tx, pkg, rel, 1, relPublished); err != nil {
			return commitStats{}, abort(tx, wl, err)
		}
		st.publishedE++

		// DEPENDS_ON edges: the release declares a random number of
		// dependencies on other packages, each with a version constraint
		// and an int64 weight (the dependency rank).
		degree := cfg.depsMin + rng.Intn(cfg.depsMax-cfg.depsMin+1)
		clear(deps)
		for len(deps) < degree {
			j := rng.Intn(cfg.packages)
			if j != i {
				deps[j] = struct{}{}
			}
		}
		rank := int64(1)
		for j := range deps {
			if err := txAddLabeledEdge(tx, rel, pkgKeys[j], rank, relDependsOn); err != nil {
				return commitStats{}, abort(tx, wl, err)
			}
			if err := tx.SetEdgeProperty(rel, pkgKeys[j], propConstraint, lpg.StringValue(constraintOf(rng))); err != nil {
				return commitStats{}, abort(tx, wl, fmt.Errorf("set constraint: %w", err))
			}
			st.dependsOnE++
			rank++
		}

		if err := tx.Commit(); err != nil {
			_ = wl.Close()
			return commitStats{}, fmt.Errorf("commit tx at package %d: %w", i, err)
		}
		st.commits++
	}
	_ = wl.Close()

	// Take a v2 snapshot (CSR + labels.bin + properties.bin) as a checkpoint
	// alongside the WAL, the same protocol a background checkpointer uses.
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		return commitStats{}, fmt.Errorf("WriteSnapshotFull: %w", err)
	}

	st.elapsed = time.Since(start)
	st.walBytes = fileSize(walPath)
	st.snapBytes = dirSize(snapDir)

	// Deterministic facts: the realised shape the recovery phase must match.
	fmt.Fprintf(w, "nodes.packages=%d\n", st.packages)
	fmt.Fprintf(w, "nodes.releases=%d\n", st.releases)
	fmt.Fprintf(w, "edges.published=%d\n", st.publishedE)
	fmt.Fprintf(w, "edges.depends_on=%d\n", st.dependsOnE)

	// Volatile telemetry: write throughput and on-disk footprint.
	edges := st.publishedE + st.dependsOnE
	fmt.Fprintf(w, "# commit.elapsed=%s\n", st.elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "# commit.tx_rate=%.0f tx/s\n", rate(st.commits, st.elapsed))
	fmt.Fprintf(w, "# commit.edge_rate=%.0f edges/s\n", rate(edges, st.elapsed))
	fmt.Fprintf(w, "# disk.wal_bytes=%s\n", humanBytes(st.walBytes))
	fmt.Fprintf(w, "# disk.snapshot_bytes=%s\n", humanBytes(st.snapBytes))
	fmt.Fprintf(w, "# disk.bytes_per_edge=%.1f\n", safeDiv(float64(st.walBytes+st.snapBytes), float64(edges)))
	return st, nil
}

// restore drops the in-memory graph, rebuilds it from disk with
// recovery.Open, and verifies the recovered shape and a few sampled
// property values against the write phase. Deterministic facts are the
// recovered counts and sampled values; telemetry is the recovery
// wall-clock and the live heap before vs after recovery.
//
//nolint:gocyclo // the recovery-verification step is a flat sequence of independent property round-trip checks.
func restore(ctx context.Context, dir string, st *commitStats, w io.Writer) error {
	before := readMem()

	start := time.Now()
	res, err := recovery.OpenCtx[string, int64](ctx, dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		return fmt.Errorf("recovery.OpenCtx: %w", err)
	}
	// A corrupt WAL is fail-stop: recovery returns a non-nil error (handled
	// above) and IsClean reports false. Refuse to build on a corrupt log
	// rather than silently proceeding with a damaged prefix.
	if !res.IsClean() {
		return fmt.Errorf("recovery: corrupt WAL: %w", res.TailErr)
	}
	elapsed := time.Since(start)
	after := readMem()

	g := res.Graph
	adj := g.AdjList()

	// Deterministic facts: the recovered shape must match what was committed.
	fmt.Fprintf(w, "recovered.nodes=%d\n", adj.Order())
	fmt.Fprintf(w, "recovered.edges=%d\n", adj.Size())
	fmt.Fprintf(w, "recovered.labels=%d\n", len(g.NodeLabelsInUse()))
	fmt.Fprintf(w, "recovered.snapshot_hit=%t\n", res.SnapshotHit)

	// Sampled property round-trip: prove typed values of three kinds
	// (string, int64, timestamp) survived the full WAL -> snapshot ->
	// recovery cycle. These are deterministic for a fixed seed.
	pkg := nameOf(st.sampleCoord)
	if !g.HasNodeLabel(pkg, labelPackage) {
		return fmt.Errorf("recovered package %q lost its %s label", pkg, labelPackage)
	}
	if !g.HasNodeLabel(st.sampleCoord, labelRelease) {
		return fmt.Errorf("recovered release %q lost its %s label", st.sampleCoord, labelRelease)
	}
	if !g.HasEdgeLabel(pkg, st.sampleCoord, relPublished) {
		return fmt.Errorf("recovered edge %q-[%s]->%q lost its label", pkg, relPublished, st.sampleCoord)
	}

	if err := wantString(g, pkg, propName, pkg); err != nil {
		return err
	}
	fmt.Fprintf(w, "recovered.sample_name=%s\n", pkg)

	if err := wantInt64(g, pkg, propDownloads, st.sampleDls); err != nil {
		return err
	}
	fmt.Fprintf(w, "recovered.sample_downloads=%d\n", st.sampleDls)

	if err := wantString(g, st.sampleCoord, propCoord, st.sampleCoord); err != nil {
		return err
	}
	fmt.Fprintf(w, "recovered.sample_coord=%s\n", st.sampleCoord)

	if err := wantTime(g, st.sampleCoord, propPublishedt, st.samplePub); err != nil {
		return err
	}
	fmt.Fprintf(w, "recovered.sample_published=%s\n", st.samplePub.UTC().Format("2006-01-02"))

	// Volatile telemetry: recovery cost and the live-heap footprint of the
	// recovered graph (after a forced GC, so it reflects reachable bytes).
	fmt.Fprintf(w, "# recovery.elapsed=%s\n", elapsed.Round(time.Microsecond))
	fmt.Fprintf(w, "# recovery.wal_ops=%d\n", res.WALOps)
	fmt.Fprintf(w, "# recovery.snapshot_labels=%d\n", res.SnapshotLabels)
	fmt.Fprintf(w, "# recovery.snapshot_properties=%d\n", res.SnapshotProperties)
	fmt.Fprintf(w, "# mem.heap_before=%s\n", humanBytes(before.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_after=%s\n", humanBytes(after.HeapAlloc))
	fmt.Fprintf(w, "# mem.heap_growth=%s\n",
		humanBytes(saturatingSub(after.HeapAlloc, before.HeapAlloc)))
	return nil
}

// wantString reads a recovered string node property and returns an error
// when it is missing or does not equal want — i.e. when the value failed to
// round-trip through WAL replay and the snapshot.
func wantString(g *lpg.Graph[string, int64], node, key, want string) error {
	v, ok := g.GetNodeProperty(node, key)
	if !ok {
		return fmt.Errorf("recovered node %q lost its %s property", node, key)
	}
	if s, _ := v.String(); s != want {
		return fmt.Errorf("recovered %s.%s = %q, want %q", node, key, s, want)
	}
	return nil
}

// wantInt64 reads a recovered int64 node property and returns an error when
// it is missing or does not equal want.
func wantInt64(g *lpg.Graph[string, int64], node, key string, want int64) error {
	v, ok := g.GetNodeProperty(node, key)
	if !ok {
		return fmt.Errorf("recovered node %q lost its %s property", node, key)
	}
	if n, _ := v.Int64(); n != want {
		return fmt.Errorf("recovered %s.%s = %d, want %d", node, key, n, want)
	}
	return nil
}

// wantTime reads a recovered timestamp node property and returns an error
// when it is missing or does not equal want.
func wantTime(g *lpg.Graph[string, int64], node, key string, want time.Time) error {
	v, ok := g.GetNodeProperty(node, key)
	if !ok {
		return fmt.Errorf("recovered node %q lost its %s property", node, key)
	}
	if tm, _ := v.Time(); !tm.Equal(want) {
		return fmt.Errorf("recovered %s.%s = %v, want %v", node, key, tm, want)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Transaction helpers
// ─────────────────────────────────────────────────────────────────────────────

// txSetNode adds a node (idempotent) and attaches its label inside the
// transaction, so both are WAL-logged. SetNodeLabel implicitly creates the
// node if it is new, so an explicit AddNode is not required.
func txSetNode(tx *txn.Tx[string, int64], key, label string) error {
	if err := tx.SetNodeLabel(key, label); err != nil {
		return fmt.Errorf("SetNodeLabel %s/%s: %w", key, label, err)
	}
	return nil
}

// txAddLabeledEdge adds a weighted directed edge and tags it with relType,
// both inside the transaction. AddEdge persists the int64 weight through
// OpAddEdgeWeighted; SetEdgeLabel records the relationship type.
func txAddLabeledEdge(tx *txn.Tx[string, int64], src, dst string, weight int64, relType string) error {
	if err := tx.AddEdge(src, dst, weight); err != nil {
		return fmt.Errorf("AddEdge %s-[%s]->%s: %w", src, relType, dst, err)
	}
	if err := tx.SetEdgeLabel(src, dst, relType); err != nil {
		return fmt.Errorf("SetEdgeLabel %s-[%s]->%s: %w", src, relType, dst, err)
	}
	return nil
}

// abort rolls the transaction back and closes the WAL, wrapping the
// triggering error so a mid-batch failure leaves the half-built tx undone
// (Atomicity) and the WAL handle released.
func abort(tx *txn.Tx[string, int64], wl *wal.Writer, cause error) error {
	_ = tx.Rollback()
	_ = wl.Close()
	return cause
}

// ─────────────────────────────────────────────────────────────────────────────
// Seeded data generation. Fixed word lists keep the dataset reproducible.
// ─────────────────────────────────────────────────────────────────────────────

// packageName assembles a plausible, unique package name of the form
// "<prefix>-<noun>-<index>". The trailing index guarantees uniqueness while
// the words keep the data realistic.
func packageName(rng *rand.Rand, i int) string {
	return pkgPrefixes[rng.Intn(len(pkgPrefixes))] + "-" +
		pkgNouns[rng.Intn(len(pkgNouns))] + "-" + itoa(i)
}

// semver returns a deterministic semantic version string drawn from rng.
func semver(rng *rand.Rand) string {
	return itoa(rng.Intn(4)) + "." + itoa(rng.Intn(20)) + "." + itoa(rng.Intn(30))
}

// constraintOf returns a deterministic semver constraint string (the kind a
// dependency declaration carries), drawn from rng.
func constraintOf(rng *rand.Rand) string {
	return constraintOps[rng.Intn(len(constraintOps))] + semver(rng)
}

// versionOf extracts the version component from a "name@version" coord.
func versionOf(coord string) string {
	for i := len(coord) - 1; i >= 0; i-- {
		if coord[i] == '@' {
			return coord[i+1:]
		}
	}
	return coord
}

// nameOf extracts the package-name component from a "name@version" coord.
func nameOf(coord string) string {
	for i := len(coord) - 1; i >= 0; i-- {
		if coord[i] == '@' {
			return coord[:i]
		}
	}
	return coord
}

// publishRef is the fixed reference date the synthetic publish dates count
// back from. Anchoring to a constant — never the wall clock — keeps the
// dataset reproducible for a given -seed. Immutable after init.
var publishRef = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// publishWindowDays bounds how far before publishRef a release may be dated:
// every Release.published falls within [publishRef-publishWindowDays,
// publishRef]. ~8 years.
const publishWindowDays = 2920

// isoPublish returns a deterministic publish timestamp (midnight UTC on a
// whole-day offset back from publishRef) drawn from rng.
func isoPublish(rng *rand.Rand) time.Time {
	return publishRef.AddDate(0, 0, -rng.Intn(publishWindowDays+1))
}

// itoa is a tiny base-10 formatter for non-negative ints, avoiding a
// strconv import for the few call sites here.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

var pkgPrefixes = []string{
	"go", "lib", "core", "fast", "micro", "open", "edge", "cloud",
	"data", "net", "sync", "async", "secure", "smart", "lite", "pure",
}

var pkgNouns = []string{
	"router", "cache", "logger", "parser", "queue", "codec", "client",
	"server", "store", "stream", "buffer", "pool", "schema", "engine",
	"index", "mapper", "tracer", "metrics", "broker", "registry",
}

var languages = []string{
	"Go", "Rust", "TypeScript", "Python", "Java", "C++", "Zig", "Elixir",
}

var constraintOps = []string{"^", "~", ">=", "=", ">"}

// ─────────────────────────────────────────────────────────────────────────────
// Telemetry helpers (mirroring example 26).
// ─────────────────────────────────────────────────────────────────────────────

// readMem returns a memory snapshot after forcing a GC so HeapAlloc
// reflects live (reachable) bytes rather than floating garbage.
func readMem() runtime.MemStats {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

// rate returns count/elapsed in units per second, or 0 for a zero-length
// interval.
func rate(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// safeDiv divides a by b, returning 0 when b is 0.
func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// saturatingSub returns a-b, or 0 when b > a (a GC between the two
// snapshots can leave the "after" heap below the "before" heap).
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
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

// fileSize returns the size in bytes of the file at path, or 0 if it
// cannot be stat-ed (the byte figures are telemetry, never asserted). A
// negative size cannot occur for a regular file and is clamped to 0.
func fileSize(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < 0 {
		return 0
	}
	return uint64(fi.Size()) //nolint:gosec // G115: size is guarded non-negative just above
}

// dirSize returns the total size in bytes of every regular file under dir,
// or 0 on error. Used for the on-disk snapshot footprint telemetry.
func dirSize(dir string) uint64 {
	var total uint64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort byte accounting for telemetry only
		}
		if !info.IsDir() && info.Size() > 0 {
			total += uint64(info.Size()) //nolint:gosec // G115: size is guarded positive in this branch
		}
		return nil
	})
	return total
}
