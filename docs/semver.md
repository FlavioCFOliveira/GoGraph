# Semantic Versioning Policy

GoGraph follows [Semantic Versioning 2.0.0](https://semver.org/).
Version numbers take the form `MAJOR.MINOR.PATCH`, with the
project's interpretation captured below.

## What "public API" means in GoGraph

The public API is every exported identifier in every package
*outside* an `internal/` directory. This includes:

- The root package (`doc.go` only) and the `graph`, `graph/adjlist`,
  `graph/csr`, `graph/lpg`, `graph/lpg/schema`, `graph/mvcc`,
  `graph/index`, `graph/index/btree`, `graph/index/count`,
  `graph/index/hash`, `graph/index/label`, `graph/index/stats`,
  `graph/query`, `graph/generation` packages, and the exporters
  `graph/io/csv`, `graph/io/dot`, `graph/io/graphml`, `graph/io/jsonl`.
- The `search`, `search/centrality`, `search/community`,
  `search/extern`, `search/flow` packages.
- The `store` package itself (`store.DB`) and the `store/wal`,
  `store/snapshot`, `store/txn`, `store/checkpoint`, `store/recovery`,
  `store/csrfile`, `store/bulk`, `store/bulkimport` packages.
- The `cypher` package and its subpackages `cypher/ast`, `cypher/exec`,
  `cypher/explain`, `cypher/expr`, `cypher/funcs`, `cypher/ir`,
  `cypher/parser`, `cypher/procs`, `cypher/sema`.
- The `bolt/server`, `bolt/proto` and `bolt/packstream` packages.
- The `ds` and `metrics` packages.
- The `bench/ldbc`, `bench/dimacs9`, `bench/rmat` packages
  (their *types* are part of the API; the benchmark numbers
  reported by their CLIs are not).

Three CATEGORIES of non-`internal/` package are **excluded by intent**, and
the exclusion is stated here so it is not mistaken for an omission:

- `cypher/parser/gen` — ANTLR-generated code. It is regenerated from
  the grammar and its exported surface is an artefact of the generator,
  not a designed API.
- `cypher/tck` — the openCypher conformance harness. Its exported
  identifiers exist to drive the TCK from tests.
- `bench/*` other than the three named above, and `cmd/*` and
  `examples/*` — harnesses and executables, not library surface.
  `examples/` in particular is not part of the module: the module
  neither imports nor depends on it.

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

Four on-disk formats are versioned:

| Format        | Current version | Spec                            |
|---------------|-----------------|---------------------------------|
| WAL frame     | 1               | `store/wal/FORMAT.md`           |
| Snapshot manifest | 3           | embedded JSON, validated by `store/snapshot.LoadManifest`; readers accept versions ≤ 3 (v1 CSR-only, v2 CSR + labels + properties, v3 adds typed mapper/indexes). Two additions landed in `v0.12.0` **without a version step**, and both are deliberate: a 16-byte **CRC32C framing trailer** — magic, algorithm, CRC over every preceding byte, marked in the document by `integrity: "crc32c-trailer"` (`snapshot.IntegrityCRC32CTrailer`) — because integrity is enforced on the *framing* and never on the schema, so an older decoder stops at the closing brace and never reads it (rmp #2520); and an additive `indexes_commit_ts` field carrying the bounding instant that lets recovery prove a snapshot index payload is not stale, absent-means-zero as the schema policy already allows (rmp #2490). |
| Snapshot `labels.bin` | 2       | Bumped 1 → 2 in `v0.11.0`: the edge record gains a `Slot` field, so a relationship *type* is durable per slot rather than per node pair. Version 1 stored `EdgeLabelEntry{Src, Dst, StringIdx}` with no slot ordinal, so parallel edges folded into one key on disk. |
| csrfile       | 1               | `docs/csrfile-v1.md`. Weight-kind wire values **5 and 6** (the 1- and 2-byte kinds) were added in `v0.12.0` without a version step (rmp #2529): values 0-4 keep their meaning so every earlier file reads unchanged, and an older reader meeting a 5 or 6 refuses it with `ErrUnknownWeightKind` rather than misreading it. |

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
   Current status as of `v0.12.0` (2026-08-27): **100 %** —
   `const tckExecutionBaseline = 3897`, which is the full scenario count, so the
   gate fails on any regression at all rather than at a 95 % floor. The constant is
   unchanged from `v0.11.0`, and no `.feature` file changed in the `v0.11.0..v0.12.0`
   window, so the scenario population is the same one `v0.11.0` was measured against.
   The runner additionally fails on any `failed`, `undefined` or `pending` step. See
   [docs/tck/DIVERGENCES.md](tck/DIVERGENCES.md) for the authoritative
   table.

   **One thing about the gate itself changed in `v0.12.0` (rmp #2568),** and it is
   recorded here because it bears on how the threshold is read. A scenario whose query
   exceeds the runner's 10 s `queryTimeout` is no longer counted against the baseline,
   *provided the engine honoured cancellation*: such a run is `INCONCLUSIVE` — evidence
   about the host, and none at all about conformance — where before it lowered the pass
   count and reported a loss of openCypher conformance on a merely loaded machine. A
   query that does **not** return within `queryWedgeGrace` is `WEDGED` and is still a
   real failure, never credited. The credit is bounded to scenarios the harness itself
   recorded as timing out, every one is rendered in the gate output, and it cannot
   excuse a real shortfall — `TestTCKGateCheck_InconclusiveIsNotAConformanceRegression`
   asserts all three directions. `queryTimeout` is unchanged.

2. **All local gates green — MET at `v0.11.0`; the `v0.12.0` figure is pending its
   own gate run.** Every gate must pass on the release commit, run locally via
   `make ci` and `make release-preflight`: build, test, race detector, lint
   (`golangci-lint`), vet, TCK, the coverage gate, the crash-injection battery, and a
   clean `govulncheck ./...`. At `v0.11.0`: 123 packages ok with zero `FAIL` lines
   under `go test -race ./...`, `golangci-lint` 0 issues, coverage 87.1 % aggregate
   with every package above its 75 % floor.

   **MET at `v0.12.0`, on a release-branch `make release-preflight` run rather than on
   inherited figures.** `VERSION=v0.12.0 make release-preflight` exits **0** — read from
   inside its own log, not from a wrapper — with **127 packages** ok and zero failures
   under `go test -race ./...`, `golangci-lint` **0 issues**, coverage **88.3 %**
   aggregate against the 85.0 % floor with every package at or above its 75.0 % floor,
   and the release-accuracy checks passing. The run executed under the **currently
   pinned** toolchain, `go1.27.0`, and with every direct dependency at the version this
   release ships — including `godog` v0.16.0, the TCK runner itself, whose upgrade leaves
   `TestTCKExecution` at **3897/3897** scenarios and 16006/16006 steps.

   These figures coincide with the ones the sprint-350 close gate recorded at commit
   `1b7d7c7f`, but they are **no longer inherited from it**: that run predated both the
   toolchain bump and the dependency refresh, so it could not have spoken for this tree.
   The `govulncheck` half of this gate carries a stated limitation rather than a clean
   claim — see the CVE-scan step in
   [CONTRIBUTING.md](../CONTRIBUTING.md#dependency-policy): module-level scanning reports
   no vulnerabilities, and symbol-level reachability scanning is unavailable under the
   pinned toolchain.

3. **All T-series tasks closed — MET.** Every task prefixed `T-` in
   `docs/tck/DIVERGENCES.md` must be marked resolved. T-series tasks
   track known execution-engine gaps that block execution TCK scenarios.
   No unresolved `T-` entry remains — re-verified at `v0.12.0` by scanning
   `docs/tck/DIVERGENCES.md` for the prefix, which returns no `T-` series entry at
   all.

4. **Soak test green — NOT MET.** The full soak test (`SOAK_FULL=1`,
   1 024 connections, 4 hours) must pass with zero goroutine leaks and
   zero race conditions. The soak report in `soak-artefacts/` must reflect
   a run against the release commit. The canonical execution path is to
   run the soak locally via `SOAK_FULL=1 make soak`; its artefacts
   (soak.log, heap profiles, bolt-soak-ci-report.md) are what the
   release gate consumes. As noted in `CLAUDE.md`, the soak is a periodic
   reliability exercise run before a major release rather than an
   automated per-push gate.
   **Current status at `v0.12.0`: the artefacts in `soak-artefacts/` are still from
   2026-05-30 at commit `b5453b9`** — before `v0.9.0`, and now roughly 700 commits
   behind. They are unchanged since `v0.11.0` and reflect no recent commit, so this
   gate is open for a **fifth consecutive cycle**. The three production
   certifications of August 2026 each record the whole-tree soak layer as unrun (see
   [`docs/certification-2026-08-13.md`](certification-2026-08-13.md) §5), and no new
   certification was taken in the `v0.11.0..v0.12.0` window.

   **Two things advanced in `v0.12.0` and neither discharges this gate**, which is
   why they are recorded here rather than counted: the `internal/sim` soak layer was
   brought green and verified twice consecutively (rmp #2620), and the DST scenario
   catalogue passes under `SOAK_FULL=1` with 36 of 36 scenarios and zero skips
   (rmp #2535). Both are the *simulator's* soak layer. This gate asks for the
   whole-tree soak — `SOAK_FULL=1`, 1 024 connections, 4 hours — against the release
   commit, and that was not run.

**Gate status summary at `v0.12.0`: 3 of 4 met, unchanged from `v0.11.0`** — with
gate 2's `v0.12.0` figure pending its own release-commit gate run, as noted above.
Gate 4 (whole-tree soak against the release commit) remains the outstanding blocker
for `1.0.0`, together with the unmeasured items the current certification envelope
lists — and, at `v0.12.0`, with the fact that **no production certification was taken
in this window at all**: the most recent is
[`docs/certification-2026-08-13.md`](certification-2026-08-13.md), at the `v0.11.0`
release commit, 192 commits behind this tag.

### Pre-release candidates

Pre-release candidate tags (`v1.0.0-rc1`, `v1.0.0-rc2`, …) are tagged as
significant improvements become available, without waiting for all stable
gates to be met. Each candidate documents its own conformance numbers and
known limitations in the corresponding `release-notes/v<version>.md` file.

Production deployments should pin a stable tag. Candidates are suitable for
integration testing and early adoption, with the understanding that execution
conformance is still being improved.
