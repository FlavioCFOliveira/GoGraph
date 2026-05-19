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
