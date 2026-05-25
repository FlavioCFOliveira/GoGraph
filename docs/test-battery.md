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
   - [`internal/crashinject`](#internalcrashinject)
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
│  │ internal/testfs │  │internal/crashinject│  │internal/subproc │  │
│  │ (FS faults)     │  │ (SIGKILL harness)  │  │ (child procs)   │  │
│  └─────────────────┘  └───────────────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

---

## The Shape interface and `internal/shapegen`

Package: `gograph/internal/shapegen`

Every graph shape is represented by the `Shape` interface:

```go
type Shape interface {
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

Package: `gograph/internal/invariants`

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

---

## Fault-injection packages

### `internal/testfs`

Package: `gograph/internal/testfs`

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

### `internal/crashinject`

Package: `gograph/internal/crashinject`

Subprocess-based crash harness using SIGKILL.

```go
// In production library code:
crashinject.Breakpoint("wal.mid-frame") // no-op in production

// In tests:
out, err := crashinject.Run(t, "wal.mid-frame", crashinject.Opts{})
// out.Killed == true; out.Dir contains the artefacts
```

`Run` lazily builds `cmd/crashinject-helper` and spawns it with
`GOGRAPH_CRASH_AT=<scenario>`. The helper calls `Breakpoint` at the
named execution point, triggering SIGKILL.

**Registered scenarios in `cmd/crashinject-helper`:**

| Scenario | Description |
|---|---|
| `wal.mid-frame` | Writes one complete WAL frame, appends a torn 10-byte header, then SIGKILL |

### `internal/subproc`

Package: `gograph/internal/subproc`

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

Package: `gograph/internal/goldens`

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
| **short** | _(default)_ | — | `make test-short` | < 60 s/pkg |
| **soak** | `-tags=soak` | `SOAK_FULL=1` | `make test-soak` | minutes |
| **nightly** | `-tags=nightly` | `GOGRAPH_NIGHTLY=1` | `make test-nightly` | hours |

Each layer is a strict superset: `nightly` always includes `soak` and
`short`. See [docs/test-layers.md](test-layers.md) for the full
specification, CI workflow table, and Makefile targets.

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
import "gograph/internal/invariants"

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

*Last reviewed: 2026-05-25 against commit `b391da02ac44a9a4b961d0b78e6a0835d5b5466f`. This document is tracked by the doc-freshness CI gate in `.github/workflows/ci.yml`.*
