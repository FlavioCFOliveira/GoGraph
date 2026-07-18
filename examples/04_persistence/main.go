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
//  4. Atomicity on the abort path is demonstrated deliberately: two extra
//     transactions run on the same store just before the snapshot — a
//     committed "survivor" publication that must persist, and a "phantom"
//     publication whose release accidentally declares a DEPENDS_ON its own
//     owning package (a self-dependency the supply-chain invariant forbids),
//     so it is intentionally aborted with Tx.Rollback instead of committed.
//     After the reopen the recovered graph must equal the pre-transaction
//     state exactly: the survivor is present, none of the phantom's nodes,
//     edges, labels or properties are, and the WAL holds no OpCommit marker
//     (and not one byte) of the aborted transaction. A rolled-back mutation
//     that survived would be an Atomicity violation and is surfaced as a
//     fatal error, not hidden.
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
	"bytes"
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

	"github.com/FlavioCFOliveira/GoGraph/graph"
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

// Fixed identities for the atomicity demonstration. They are deliberately
// literal (never seed-derived) so the survivor and phantom are byte-stable
// across seeds, and shaped so they cannot collide with a generated package
// name (which is always "<prefix>-<noun>-<index>" with a numeric suffix).
const (
	// survivor* names the committed transaction that must survive the reopen.
	survivorPkgKey           = "hotfix-security-pkg"
	survivorRelKey           = "hotfix-security-pkg@2.0.0"
	survivorVersion          = "2.0.0"
	survivorDownloads  int64 = 4_200_000
	survivorConstraint       = "^1.0.0"

	// phantom* names the intentionally aborted transaction that must leave no
	// trace. Its release declares a self-dependency on its own owning package,
	// which violates the supply-chain invariant and triggers the rollback.
	phantomPkgKey           = "phantom-rogue-pkg"
	phantomRelKey           = "phantom-rogue-pkg@9.9.9"
	phantomVersion          = "9.9.9"
	phantomDownloads  int64 = 9_000_000
	phantomConstraint       = "^9.0.0"
)

// Fixed publish timestamps for the survivor and phantom releases, anchored to
// constants rather than the wall clock so the demonstration stays reproducible.
// Immutable after init.
var (
	survivorPub = time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC)
	phantomPub  = time.Date(2023, time.March, 15, 0, 0, 0, 0, time.UTC)
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

	// Phase 1: build the graph through WAL-committed transactions, then run the
	// committed-survivor / aborted-phantom atomicity contrast on the same store.
	stats, rb, err := commit(ctx, dir, cfg, w)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 2: drop every in-memory reference and rebuild from disk, proving
	// the committed data survived and the aborted transaction left no trace.
	if err := restore(ctx, dir, &stats, &rb, w); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// commitStats reports the realised shape of the write phase (the random
// degrees mean the edge total is not known until the graph is built) plus
// the wall-clock cost, the on-disk footprint, and a sample package coord
// used to anchor the recovered-property assertions.
type commitStats struct {
	samplePub   time.Time
	sampleCoord string // coord of release[0], anchors the recovered-value facts
	packages    int
	releases    int
	publishedE  int
	dependsOnE  int
	commits     int
	elapsed     time.Duration
	walBytes    uint64
	snapBytes   uint64
	sampleDls   int64 // package[0].downloads, asserted after recovery
}

// rollbackStats captures the atomicity demonstration: the committed survivor,
// the intentionally aborted phantom, the pre-transaction baseline the abort
// must not disturb, and the WAL's structural proof that the abort wrote
// nothing. The pre-tx baseline is snapshotted from the in-memory graph after
// the survivor commit and before the phantom transaction, so it is the durable
// state as of the last commit — exactly what the recovered graph must equal.
type rollbackStats struct {
	survivorDep string // the DEPENDS_ON target of the committed survivor
	phantomDep  string // the (self-referential) DEPENDS_ON target of the phantom

	opsAttempted  int // mutations buffered by the aborted phantom transaction (K)
	walMarkers    int // OpCommit markers in the WAL (genesis commits + survivor)
	phantomFrames int // WAL frames whose payload contains the phantom name (must be 0)

	preNodes     uint64 // in-memory node count at the pre-tx baseline
	preEdges     uint64 // in-memory edge count at the pre-tx baseline
	preLabels    int    // distinct node labels in use at the pre-tx baseline
	preNodeProps int    // total node properties at the pre-tx baseline
}

