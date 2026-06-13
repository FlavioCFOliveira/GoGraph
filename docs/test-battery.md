# Production-Readiness Test Battery

This document describes the test infrastructure and shape-generator
catalogue that together form GoGraph's production-readiness test battery.
It is the single authoritative reference for anyone who wants to extend
the suite, add a new shape, or integrate a new algorithm into the
correctness pipeline.

For the three-layer test discipline and CI integration, see
[docs/test-layers.md](test-layers.md).

---

## Table of contents

1. [Architecture overview](#architecture-overview)
2. [The Shape interface and `internal/shapegen`](#the-shape-interface-and-internalshapengen)
3. [Shape catalogue](#shape-catalogue)
4. [Real-world dataset loaders](#real-world-dataset-loaders)
5. [Invariant checkers (`internal/invariants`)](#invariant-checkers-internalinvariants)
6. [Fault-injection packages](#fault-injection-packages)
   - [`internal/testfs`](#internaltestfs)
   - [`internal/crashpoint` and `internal/crashinject`](#internalcrashpoint-and-internalcrashinject)
   - [`internal/subproc`](#internalsubproc)
7. [Golden-file helper (`internal/goldens`)](#golden-file-helper-internalgoldens)
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

- **`Name()`** — the canonical, human-readable name of the shape
  (e.g. `"PathGraph-n5"`, `"ErdosRenyi-n1000-p0.01"`).
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

---

## Shape catalogue

The following families are implemented in `internal/shapegen`:

| File | Family | Shapes |
|---|---|---|
| `trivial.go` | Degenerate | Empty, single node, single edge, self-loop, parallel digon, isolated-only, universal self-loops |
| `classic.go` | Classic | Path Pₙ, Cycle Cₙ, Star Sₙ, Complete Kₙ, Complete bipartite Kₘₙ, Petersen, hypercube Qₙ, grid, torus |
| `trees.go` | Trees | Star tree, path tree, balanced binary tree, random spanning tree |
| `structured.go` | Structured | Ladder, wheel, friendship, Möbius–Kantor, Cayley (dihedral) |
| `erdos_renyi.go` | Random | Erdős–Rényi G(n,p) and G(n,m) |
| `barabasi_albert.go` | Scale-free | Barabási–Albert preferential attachment |
| `watts_strogatz.go` | Small-world | Watts–Strogatz rewiring |
| `configmodel.go` | Degree sequences | Configuration model, Chung–Lu |
| `sbm.go` | Community | Stochastic block model, planted partition |
| `lfr.go` | Community | LFR community benchmark |
| `rmat.go` | Synthetic large | R-MAT graph generator |
| `rgg.go` | Geometric | Random geometric graph |
| `dags.go` | DAGs | Random DAG, layered DAG |
| `adversarial.go` | Adversarial | Adversarial edge weights and labels |
| `mapperadv.go` | Adversarial | Mapper shard-0 key preimage generator |
| `specials.go` | Special | Specials: path, cycle, bipartite for rapid |

---

## Real-world dataset loaders

### SNAP datasets (`snap.go`)

Package-level loader functions for Stanford Network Analysis Project
(SNAP) datasets. Datasets are fetched from
`https://snap.stanford.edu/data/` on first use and cached under
`$GOGRAPH_SNAP_DIR` (default: `$HOME/.cache/gograph-snap`).

| Function | Dataset | Nodes | Edges | Layer |
|---|---|---|---|---|
| `CitHepPh(cacheDir)` | cit-HepPh | 34 546 | 421 578 | soak |
| `WebGoogle(cacheDir)` | web-Google | 875 713 | 5 105 039 | soak |
| `SocLiveJournal1(cacheDir)` | soc-LiveJournal1 | 4 847 571 | 68 993 773 | nightly |

### LDBC Graphalytics datasets (`graphalytics.go`)

Package-level loader `LoadGraphalytics(name, cacheDir)` and reference
output accessor `LoadGraphalyticsReference(name, alg, cacheDir)`. Data
is fetched from the SURF cold-storage mirror; HTTP 409 is surfaced as
`ErrGraphalyticsStaging`.

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

`BuildBFSDepths[W](ctx, csr, src)` is a convenience function that runs
BFS from `src` and returns `map[graph.NodeID]int` for use with
`AssertDistanceBound`.

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
implements the `File` interface accepted by `store/wal.OpenWith` and
future store adapters.

```go
type Faults struct {
    FailWritesAfterBytes int64          // truncate writes at N bytes total
    ReturnENOSPC         bool           // all writes return syscall.ENOSPC
    FsyncDelay           time.Duration  // sleep before each Sync
    CorruptOnRead        func(offset, n int64) bool // flip first byte on read
}

ff, _ := testfs.New(path, testfs.Faults{FailWritesAfterBytes: 128})
w, _ := wal.OpenWith(ff)  // inject fault into WAL writer
```

`IsENOSPC(err)` unwraps `*os.PathError` to check for `syscall.ENOSPC`.

### `internal/crashpoint` and `internal/crashinject`

The crash-injection machinery is split across two packages so that
production code never links the `testing` package or the subprocess
runner.

#### `internal/crashpoint`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/crashpoint`

The production-callable half. It holds the `Breakpoint` hook and the two
environment-variable constants (`EnvCrashAt` = `GOGRAPH_CRASH_AT`,
`EnvCrashDir` = `GOGRAPH_CRASH_DIR`) and depends on nothing beyond `os`
and `syscall`. Production write paths embed breakpoints by importing it
directly:

```go
// In production library code (store/checkpoint, store/wal, …):
crashpoint.Breakpoint("checkpoint.mid-truncate") // no-op in production
```

`Breakpoint(name)` is a no-op when `GOGRAPH_CRASH_AT` is unset or does
not match `name` (one string comparison, no locks, safe for concurrent
use). When it matches, the process sends itself SIGKILL, simulating an
abrupt crash at that exact execution point.

#### `internal/crashinject`

Package: `github.com/FlavioCFOliveira/GoGraph/internal/crashinject`

The subprocess crash harness. It re-exports `Breakpoint`, `EnvCrashAt`,
and `EnvCrashDir` from `crashpoint` so existing call sites keep working,
and adds the `Run` driver:

```go
// In tests:
out, err := crashinject.Run(t, "wal.mid-frame", crashinject.Opts{})
// out.Killed == true; out.Dir contains the artefacts
```

`Run` lazily builds `cmd/crashinject-helper` and spawns it with
`GOGRAPH_CRASH_AT=<scenario>` and `GOGRAPH_CRASH_DIR=<dir>`. The helper
exercises the scenario's write path until a `Breakpoint` call at the
named execution point triggers SIGKILL, leaving the artefacts in a
deterministically torn state for the parent to inspect.

**Registered scenarios in `cmd/crashinject-helper`:**

| Scenario | Breakpoint site | Description |
|---|---|---|
| `wal.mid-frame` | helper | Writes one complete WAL frame, appends a partial second-frame header, then SIGKILL; `wal.Reader` must report `ErrTornFrame` |
| `checkpoint.post-snapshot-pre-truncate` | `store/checkpoint` | Commits an int64-keyed workload, then drives a codec-aware checkpoint that crashes after the self-sufficient snapshot is durable but before the WAL is truncated; recovery rebuilds state from the snapshot plus the still-intact WAL |
| `checkpoint.mid-truncate` | `store/wal` | Same workload and checkpoint, but the crash lands mid-truncation (the WAL file is already shrunk to zero); recovery rebuilds state from the self-sufficient snapshot alone |
| `recovery.snapshot-promote-post-rename-pre-fsync` | `store/recovery` | Commits and checkpoints a self-sufficient int64-keyed snapshot, then stages the interrupted-publish state (the live snapshot archived to `snapshot.bak`) and drives `recovery.Open`, which crashes after promoting `.bak` back onto the live snapshot name via rename but before the parent-directory fsync; a second recovery must still observe the promoted snapshot (the rename and its dirent are durably re-promoted) — guards A1-F4 (#1454) |

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

---

## Test layers quick-reference

| Layer | Build tag | Env var | Make target | Budget |
|---|---|---|---|---|
| **short** | _(default)_ | — | `make test-short` | < 60 s/pkg (enforced) |
| **soak** | `-tags=soak` | `SOAK_FULL=1` | `make test-soak` | minutes |
| **nightly** | `-tags=nightly` | `GOGRAPH_NIGHTLY=1` | `make test-nightly` | hours |

Each layer is a strict superset: `nightly` always includes `soak` and
`short`. The short-layer `< 60 s/pkg` budget is enforced by the
`timing-budget` CI job via `make test-short-timings`
(`scripts/pkg_time_budget.sh`): a warning above 60 s/pkg, a hard failure
above 240 s/pkg. See [docs/test-layers.md](test-layers.md) for the full
specification, CI workflow table, and Makefile targets.

### Runnable godoc examples

The public packages ship runnable `Example` functions (around 97 across
the `graph/`, `cypher/`, `search/`, `store/`, `bolt/`, and `ds/` trees).
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

Run once with `-update` to create the golden file:

```bash
GOGRAPH_UPDATE_GOLDENS=1 go test ./internal/shapegen/... -run TestMyFamily_Golden
```

Then commit `internal/shapegen/testdata/<family>.golden`.

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

func TestMyFamily_Soak(t *testing.T) {
    g := MyFamilyShape(1_000_000, 10)
    invariants.AssertConnected(t, g)
    // … additional checks …
}
```

### 6. Register the shape in any conformance / regression harness

If the project has a shape-level conformance matrix (e.g.
`internal/shapegen/registry.go`), add the new shape there so it is
automatically exercised by cross-algorithm correctness checks.

### 7. Update this document

Add the new family to the [Shape catalogue](#shape-catalogue) table
and update the "Last reviewed" footer at the bottom of this file.

---

*Last reviewed: 2026-06-13 against commit `f72fa2b`. This document is tracked by the doc-freshness CI gate in `.github/workflows/ci.yml`.*
