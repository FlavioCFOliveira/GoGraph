# Semantic Versioning Policy

GoGraph follows [Semantic Versioning 2.0.0](https://semver.org/).
Version numbers take the form `MAJOR.MINOR.PATCH`, with the
project's interpretation captured below.

## What "public API" means in GoGraph

The public API is every exported identifier in every package
*outside* an `internal/` directory. This includes:

- The `graph`, `graph/adjlist`, `graph/csr`, `graph/lpg`,
  `graph/lpg/schema`, `graph/index/*`, `graph/query`,
  `graph/generation` packages.
- The `search`, `search/centrality`, `search/community`,
  `search/extern`, `search/flow` packages.
- The `store/wal`, `store/snapshot`, `store/txn`,
  `store/checkpoint`, `store/recovery`, `store/csrfile`,
  `store/bulk` packages.
- The `ds` package.
- The `bench/ldbc`, `bench/dimacs9`, `bench/rmat` packages
  (their *types* are part of the API; the benchmark numbers
  reported by their CLIs are not).

Internal helpers (lowercase identifiers, package-private types)
are excluded from the API surface and may change at any time.

## MAJOR bumps

Increment MAJOR when any of the following is true:

- An exported identifier is removed or renamed.
- A method signature changes incompatibly (parameter or return
  type, ordering, kind).
- A behavioural guarantee is weakened in a way that breaks
  existing callers (e.g. a method that always returned a
  freshly-allocated slice starts aliasing internal state).
- An on-disk format (WAL frame, snapshot manifest, csrfile
  layout) is changed in a way that older readers cannot parse.

## MINOR bumps

Increment MINOR when:

- A new exported identifier is added (package, type, method,
  function, constant, error).
- A new behaviour is added behind an opt-in option / flag.
- An on-disk format version is bumped *forward*-compatibly
  (new versions readable, older versions still parseable).

## PATCH bumps

Increment PATCH when:

- A bug is fixed without changing the API.
- A documentation, doc-test, or comment-only change ships.
- An optimisation lands without changing observable behaviour
  (benchstat numbers improve; semantics unchanged).

## On-disk formats

Three on-disk formats are versioned:

| Format        | Current version | Spec                            |
|---------------|-----------------|---------------------------------|
| WAL frame     | 1               | `store/wal/FORMAT.md`           |
| Snapshot manifest | 3           | embedded JSON, validated by `store/snapshot.LoadManifest`; readers accept versions ≤ 3 (v1 CSR-only, v2 CSR + labels + properties, v3 adds typed mapper/indexes). |
| Snapshot `labels.bin` | 2       | Bumped 1 → 2 in `v0.11.0`: the edge record gains a `Slot` field, so a relationship *type* is durable per slot rather than per node pair. Version 1 stored `EdgeLabelEntry{Src, Dst, StringIdx}` with no slot ordinal, so parallel edges folded into one key on disk. |
| csrfile       | 1               | `docs/csrfile-v1.md`            |

Format-version bumps follow the bump-on-incompatible-change rule
(MAJOR) or the forward-compatible bump rule (MINOR) above.

## Pre-release

Pre-release identifiers (e.g. `1.0.0-rc.1`) are tagged ahead of
each MINOR / MAJOR release; production users should pin a stable
tag.

## Deprecation policy

Deprecated APIs are kept for at least one MINOR release before
removal. Each deprecated identifier carries a godoc comment
mentioning the replacement and the version where removal is
expected.

## Release gates

### 1.0.0 stable

The first stable `1.0.0` release will be cut from the current `0.x`
baseline when **all** of the following conditions are met:

1. **Execution-level TCK ≥ 95 % — MET.** The openCypher TCK execution runner
   (`cypher/tck`) must pass at least 95 % of the scenarios it runs (i.e.
   ≥ 3 702 / 3 897). The local TCK conformance gate — the `cypher/tck`
   `TestTCKExecution` baseline (`const tckExecutionBaseline`), run inside
   `make ci` (which `make release-preflight` invokes) — must be green at this
   threshold.
   Current status as of `v0.11.0` (2026-08-13): **100 %** —
   `const tckExecutionBaseline = 3897`, which is the full scenario count, so the
   gate fails on any regression at all rather than at a 95 % floor. The runner
   additionally fails on any `failed`, `undefined` or `pending` step. See
   [docs/tck/DIVERGENCES.md](tck/DIVERGENCES.md) for the authoritative
   table.

2. **All local gates green — MET at `v0.11.0`.** Every gate must pass on the
   release commit, run locally via `make ci` and `make release-preflight`: build,
   test, race detector, lint (`golangci-lint`), vet, TCK, the coverage gate, the
   crash-injection battery, and a clean `govulncheck ./...`. At `v0.11.0`:
   123 packages ok with zero `FAIL` lines under `go test -race ./...`,
   `golangci-lint` 0 issues, coverage 87.1 % aggregate with every package above
   its 75 % floor.

3. **All T-series tasks closed — MET.** Every task prefixed `T-` in
   `docs/tck/DIVERGENCES.md` must be marked resolved. T-series tasks
   track known execution-engine gaps that block execution TCK scenarios.
   No unresolved `T-` entry remains.

4. **Soak test green — NOT MET.** The full soak test (`SOAK_FULL=1`,
   1 024 connections, 4 hours) must pass with zero goroutine leaks and
   zero race conditions. The soak report in `soak-artefacts/` must reflect
   a run against the release commit. The canonical execution path is to
   run the soak locally via `SOAK_FULL=1 make soak`; its artefacts
   (soak.log, heap profiles, bolt-soak-ci-report.md) are what the
   release gate consumes. As noted in `CLAUDE.md`, the soak is a periodic
   reliability exercise run before a major release rather than an
   automated per-push gate.
   **Current status: the artefacts in `soak-artefacts/` are from 2026-05-30 at
   commit `b5453b9`** — before `v0.9.0`, and more than 500 commits behind
   `v0.11.0`. They do not reflect any recent commit, so this gate is open. The
   three production certifications of August 2026 each record the whole-tree soak
   layer as unrun (see
   [`docs/certification-2026-08-13.md`](certification-2026-08-13.md) §5).

**Gate status summary at `v0.11.0`: 3 of 4 met.** Gate 4 (soak against the
release commit) is the outstanding blocker for `1.0.0`, together with the
unmeasured items the current certification envelope lists.

### Pre-release candidates

Pre-release candidate tags (`v1.0.0-rc1`, `v1.0.0-rc2`, …) are tagged as
significant improvements become available, without waiting for all stable
gates to be met. Each candidate documents its own conformance numbers and
known limitations in the corresponding `release-notes/v<version>.md` file.

Production deployments should pin a stable tag. Candidates are suitable for
integration testing and early adoption, with the understanding that execution
conformance is still being improved.