// commit builds the seeded supply-chain graph entirely through WAL
// transactions -- one committed transaction per package (package node,
// release node, and their PUBLISHED/DEPENDS_ON edges) -- then runs the
// committed-survivor / aborted-phantom atomicity contrast on the same store,
// and takes a v2 snapshot. It returns the realised shape, the atomicity
// demonstration's facts, and the on-disk byte footprint. The write loop polls
// ctx for cancellation every cfg.batch packages.
//
//nolint:gocyclo // one linear build pipeline: nodes+labels+props, then edges+props, one tx per package.
func commit(ctx context.Context, dir string, cfg config, w io.Writer) (commitStats, rollbackStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional — the example must
	// reproduce a fixed dataset for a given -seed; crypto/rand would defeat that.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	walPath := filepath.Join(dir, "wal")
	wl, err := wal.Open(walPath)
	if err != nil {
		return commitStats{}, rollbackStats{}, fmt.Errorf("wal.Open: %w", err)
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
				return commitStats{}, rollbackStats{}, err
			}
		}
		tx := store.Begin()

		// Package node, its label and typed properties.
		pkg, rel := pkgKeys[i], relKeys[i]
		if err := txSetNode(tx, pkg, labelPackage); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, err)
		}
		if err := tx.SetNodeProperty(pkg, propName, lpg.StringValue(pkg)); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set %s.name: %w", pkg, err))
		}
		if err := tx.SetNodeProperty(pkg, propLanguage, lpg.StringValue(languages[i%len(languages)])); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set %s.language: %w", pkg, err))
		}
		if err := tx.SetNodeProperty(pkg, propDownloads, lpg.Int64Value(downloads[i])); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set %s.downloads: %w", pkg, err))
		}

		// Release node, its label and typed properties.
		published := isoPublish(rng)
		if i == 0 {
			st.samplePub = published
		}
		if err := txSetNode(tx, rel, labelRelease); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, err)
		}
		if err := tx.SetNodeProperty(rel, propCoord, lpg.StringValue(rel)); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set %s.coord: %w", rel, err))
		}
		if err := tx.SetNodeProperty(rel, propVersion, lpg.StringValue(versionOf(rel))); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set %s.version: %w", rel, err))
		}
		if err := tx.SetNodeProperty(rel, propPublishedt, lpg.TimeValue(published)); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set %s.published: %w", rel, err))
		}

		// PUBLISHED edge: the package owns its release (weight = 1).
		if err := txAddLabeledEdge(tx, pkg, rel, 1, relPublished); err != nil {
			return commitStats{}, rollbackStats{}, abort(tx, wl, err)
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
				return commitStats{}, rollbackStats{}, abort(tx, wl, err)
			}
			if err := tx.SetEdgeProperty(rel, pkgKeys[j], propConstraint, lpg.StringValue(constraintOf(rng))); err != nil {
				return commitStats{}, rollbackStats{}, abort(tx, wl, fmt.Errorf("set constraint: %w", err))
			}
			st.dependsOnE++
			rank++
		}

		if err := tx.Commit(); err != nil {
			_ = wl.Close()
			return commitStats{}, rollbackStats{}, fmt.Errorf("commit tx at package %d: %w", i, err)
		}
		st.commits++
	}

	// Atomicity demonstration on the same store and WAL: commit a survivor
	// publication, then attempt a phantom publication that violates the
	// supply-chain invariant and abort it with Rollback. The survivor must
	// survive the reopen; the phantom must leave no trace.
	rb, err := demonstrateAtomicRollback(store, g, pkgKeys[0])
	if err != nil {
		_ = wl.Close()
		return commitStats{}, rollbackStats{}, fmt.Errorf("atomic-rollback demo: %w", err)
	}
	st.commits++ // the committed survivor transaction
	_ = wl.Close()

	// Take a v2 snapshot (CSR + labels.bin + properties.bin) as a checkpoint
	// alongside the WAL, the same protocol a background checkpointer uses. It
	// reflects the graph after the survivor commit and the phantom rollback, so
	// the phantom is absent from the checkpoint by construction.
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		return commitStats{}, rollbackStats{}, fmt.Errorf("WriteSnapshotFull: %w", err)
	}

	// Structural proof, read straight from the closed WAL: exactly one OpCommit
	// marker per committed transaction (genesis + survivor) and not a single
	// byte of the aborted phantom anywhere in the log.
	rb.walMarkers, rb.phantomFrames, err = inspectWAL(walPath, phantomPkgKey)
	if err != nil {
		return commitStats{}, rollbackStats{}, fmt.Errorf("inspectWAL: %w", err)
	}

	st.elapsed = time.Since(start)
	st.walBytes = fileSize(walPath)
	st.snapBytes = dirSize(snapDir)

	// Deterministic facts: the realised shape the recovery phase must match.
	fmt.Fprintf(w, "nodes.packages=%d\n", st.packages)
	fmt.Fprintf(w, "nodes.releases=%d\n", st.releases)
	fmt.Fprintf(w, "edges.published=%d\n", st.publishedE)
	fmt.Fprintf(w, "edges.depends_on=%d\n", st.dependsOnE)

	// Deterministic facts of the atomicity demonstration.
	fmt.Fprintf(w, "survivor.committed=1\n")
	fmt.Fprintf(w, "rollback.ops_attempted=%d\n", rb.opsAttempted)
	fmt.Fprintf(w, "wal.commit_markers=%d\n", rb.walMarkers)
	fmt.Fprintf(w, "wal.phantom_frames=%d\n", rb.phantomFrames)

	// Fail-stop: the WAL must carry one marker per committed transaction and no
	// trace of the aborted one. A mismatch means the rolled-back transaction
	// leaked to the durable log — an Atomicity violation, surfaced, not hidden.
	if rb.walMarkers != st.commits {
		return commitStats{}, rollbackStats{}, fmt.Errorf(
			"ATOMICITY VIOLATION: WAL holds %d OpCommit markers, want %d (one per committed transaction); the aborted transaction may have leaked a marker",
			rb.walMarkers, st.commits)
	}
	if rb.phantomFrames != 0 {
		return commitStats{}, rollbackStats{}, fmt.Errorf(
			"ATOMICITY VIOLATION: the aborted transaction's identity appears in %d WAL frame(s); a rolled-back mutation reached the durable log",
			rb.phantomFrames)
	}

	// Volatile telemetry: write throughput and on-disk footprint.
	edges := st.publishedE + st.dependsOnE
	fmt.Fprintf(w, "# commit.elapsed=%s\n", st.elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "# commit.tx_rate=%.0f tx/s\n", rate(st.commits, st.elapsed))
	fmt.Fprintf(w, "# commit.edge_rate=%.0f edges/s\n", rate(edges, st.elapsed))
	fmt.Fprintf(w, "# disk.wal_bytes=%s\n", humanBytes(st.walBytes))
	fmt.Fprintf(w, "# disk.snapshot_bytes=%s\n", humanBytes(st.snapBytes))
	fmt.Fprintf(w, "# disk.bytes_per_edge=%.1f\n", safeDiv(float64(st.walBytes+st.snapBytes), float64(edges)))
	return st, rb, nil
}

