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
| Snapshot manifest | 1           | embedded JSON, validated by   `store/snapshot.LoadManifest` |
| csrfile       | 1               | `docs/csrfile-v1.md`            |

Format-version bumps follow the bump-on-incompatible-change rule
(MAJOR) or the forward-compatible bump rule (MINOR) above.

## Pre-release

Pre-release identifiers (e.g. `1.1.0-rc.1`) are tagged ahead of
each MINOR / MAJOR release; production users should pin a stable
tag.

## Deprecation policy

Deprecated APIs are kept for at least one MINOR release before
removal. Each deprecated identifier carries a godoc comment
mentioning the replacement and the version where removal is
expected.

## Release gates

### v2.0.0 stable

v2.0.0 stable will be cut when **all** of the following conditions are met:

1. **Execution-level TCK ≥ 80 %.** The openCypher TCK execution runner
   (`cypher/tck`) must pass at least 80 % of the scenarios it runs. The
   CI gate in `.github/workflows/tck.yml` must be green at this threshold.
   Current status as of v2.0.0-rc2: **25.8 %**. Current status on HEAD
   (commit `7405463`, 2026-05-22): **39.4 %** (1 536 / 3 897 scenarios).
   See [docs/tck/DIVERGENCES.md](tck/DIVERGENCES.md) for the
   authoritative table updated by the same workflow.

2. **All CI checks green.** Every job in the CI pipeline must pass on the
   release commit: build, test, race detector, lint (`golangci-lint`),
   vet, TCK, soak (short variant), and govulncheck.

3. **All T-series tasks closed.** Every task prefixed `T-` in
   `docs/tck/DIVERGENCES.md` must be marked resolved. T-series tasks
   track known execution-engine gaps that block execution TCK scenarios.

4. **Soak test green.** The full soak test (`SOAK_FULL=1`,
   1 024 connections, 4 hours) must pass with zero goroutine leaks and
   zero race conditions. The soak report in `soak-artefacts/` must reflect
   a run against the release commit. The canonical execution path is the
   `Soak` GitHub Actions workflow at
   [`.github/workflows/soak.yml`](../.github/workflows/soak.yml), which
   runs weekly (Sunday 02:00 UTC) and on `workflow_dispatch`. Operators
   may also run the soak locally via `SOAK_FULL=1 make soak`, but the
   CI workflow run is what the release gate consumes — its artefacts
   (soak.log, heap profiles, bolt-soak-ci-report.md) are retained on
   the workflow run for 30/90 days respectively.

### Pre-release candidates

Pre-release candidate tags (`v2.0.0-rc1`, `v2.0.0-rc2`, …) are tagged as
significant improvements become available, without waiting for all stable
gates to be met. Each candidate documents its own conformance numbers and
known limitations in the corresponding `release-notes/v<version>.md` file.

Production deployments should pin a stable tag. Candidates are suitable for
integration testing and early adoption, with the understanding that execution
conformance is still being improved.
