# Production-Readiness Test Battery

This document describes the test infrastructure and shape-generator
catalogue that together form GoGraph's production-readiness test battery.
It is the single authoritative reference for anyone who wants to extend
the suite, add a new shape, or integrate a new algorithm into the
correctness pipeline.

For the three-layer test discipline and CI integration, see
[docs/test-layers.md](test-layers.md). For the Deterministic Simulation Testing
harness — the seed-reproducible, VOPR-modelled simulator that drives the engine
through randomised operations and faults and checks it against a
correct-by-construction oracle (including the `search/` algorithm battery) — see
[docs/dst.md](dst.md). For the isolation harness — the scripted, EXHAUSTIVE,
deterministic enumeration of a concurrent scenario's interleavings, modelled on
PostgreSQL's `src/test/isolation` — see
[docs/isolation-harness.md](isolation-harness.md). The two are complementary:
DST samples a large space at random, while the isolation harness certifies a
small one completely.

---

## Table of contents

1. [Architecture overview](#architecture-overview)
2. [The Shape interface and `internal/shapegen`](#the-shape-interface-and-internalshapegen)
3. [Shape catalogue](#shape-catalogue)
4. [Real-world dataset loaders](#real-world-dataset-loaders)
5. [Invariant checkers (`internal/invariants`)](#invariant-checkers-internalinvariants)
6. [Fault-injection packages](#fault-injection-packages)
   - [`internal/testfs`](#internaltestfs)
   - [`internal/crashpoint` and `internal/crashinject`](#internalcrashpoint-and-internalcrashinject)
   - [`internal/subproc`](#internalsubproc)
7. [Golden-file helper (`internal/goldens`)](#golden-file-helper-internalgoldens)
   - [Isolation harness (`internal/isolationtest`)](#isolation-harness-internalisolationtest)
   - [Anomaly classifier (`internal/anomaly`)](#anomaly-classifier-internalanomaly)
8. [Test layers quick-reference](#test-layers-quick-reference)
9. [Add-new-shape recipe](#add-new-shape-recipe)

---

## Architecture overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Test battery                                 │
│                                                                     │
│  ┌─────────────────┐   ┌──────────────────┐   ┌─────────────────┐  │
│  │  Shape catalogue│   │ Invariant checkers│   │  Dataset loaders│  │
│  │ internal/shapegen│  │ internal/invariants│  │  SNAP / LDBC    │  │
│  └────────┬────────┘   └────────┬─────────┘   └────────┬────────┘  │
│           │                     │                       │           │
│           └─────────────────────┼───────────────────────┘           │
│                                 │                                   │
│                    ┌────────────▼────────────┐                     │
│                    │    Property-based tests  │                     │
│                    │  (pgregory.net/rapid)    │                     │
│                    └────────────┬────────────┘                     │
│                                 │                                   │
│            ┌────────────────────┼───────────────────┐              │
│            │                    │                   │              │
│  ┌─────────▼──────┐  ┌──────────▼────────┐  ┌──────▼──────────┐  │
│  │ internal/testfs │  │crashpoint+crashinject│ │internal/subproc │  │
│  │ (FS faults)     │  │ (SIGKILL harness)  │  │ (child procs)   │  │
│  └─────────────────┘  └───────────────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

---

## The Shape interface and `internal/shapegen`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/shapegen`

Every graph shape is represented by the `Shape` interface, which is
generic on the node type `N` and the edge weight type `W` so it can
produce graphs compatible with any LPG specialisation in the module:

```go
type Shape[N comparable, W any] interface {
    Name() string
    Build(cfg adjlist.Config) (*lpg.Graph[N, W], error)
    Knobs() []Knob
}
```

- **`Name()`** — the canonical catalogue identifier of the shape: a
  dotted, family-prefixed, parameter-free string (e.g. `"trivial.empty"`,
  `"classic.path"`, `"random.erdos-renyi-np"`, `"specials.petersen"`).
  Because the name omits the knob values, two `Path` shapes built with
  different `n` share one name and therefore one registry key.
- **`Build(cfg)`** — constructs and returns a new graph on every call.
  The result must be reproducible given the same parameters and seed.
  Build is safe for concurrent calls.
- **`Knobs()`** — returns the parameter space as a slice of `Knob` values,
  each with a `Name`, `Min`, `Max`, and `Default`. Used by rapid-based
  property tests to sweep the parameter space.

### Concurrency contract

All `Build` implementations are safe for concurrent calls. Each call
creates an independent graph; there is no shared state between calls.

### Registry and knob sweeps

`shapegen` maintains a process-local typed registry keyed by `Shape`
`Name()` and its `(N, W)` specialisation, guarded by a `sync.RWMutex`.
`Register[N, W]`, `Lookup[N, W]`, and `Unregister[N, W]` are the only
mutation surface; tests that register transient shapes clean up via
`t.Cleanup`. `MakeKnobValues(g *rapid.T, knobs []Knob) []int` draws one
integer per knob — in declaration order, using each knob's `Name` as
its rapid label — so property-based tests can sweep the parameter space
with self-describing counter-examples.

No catalogue shape registers itself: `Register` is exercised only by
`internal/shapegen/shapegen_test.go`, and nothing in the module enumerates the
registry. It is a working, tested facility with no production consumer, which
is why step 6 of the [add-new-shape recipe](#add-new-shape-recipe) has nothing
to ask a new family to do.

---

## Shape catalogue

The following families are implemented in `internal/shapegen`. Each row lists
the file's exported constructors, which is what the catalogue actually offers;
`snap.go` and `graphalytics.go` are the dataset loaders and are described in
[Real-world dataset loaders](#real-world-dataset-loaders) instead.

| File | Family | Exported constructors |
|---|---|---|
| `trivial.go` | Degenerate | `EmptyGraph`, `SingleNode`, `SingleEdge` (K₂, or the lone self-loop when `selfLoop` is set), `ParallelDigon`, `IsolatedOnly`, `UniversalSelfLoops` |
| `classic.go` | Classic | `Path` Pₙ, `Cycle` Cₙ, `Star` Sₙ, `DoubleStar`, `Complete` Kₙ, `CompleteBipartite` Kₘ,ₙ, `Multipartite` |
| `trees.go` | Trees | `BalancedBinary`, `CompleteKAry`, `PruferTree`, `PathDegenerate`, `Caterpillar`, `Spider`, `Lobster` |
| `structured.go` | Structured 2D/3D | `Hypercube` Qₙ, `Grid`, `Torus`, `Rook`, `Mobius` (the Möbius ladder Mₙ), `Ladder`, `Prism`, `Theta` |
| `specials.go` | Named sparse specials | `Petersen`, `Dodecahedral`, `GoldnerHarary`, `MoserSpindle`, `Kneser` |
| `erdos_renyi.go` | Random | `ErdosRenyiNP` G(n,p), `ErdosRenyiNM` G(n,m) |
| `barabasi_albert.go` | Scale-free | `BarabasiAlbert` preferential attachment |
| `watts_strogatz.go` | Small-world | `WattsStrogatz` rewiring |
| `configmodel.go` | Degree sequences | `RandomRegular`, `ConfigurationModel` |
| `sbm.go` | Community | `SBM` (stochastic block model), `PlantedPartition` |
| `lfr.go` | Community | `LFR` community benchmark |
| `rmat.go` | Synthetic large | `RMAT`; `RMATPick` exposes the quadrant draw on its own |
| `rgg.go` | Geometric | `RGG` random geometric graph |
| `dags.go` | DAGs | `TransitiveTournament`, `Diamond`, `Layered`, `LengauerTarjanExample`, `BuildDepDAG`; plus `NegativeWeightAcyclic` |
| `adversarial.go` | Adversarial property values | `AdversarialIntWeights`, `AdversarialFloatWeights`, `AdversarialStrings`, `AdversarialBytes`, `AdversarialTimes`, `AdversarialBools`, `AllMix`, `ApplyAdversarialProps` |
| `mapperadv.go` | Adversarial natural keys | `GenerateShardZeroKeys` — a flood of distinct keys that all hash to mapper shard 0 |

`adversarial.go` and `mapperadv.go` are the two exceptions to the pattern: they
return property values and key material rather than a `Shape`, and are applied
to a graph built by one of the other families.

---

## Real-world dataset loaders

### SNAP datasets (`snap.go`)

Package-level loader functions for Stanford Network Analysis Project
(SNAP) datasets. Datasets are fetched from
`https://snap.stanford.edu/data/` on first use and cached under
`$GOGRAPH_SNAP_DIR`; when that is unset the cache is
`$HOME/.cache/gograph-snap`, falling back to `$TMPDIR/gograph-snap` when the
home directory cannot be resolved. Every archive is verified against the
SHA-256 pinned in `SNAPDatasets`; a mismatch is `ErrSNAPChecksumMismatch`, a
failed fetch is `ErrSNAPOffline`, and an unregistered name is
`ErrSNAPUnknownDataset`. `LoadSNAP(name, cacheDir)` is the generic form of the
three named loaders.

| Function | Dataset | Nodes | Edges | Layer |
|---|---|---|---|---|
| `CitHepPh(cacheDir)` | cit-HepPh | 34 546 | 421 578 | soak |
| `WebGoogle(cacheDir)` | web-Google | 875 713 | 5 105 039 | soak |
| `SocLiveJournal1(cacheDir)` | soc-LiveJournal1 | 4 847 571 | 68 993 773 | nightly |

### LDBC Graphalytics datasets (`graphalytics.go`)

Package-level loader `LoadGraphalytics(name, cacheDir)` and reference
output accessor `LoadGraphalyticsReference(name, alg, cacheDir)`. Data is
fetched from the SURF Data Repository and cached under
`$GOGRAPH_GRAPHALYTICS_DIR`. Because the repository keeps these archives on
cold storage, a request for a file that has not been staged answers HTTP 409,
which is surfaced as `ErrGraphalyticsStaging`; `ErrGraphalyticsOffline`,
`ErrGraphalyticsChecksumMismatch`, `ErrGraphalyticsUnknownDataset` and
`ErrGraphalyticsUnknownAlgorithm` are the other typed outcomes. Archives are
verified against the SHA-256 in `GraphalyticsDatasets` when one is recorded,
and against the repository's published MD5 otherwise — no registered dataset
carries a SHA-256 yet, so all three currently verify by MD5.

| Dataset | Nodes | Edges | Algorithms | Layer |
|---|---|---|---|---|
| `dota-league` | 61 170 | 50 870 313 | BFS,CDLP,LCC,PR,SSSP,WCC | soak |
| `kgs` | 832 247 | 17 891 698 | BFS,CDLP,LCC,PR,SSSP,WCC | soak |
| `cit-Patents` | 3 774 768 | 16 518 947 | BFS,CDLP,LCC,PR,SSSP,WCC | soak |

---

## Invariant checkers (`internal/invariants`)

Package: `github.com/FlavioCFOliveira/GoGraph/internal/invariants`

| Function | What it checks | Counter-example on failure |
|---|---|---|
| `AssertConnected[N,W](t, g)` | WCC count == 1 | number of components |
| `AssertDAG[N,W](t, g)` | no directed cycle, no self-loop | first SCC (up to 5 nodes) |
| `AssertBipartite[N,W](t, g)` | 2-colourable via BFS | offending edge (u, v) |
| `AssertDistanceBound[W](t, bfs, dijkstra)` | BFS depth ≤ Dijkstra dist | offending node + both distances |
| `AssertShapeEqual[N,W](t, a, b)` | Order, Size, edge sets identical | first missing/extra edge |

All helpers call `t.Errorf` (not `t.Fatalf`) so multiple invariants can
be checked in a single test body with all failures accumulated.

`BuildBFSDepths[W](ctx, csr, src)` is a convenience function that runs BFS
from `src` over a `*csr.CSR[W]` and returns
`(map[graph.NodeID]int, error)` for use with `AssertDistanceBound`.

Each checker is exercised on real generator output by
`internal/shapegen/invariants_battery_test.go` (the four topology checkers
run on known-topology shapes — path, even cycle, star, directed path,
balanced binary tree; `AssertDistanceBound` runs on a unit-weighted path,
since the shapegen catalogue uses a sentinel edge weight of 0). A meta-test,
`internal/invariants.TestInvariantsHasExternalImporter`, fails if the package
ever loses its last external consumer, so the checkers cannot silently revert
to paper coverage.

---

## Fault-injection packages

### `internal/testfs`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/testfs`

`FaultFile` wraps `*os.File` with configurable fault injection. It
implements `testfs.File`, the minimal filesystem interface used by the
`store/wal` and `store/snapshot` write paths, and accepted by
`store/wal.OpenWith`.

```go
type Faults struct {
    FailWritesAfterBytes int64          // Write fails with ErrPartialWrite past N cumulative bytes
    ReturnENOSPC         bool           // all writes return syscall.ENOSPC
    FsyncDelay           time.Duration  // sleep before each Sync
    FailSyncAfter        int            // first N Syncs succeed, then ErrSyncFailed
    ReturnEIOOnSync      bool           // every Sync fails with ErrSyncFailed
    CorruptOnRead        func(offset, n int64) bool // invert the first byte of the read buffer
}

ff, _ := testfs.New(path, testfs.Faults{FailWritesAfterBytes: 128})
w, _ := wal.OpenWith(ff)  // inject fault into WAL writer
```

The two sync faults model the post-"fsyncgate" kernel contract: when the fault
fires, the bytes written since the last successful `Sync` are discarded, so the
file is left holding exactly the durable prefix a crash would preserve.

`IsENOSPC(err)` reports `syscall.ENOSPC`, unwrapping an `*os.PathError` first.
`FaultFile` is safe for concurrent `Read`/`Write`/`Seek`/`Sync`/`Truncate`/
`Close`; all mutations serialise on an internal mutex.

### `internal/crashpoint` and `internal/crashinject`

The crash-injection machinery is split across two packages so that
production code never links the `testing` package or the subprocess
runner.

#### `internal/crashpoint`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/crashpoint`

The production-callable half. It holds the `Breakpoint` hook and three
environment-variable constants (`EnvCrashAt` = `GOGRAPH_CRASH_AT`,
`EnvCrashDir` = `GOGRAPH_CRASH_DIR`, `EnvCrashAfter` = `GOGRAPH_CRASH_AFTER`)
and depends on nothing beyond `os` and `syscall`. Production write paths embed
breakpoints by importing it directly:

```go
// In production library code (store/checkpoint, store/wal, …):
crashpoint.Breakpoint("checkpoint.p2-snapshot-published-pre-truncate")
```

**`Breakpoint` is gated by the `gograph_crashinject` build tag, and the tag is
what makes it live.** Two implementations are selected at compile time:

- `crashpoint_disabled.go` (`//go:build !gograph_crashinject`) — the default,
  and what every released binary links. `Breakpoint` is an empty function: it
  does not read `GOGRAPH_CRASH_AT`, links no syscall, and the compiler elides
  it. An inherited `GOGRAPH_CRASH_AT` therefore cannot kill a production
  process, and the durability paths pay nothing per call.
- `crashpoint_enabled.go` (`//go:build gograph_crashinject`) — the active hook.
  `Breakpoint(name)` returns immediately when `GOGRAPH_CRASH_AT` is unset or
  does not equal `name`; on a match it sends itself SIGKILL, simulating an
  abrupt crash at that exact execution point. `GOGRAPH_CRASH_AFTER=n` lets the
  first `n` matching hits through and kills on the `(n+1)`th, so a breakpoint on
  a hot path can be moved past the degenerate first-commit window into the
  steady state where several writers are in flight.

The exported API is identical in both modes, so call sites never change. Run
the battery with `make test-crashinject`, or `go test -tags=gograph_crashinject`.
Every crash test is itself headed `//go:build gograph_crashinject`, so without
the tag those tests are not compiled at all: `go test -list '.*'
./internal/crashinject/` lists 14 harness unit tests and not one crash
scenario. A green `go test ./...` therefore says nothing about crash safety.

The breakpoints compiled into production packages are
`checkpoint.p2-snapshot-published-pre-truncate` (`store/checkpoint`),
`checkpoint.truncprefix.tmp-written-pre-rename`,
`checkpoint.truncprefix.post-rename-pre-dirfsync`,
`checkpoint.truncprefix.post-rename-pre-bookkeeping`,
`wal.appendrun.frame-emitted`, `wal.sync.pre-datasync` (`store/wal`),
`recovery.snapshot-promote-post-rename-pre-fsync` (`store/recovery`) and
`mvcc.commit.post-fsync-pre-publish` (`cypher`).

#### `internal/crashinject`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/crashinject`

The subprocess crash harness. It re-exports `Breakpoint`, `EnvCrashAt`,
and `EnvCrashDir` from `crashpoint` so existing call sites keep working
(`EnvCrashAfter` is not re-exported), and adds the `Run` driver:

```go
// In tests, in a file built with -tags gograph_crashinject:
out, err := crashinject.Run(t, "wal.mid-frame", crashinject.Opts{})
// out.Killed == true; out.Dir contains the artefacts
```

`Run` lazily builds `cmd/crashinject-helper` and spawns it with
`GOGRAPH_CRASH_AT=<scenario>` and `GOGRAPH_CRASH_DIR=<dir>`. The helper
exercises the scenario's write path until a `Breakpoint` call at the
named execution point triggers SIGKILL, leaving the artefacts in a
deterministically torn state for the parent to inspect. `Opts` carries the
artefact `Dir` (default a fresh `t.TempDir()`), extra child `Env`, and a
`Timeout` (default 30 s). `Out.Killed` reports a genuine breakpoint self-kill
and is false when the deadline elapsed instead — that case sets `Out.TimedOut`.

**Registered scenarios in `cmd/crashinject-helper`:**

| Scenario | Breakpoint site | Description |
|---|---|---|
| `wal.mid-frame` | helper | Writes one complete WAL frame, appends a partial second-frame header, then SIGKILL; `wal.Reader` must report `ErrTornFrame` |
| `checkpoint.p2-snapshot-published-pre-truncate` | `store/checkpoint` | Commits an int64-keyed workload, then drives a codec-aware checkpoint that crashes after the self-sufficient snapshot is published and durable but before the WAL prefix is truncated; recovery rebuilds state from the snapshot plus the still-intact WAL |
| `checkpoint.truncprefix.tmp-written-pre-rename` | `store/wal` | Crash inside `wal.Writer.TruncatePrefix`, after the replacement WAL is written to its temp name but before the rename |
| `checkpoint.truncprefix.post-rename-pre-dirfsync` | `store/wal` | Same truncate, crashing after the rename but before the parent-directory fsync |
| `checkpoint.truncprefix.post-rename-pre-bookkeeping` | `store/wal` | Same truncate, crashing after the rename is durable but before the writer updates its own offset bookkeeping |
| `recovery.snapshot-promote-post-rename-pre-fsync` | `store/recovery` | Stages the interrupted-publish state (the live snapshot archived to `snapshot.bak`) and drives `recovery.Open`, which crashes after promoting `.bak` back onto the live snapshot name via rename but before the parent-directory fsync; a second recovery must still observe the promoted snapshot — guards A1-F4 (#1454) |
| `constraint.drop.post-wal-sync` | helper | Commits a durable `CREATE CONSTRAINT` (UNIQUE) and a node, then a durable `DROP CONSTRAINT` frame, and crashes after the fsync; recovery must show the constraint and its backing index gone together, with no torn intermediate (#1556) |
| `edgehandle.setprop.post-wal-sync` | helper | Two parallel edges over one ordered `(src, dst)` pair; a durable `OpSetEdgePropertyByHandle` touches the first handle only, then SIGKILL. Recovery must show the property on that handle alone, the sibling untouched, and still exactly two parallel edges (#1686) |
| `edgehandle.delprop.post-wal-sync` | helper | The same two edges, with `tag` seeded on the first handle at CREATE; a durable `OpDelEdgePropertyByHandle` then removes it from that handle, and the crash follows the fsync. Recovery must show `tag` gone from handle 1 and the sibling's own state intact |
| `edgehandle.delete.post-wal-sync` | helper | A durable `OpRemoveEdgeByHandle` retires the second of two parallel edges, then SIGKILL; recovery must land on exactly the first handle, with its own property intact (rmp #2018) |
| `wal.appendrun.frame-emitted` | `store/wal` | Several writer goroutines commit multi-op transactions concurrently and the crash lands mid-append run, with transactions in flight |
| `wal.sync.pre-datasync` | `store/wal` | The same concurrent workload, crashing inside `wal.Writer.SyncGroup` before the data fsync |
| `mvcc.commit.post-fsync-pre-publish` | `cypher` | Commits through the Cypher engine and crashes in the window between the WAL fsync and the MVCC visibility publish (rmp #2309, MVCC C3c) |

The helper also honours a **workload override**, `GOGRAPH_CRASH_WORKLOAD`.
Every scenario above is selected by its breakpoint name, which works only while
each breakpoint has one workload worth driving it through. Setting
`GOGRAPH_CRASH_WORKLOAD=checkpoint-concurrent` runs
`checkpoint.p2-snapshot-published-pre-truncate` with transactions committing
*throughout* the checkpoint — a different question at the same crash point,
and one no second breakpoint name would express honestly (rmp #2310).

### `internal/subproc`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/subproc`

TestMain-pattern subprocess helper for deterministic cross-process tests.

```go
// In TestMain:
subproc.Register("open-snapshot", func(args []string) int { … })
subproc.Dispatch() // no-op in parent; calls handler + os.Exit in child

// In tests:
out, _, err := subproc.Run(t, "open-snapshot", snapshotPath)
```

`Run` re-execs `os.Args[0]` (the test binary) with
`GOGRAPH_SUBPROC_MODE=<mode>` set. The child's working directory is
`t.TempDir()`, which is cleaned up by the testing framework.

---

## Golden-file helper (`internal/goldens`)

Package: `github.com/FlavioCFOliveira/GoGraph/internal/goldens`

```go
goldens.Assert(t, "testdata/output.golden", got)
```

On mismatch, reports a unified diff. With `-update` or
`GOGRAPH_UPDATE_GOLDENS=1`, overwrites the file atomically
(temp file + rename) and continues. Call `goldens.UpdateRequested()`
in `TestMain` to gate conditional generation logic.

### Isolation harness (`internal/isolationtest`)

Package: `github.com/FlavioCFOliveira/GoGraph/internal/isolationtest`

The scripted, **exhaustive**, deterministic counterpart of the randomised DST
battery, modelled on PostgreSQL's `src/test/isolation` (read at commit
`0ec3f048`). A spec declares named sessions holding named steps; the harness runs
**every** interleaving that preserves each session's own step order, and diffs the
rendered transcript against `testdata/<spec>.golden`.

```go
isolationtest.Check(t, spec, &isolationtest.Runner{NewEngine: memEngine})
```

Two assertion mechanisms, answering different questions: the **golden
transcript** catches a change in behaviour, while **`Runner.Observe`** catches
incorrect behaviour by asserting an invariant over each step's structured rows.
A failing permutation is named, and `Runner.Only` replays it alone.

Shipped at the **short** layer: `lost-update`, `write-skew` and
`bank-transfer`, each two sessions of three steps and therefore C(6,3) = 20
permutations, plus one *named* `read-only-anomaly` permutation — PostgreSQL's
own interleaving, run on its own. The exhaustive `read-only-anomaly`
(4 200 permutations) runs at **soak** behind `testlayers.RequireSoak`, and
asserts an `Observe` invariant rather than a golden, because a
4 200-permutation transcript is a diff nobody would read. The harness is proven to catch a real fault rather than merely to pass
— see the negative control in `fault_test.go`.

Full specification, including the enumeration algorithm, the determinism
argument, what was deliberately not copied from PostgreSQL, and the recipe for a
new spec: [docs/isolation-harness.md](isolation-harness.md).

### Anomaly classifier (`internal/anomaly`)

Package: `github.com/FlavioCFOliveira/GoGraph/internal/anomaly`

Turns an observed transaction history into a NAMED phenomenon — G0, G1a, G1b,
G1c, G-single, G-nonadjacent, G2-item — instead of a bare domain symptom, so a
sighting points at a mechanism rather than starting a search.

```go
h := recorder.History()
rep, err := anomaly.Check(&h, anomaly.SnapshotIsolation)
```

The level boundary is the substance: snapshot isolation forbids G-nonadjacent
(⊇ G-single, the shape of a lost update) and **permits** G2-item cycles whose
anti-dependencies are adjacent, which is write skew. Both directions are
asserted. The `Recorder` is sharded and pre-sized because a shared lock on the
recording path measurably suppressed the anomaly it was there to observe.

Definitions cited to Adya (ICDE 2000), Berenson et al. (SIGMOD 1995), Cerone et
al. (CONCUR 2015) and verified against Jepsen's Elle. Full specification, the
validation on a healthy and a defective engine, the perturbation measurement,
and the rmp #2336 classification attempt:
[docs/isolation-anomalies.md](isolation-anomalies.md).

---

## Test layers quick-reference

| Layer | Build tag | Env var | Make target | Budget |
|---|---|---|---|---|
| **short** | _(default)_ | — | `make test-short` | see [docs/test-layers.md](test-layers.md) |
| **soak** | `-tags=soak` | `SOAK_FULL=1` | `make test-soak` (`-tags=soak`) | minutes |
| **nightly** | `-tags=nightly` | `GOGRAPH_NIGHTLY=1` | `make test-nightly` (`-tags=soak,nightly,soakfull`) | hours |

The layers are supersets, but *which* mechanism delivers that differs, and the
difference bites. The runtime helpers nest correctly:
`testlayers.IsSoak` is true under any of `soak`, `nightly`, `soakfull` or
`stress`, so `RequireSoak` passes under `-tags=nightly`. **Compile-time gating
does not nest**: a file headed `//go:build soak` is not compiled by
`-tags=nightly` alone, and 13 such soak-only files exist in the tree (plus two
headed `//go:build soakfull`). That is why `make test-nightly` passes
`-tags=soak,nightly,soakfull` rather than `-tags=nightly` — use the make
target, not the bare tag, to get the whole superset.

The crash-injection battery is a fourth, orthogonal gate rather than a layer:
it needs `-tags=gograph_crashinject` (`make test-crashinject`) and is inert
without it. See
[`internal/crashpoint` and `internal/crashinject`](#internalcrashpoint-and-internalcrashinject).

The short-layer budget is enforced by `scripts/pkg_time_budget.sh`,
which `make test-short` pipes its output through, so every `make ci` reads it.
**The figures are deliberately not repeated here** — they live in one place, with
the measurements and the exceptions that justify them, because three documents
restating one number is how they came to disagree (rmp #2585). See
[docs/test-layers.md](test-layers.md) for the full specification and
Makefile targets.

### Runnable godoc examples

The public packages ship 101 runnable `Example` functions: `graph/` 36,
`cypher/` 28, `search/` 18, `store/` 10, `bolt/` 5, `ds/` 3 and `metrics/` 1.
They carry `// Output:` markers, so the `testing` framework compiles and
executes them — and verifies their printed output — as part of the
default **short** layer on plain `go test ./...`. They double as
compiler-checked documentation and as a category of correctness test.

---

## Add-new-shape recipe

Follow these steps whenever a new graph family is added to the battery.

### 1. Create the generator file

Add `internal/shapegen/<family>.go`. Implement the `Shape` interface or
expose a package-level constructor function. Document the family's
asymptotic properties and the range of each knob.

Every existing catalogue family returns `Shape[int, int64]`, with `int64(0)`
as the "unweighted" sentinel; the sketch below takes the simpler
package-level-constructor option, which is why it can be handed straight to
`invariants` in step 4 but cannot be handed to `Register` in step 6.

```go
// internal/shapegen/myfamily.go
package shapegen

// MyFamilyShape generates a graph from the MyFamily model.
//
// Knobs:
//   n: number of vertices (default 10, min 1, max 1_000_000)
//   k: parameter k (default 3, min 1, max n-1)
func MyFamilyShape(n, k int) *lpg.Graph[int, struct{}] { … }
```

### 2. Write the short-layer unit tests

Add `internal/shapegen/<family>_test.go`. At minimum:
- Verify `Order()` and `Size()` against the analytical formula.
- Verify boundary conditions (`n=0`, `n=1`, `k=0`).
- Add a `rapid`-based property test that sweeps the knobs.

```go
func TestMyFamily_OrderSize(t *testing.T) { … }
func TestMyFamily_Rapid(t *testing.T) {
    rapid.Check(t, func(rt *rapid.T) {
        n := rapid.IntRange(1, 100).Draw(rt, "n")
        g := MyFamilyShape(n, 3)
        if g.AdjList().Order() != uint64(n) { rt.Errorf(…) }
    })
}
```

### 3. Lock the determinism with a golden

Write the golden assertion with the shared helper,
`goldens.Assert(t, "testdata/<family>.golden", got)`, then run once to create
the file:

```bash
GOGRAPH_UPDATE_GOLDENS=1 go test ./internal/shapegen/... -run TestMyFamily_Golden
```

Then commit `internal/shapegen/testdata/<family>.golden`.

Note that the families already in the catalogue do **not** use
`internal/goldens`. Each has its own copy of an `assertGolden` helper, driven
by a package-local `-shapegen-update` flag declared in `trivial_test.go`, and
writes to `testdata/shapegen/<family>/<name>.txt`. `GOGRAPH_UPDATE_GOLDENS=1`
has no effect on those. New families should use the shared helper as above;
the migration of the existing ones is tracked as #526.

### 4. Add invariant assertions (optional but recommended)

If the family has a known topological property, assert it:

```go
import "github.com/FlavioCFOliveira/GoGraph/internal/invariants"

invariants.AssertConnected(t, g)     // connected families
invariants.AssertDAG(t, g)           // DAG families
invariants.AssertBipartite(t, g)     // bipartite families
```

### 5. Add a soak-layer test for large instances

```go
//go:build soak

package shapegen

import (
    "testing"

    "github.com/FlavioCFOliveira/GoGraph/internal/invariants"
)

func TestMyFamily_Soak(t *testing.T) {
    g := MyFamilyShape(1_000_000, 10)
    invariants.AssertConnected(t, g)
    // … additional checks …
}
```

A `//go:build soak` header is compiled by `-tags=soak` and by
`make test-nightly`, but **not** by a bare `-tags=nightly` — see
[Test layers quick-reference](#test-layers-quick-reference).

### 6. Register the shape in any conformance / regression harness

There is currently **nothing to do here**, and that is a measured fact rather
than an omission: the module has no shape-level conformance matrix, there is no
`internal/shapegen/registry.go` file (the registry lives in `shapegen.go`), and
`Register` has no caller outside `internal/shapegen/shapegen_test.go`. A family
that implements `Shape` may still register itself, but nothing enumerates the
registry, so registration exercises nothing today. Delete this step from your
checklist unless and until a matrix exists.

### 7. Update this document

Add the new family to the [Shape catalogue](#shape-catalogue) table
and update the "Last reviewed" footer at the bottom of this file.

---

*Last reviewed: 2026-09-08 against commit `8c83329b`. This document's freshness is checked by `scripts/check_doc_freshness.sh`, run locally.*