// restore drops the in-memory graph, rebuilds it from disk with
// recovery.Open, and verifies the recovered shape and a few sampled
// property values against the write phase. Deterministic facts are the
// recovered counts and sampled values; telemetry is the recovery
// wall-clock and the live heap before vs after recovery.
//
//nolint:gocyclo // the recovery-verification step is a flat sequence of independent property round-trip checks.
func restore(ctx context.Context, dir string, st *commitStats, rb *rollbackStats, w io.Writer) error {
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

	// ── Atomicity of the aborted transaction ─────────────────────────────────
	// Count how many of the phantom transaction's entities (nodes, labels, node
	// property, both edges, edge property) survived into the recovered graph.
	// Atomicity on the abort path demands zero.
	applied := countPhantomSurvivors(g, adj, rb)
	fmt.Fprintf(w, "rollback.applied_after_reopen=%d\n", applied)

	// The recovered graph must equal the pre-transaction baseline exactly —
	// same node, edge, label and node-property counts.
	recProps := countNodeProperties(g, adj)
	matches := adj.Order() == rb.preNodes &&
		adj.Size() == rb.preEdges &&
		len(g.NodeLabelsInUse()) == rb.preLabels &&
		recProps == rb.preNodeProps
	fmt.Fprintf(w, "state.matches_pre_tx=%d\n", b2i(matches))

	// The committed survivor, by contrast, must be fully present.
	survived := survivorPresent(g, adj, rb)
	fmt.Fprintf(w, "survivor.present=%d\n", b2i(survived))

	// Fail-stop: a rolled-back mutation that survived, a pre-tx baseline that
	// drifted, or a lost committed survivor is an ACID violation — surface it
	// with the concrete divergence rather than reporting success.
	if applied != 0 {
		return fmt.Errorf(
			"ATOMICITY VIOLATION: %d entit(y/ies) of the rolled-back transaction (%s / %s) survived into the recovered graph",
			applied, phantomPkgKey, phantomRelKey)
	}
	if !matches {
		return fmt.Errorf(
			"ATOMICITY VIOLATION: recovered state diverged from the pre-tx baseline (nodes %d want %d, edges %d want %d, labels %d want %d, node-props %d want %d)",
			adj.Order(), rb.preNodes, adj.Size(), rb.preEdges,
			len(g.NodeLabelsInUse()), rb.preLabels, recProps, rb.preNodeProps)
	}
	if !survived {
		return fmt.Errorf(
			"DURABILITY VIOLATION: the committed survivor %q was lost after reopen", survivorPkgKey)
	}

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
// Atomicity demonstration: a committed survivor vs an aborted phantom
// ─────────────────────────────────────────────────────────────────────────────

// demonstrateAtomicRollback runs the committed-survivor / aborted-phantom
// contrast on store, sharing the same WAL as the genesis build. It commits a
// well-formed "survivor" publication (which must persist), snapshots the
// in-memory graph as the pre-transaction baseline, then buffers a "phantom"
// publication and aborts it with Rollback because its release declares a
// DEPENDS_ON its own owning package — a self-dependency the supply-chain
// invariant forbids. survivorDep is an existing package the survivor legitimately
// depends on (genesis package 0). It returns the demonstration's facts; the WAL
// marker/phantom-frame counts are filled in by the caller after the WAL closes.
func demonstrateAtomicRollback(store *txn.Store[string, int64], g *lpg.Graph[string, int64], survivorDep string) (rollbackStats, error) {
	rb := rollbackStats{survivorDep: survivorDep, phantomDep: phantomPkgKey}

	// (a) The committed survivor — a routine publication that satisfies every
	// invariant, so it commits and must survive the reopen.
	if err := commitSurvivor(store, survivorDep); err != nil {
		return rollbackStats{}, fmt.Errorf("survivor: %w", err)
	}

	// Capture the pre-transaction baseline: the durable state as of the last
	// commit, against which the aborted transaction must leave no trace.
	adj := g.AdjList()
	rb.preNodes = adj.Order()
	rb.preEdges = adj.Size()
	rb.preLabels = len(g.NodeLabelsInUse())
	rb.preNodeProps = countNodeProperties(g, adj)

	// (b) The aborted phantom — a publication buffered in full, then rolled
	// back because it violates the supply-chain invariant.
	k, err := attemptPhantom(store, phantomPkgKey, rb.phantomDep)
	if err != nil {
		return rollbackStats{}, fmt.Errorf("phantom: %w", err)
	}
	rb.opsAttempted = k
	return rb, nil
}

// commitSurvivor buffers a well-formed publication and commits it, so the whole
// transaction becomes durable and must survive the reopen. survivorDep is the
// existing package it declares a DEPENDS_ON edge to.
func commitSurvivor(store *txn.Store[string, int64], survivorDep string) error {
	tx := store.Begin()
	if _, err := buildPublication(tx, survivorPkgKey, survivorRelKey,
		survivorDownloads, survivorVersion, survivorPub, survivorDep, survivorConstraint); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("build: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// attemptPhantom buffers a full publication for owningPkg/phantomRelKey whose
// release declares a DEPENDS_ON edge to depTarget, then enforces the
// supply-chain invariant that a release must not depend on its own owning
// package. Because depTarget is owningPkg here, the invariant is violated and
// the whole transaction is rolled back: Atomicity guarantees none of the k
// buffered mutations reach the WAL or the graph. It returns k, the number of
// mutations that were buffered before the abort.
func attemptPhantom(store *txn.Store[string, int64], owningPkg, depTarget string) (int, error) {
	tx := store.Begin()
	k, err := buildPublication(tx, owningPkg, phantomRelKey,
		phantomDownloads, phantomVersion, phantomPub, depTarget, phantomConstraint)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("build: %w", err)
	}
	// Consistency invariant: a Release must not DEPENDS_ON its own owning
	// Package. When the buffered publication violates it, abort before commit.
	if depTarget == owningPkg {
		if rerr := tx.Rollback(); rerr != nil {
			return 0, fmt.Errorf("rollback: %w", rerr)
		}
		return k, nil
	}
	if cerr := tx.Commit(); cerr != nil {
		return 0, fmt.Errorf("commit: %w", cerr)
	}
	return k, nil
}

// buildPublication buffers a complete package publication into tx — the Package
// and Release nodes with their labels and typed properties, the PUBLISHED edge,
// and one DEPENDS_ON edge (with a version-constraint property) to depTarget —
// and returns the number of mutations buffered. It neither commits nor rolls
// back: the caller owns the transaction's fate, so the committed survivor and
// the aborted phantom go through this one builder and differ only in whether
// the caller calls Commit or Rollback.
func buildPublication(tx *txn.Tx[string, int64], pkg, rel string, downloads int64, version string, published time.Time, depTarget, constraint string) (int, error) {
	ops := 0
	add := func(err error) error {
		if err == nil {
			ops++
		}
		return err
	}

	// Package node, label and typed properties.
	if err := add(tx.SetNodeLabel(pkg, labelPackage)); err != nil {
		return ops, fmt.Errorf("pkg label: %w", err)
	}
	if err := add(tx.SetNodeProperty(pkg, propName, lpg.StringValue(pkg))); err != nil {
		return ops, fmt.Errorf("pkg name: %w", err)
	}
	if err := add(tx.SetNodeProperty(pkg, propLanguage, lpg.StringValue("Go"))); err != nil {
		return ops, fmt.Errorf("pkg language: %w", err)
	}
	if err := add(tx.SetNodeProperty(pkg, propDownloads, lpg.Int64Value(downloads))); err != nil {
		return ops, fmt.Errorf("pkg downloads: %w", err)
	}

	// Release node, label and typed properties.
	if err := add(tx.SetNodeLabel(rel, labelRelease)); err != nil {
		return ops, fmt.Errorf("rel label: %w", err)
	}
	if err := add(tx.SetNodeProperty(rel, propCoord, lpg.StringValue(rel))); err != nil {
		return ops, fmt.Errorf("rel coord: %w", err)
	}
	if err := add(tx.SetNodeProperty(rel, propVersion, lpg.StringValue(version))); err != nil {
		return ops, fmt.Errorf("rel version: %w", err)
	}
	if err := add(tx.SetNodeProperty(rel, propPublishedt, lpg.TimeValue(published))); err != nil {
		return ops, fmt.Errorf("rel published: %w", err)
	}

	// PUBLISHED edge: the package owns its release (weight = 1).
	if err := add(tx.AddEdge(pkg, rel, 1)); err != nil {
		return ops, fmt.Errorf("PUBLISHED edge: %w", err)
	}
	if err := add(tx.SetEdgeLabel(pkg, rel, relPublished)); err != nil {
		return ops, fmt.Errorf("PUBLISHED label: %w", err)
	}

	// DEPENDS_ON edge to depTarget, with a version-constraint property.
	if err := add(tx.AddEdge(rel, depTarget, 1)); err != nil {
		return ops, fmt.Errorf("DEPENDS_ON edge: %w", err)
	}
	if err := add(tx.SetEdgeLabel(rel, depTarget, relDependsOn)); err != nil {
		return ops, fmt.Errorf("DEPENDS_ON label: %w", err)
	}
	if err := add(tx.SetEdgeProperty(rel, depTarget, propConstraint, lpg.StringValue(constraint))); err != nil {
		return ops, fmt.Errorf("DEPENDS_ON constraint: %w", err)
	}

	return ops, nil
}

// inspectWAL reads the closed WAL back frame by frame and returns the number of
// OpCommit markers it holds and the number of frames whose raw payload contains
// phantom (the aborted transaction's package name — the string codec stores raw
// UTF-8, so a committed phantom op would leave its bytes here). For a correct
// module the marker count equals the number of committed transactions and the
// phantom-frame count is zero: the aborted transaction wrote neither a marker
// nor a single op frame.
func inspectWAL(walPath, phantom string) (markers, phantomFrames int, err error) {
	r, err := wal.OpenReader(walPath)
	if err != nil {
		return 0, 0, fmt.Errorf("wal.OpenReader: %w", err)
	}
	defer func() { _ = r.Close() }()

	needle := []byte(phantom)
	for f := range r.Frames() {
		if op, derr := recovery.Decode(f.Payload); derr == nil && op.Kind == txn.OpCommit {
			markers++
		}
		if bytes.Contains(f.Payload, needle) {
			phantomFrames++
		}
	}
	if te := r.TailError(); te != nil {
		return markers, phantomFrames, fmt.Errorf("wal tail: %w", te)
	}
	return markers, phantomFrames, nil
}

// countNodeProperties returns the total number of node properties in g, walking
// every interned node via the mapper. It is used to snapshot the pre-tx
// baseline and to confirm the recovered graph carries exactly the same number
// of node properties — a rolled-back property write would inflate the count.
func countNodeProperties(g *lpg.Graph[string, int64], adj *adjlist.AdjList[string, int64]) int {
	total := 0
	adj.Mapper().Walk(func(id graph.NodeID, _ string) bool {
		g.NodePropertiesByIDFunc(id, func(string, lpg.PropertyValue) { total++ })
		return true
	})
	return total
}

// countPhantomSurvivors counts how many of the aborted phantom transaction's
// entities are present in the recovered graph: the two nodes, the node label,
// a node property, both edges, and the edge property. Atomicity on the abort
// path requires the result to be zero.
func countPhantomSurvivors(g *lpg.Graph[string, int64], adj *adjlist.AdjList[string, int64], rb *rollbackStats) int {
	n := 0
	if _, ok := adj.Mapper().Lookup(phantomPkgKey); ok {
		n++ // phantom package node
	}
	if _, ok := adj.Mapper().Lookup(phantomRelKey); ok {
		n++ // phantom release node
	}
	if g.HasNodeLabel(phantomPkgKey, labelPackage) {
		n++ // phantom node label
	}
	if _, ok := g.GetNodeProperty(phantomPkgKey, propName); ok {
		n++ // phantom node property
	}
	if adj.HasEdge(phantomPkgKey, phantomRelKey) {
		n++ // phantom PUBLISHED edge
	}
	if adj.HasEdge(phantomRelKey, rb.phantomDep) {
		n++ // phantom DEPENDS_ON (self-dependency) edge
	}
	if _, ok := g.GetEdgeProperty(phantomRelKey, rb.phantomDep, propConstraint); ok {
		n++ // phantom edge property
	}
	return n
}

// survivorPresent reports whether every entity of the committed survivor
// transaction is present in the recovered graph — the two nodes, both labels,
// a node property, both edges, and the edge property.
func survivorPresent(g *lpg.Graph[string, int64], adj *adjlist.AdjList[string, int64], rb *rollbackStats) bool {
	if _, ok := adj.Mapper().Lookup(survivorPkgKey); !ok {
		return false
	}
	if !g.HasNodeLabel(survivorPkgKey, labelPackage) {
		return false
	}
	if _, ok := g.GetNodeProperty(survivorPkgKey, propName); !ok {
		return false
	}
	if !g.HasNodeLabel(survivorRelKey, labelRelease) {
		return false
	}
	if !adj.HasEdge(survivorPkgKey, survivorRelKey) { // PUBLISHED
		return false
	}
	if !g.HasEdgeLabel(survivorPkgKey, survivorRelKey, relPublished) {
		return false
	}
	if !adj.HasEdge(survivorRelKey, rb.survivorDep) { // DEPENDS_ON
		return false
	}
	if _, ok := g.GetEdgeProperty(survivorRelKey, rb.survivorDep, propConstraint); !ok {
		return false
	}
	return true
}

// b2i maps a boolean invariant to a deterministic 1/0 fact value.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
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
