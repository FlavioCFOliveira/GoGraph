# Release process

GoGraph follows a tag-driven release process orchestrated by
[goreleaser](https://goreleaser.com/).

## Pre-flight (manual)

Before tagging a new release:

1. The canonical local gate is green:

   ```bash
   VERSION=vX.Y.Z make release-preflight
   ```

   `make release-preflight` **subsumes** `make ci` — it runs the
   release-accuracy checks, then the full `make ci` correctness+coverage
   gate exactly once, then the headline benchmark. Do **not** run `make ci`
   separately as well; that would execute the whole `go test -race ./...`
   and coverage suite a second time for no added assurance. Run `make ci`
   on its own only for day-to-day iteration between releases.

2. Dependency integrity holds:

   ```bash
   go mod tidy
   go mod download
   go mod verify
   ```

   The tree must be clean afterwards (no unexpected `go.mod` /
   `go.sum` delta). The dependency policy in
   [CONTRIBUTING.md](../CONTRIBUTING.md#dependency-policy) governs
   how upgrades are landed between releases.

3. CHANGELOG.md has a new `## [vX.Y.Z] — YYYY-MM-DD` entry summarising
   the work landed since the previous tag. Follow the Keep-a-Changelog
   format: Added / Changed / Fixed / Removed / Performance / Security.

4. Release notes — long-form narrative for the
   `release-notes/vX.Y.Z.md` file — are drafted.

5. The `.goreleaser.yaml` config is rendered cleanly:

   ```bash
   make release-check
   ```

   This runs goreleaser in snapshot mode without publishing.

## Branch and tag protection policy

The `main` branch and the `v*` tag namespace are protected on GitHub.
The GitHub-native rules below are enforced via repo Settings →
Branches/Tags and are mirrored here as the canonical reference;
changes to the repo configuration must be reflected in this file.

Correctness and compliance are **not** enforced by GitHub status
checks — there is no per-push or per-PR CI. The only GitHub Actions
workflow is `.github/workflows/release.yml`, which runs on a `v*` tag
push. Every correctness and compliance gate (`go vet`, `go build`,
`go test -race ./...`, `golangci-lint`, the openCypher TCK
execution + conformance gate, the coverage gate, the crash-injection
battery, `govulncheck`, and `go mod tidy`) runs **locally** before a
developer pushes or tags — via `make ci` for day-to-day work and the
canonical `make release-preflight` gate before tagging a release.
Enforcement is by developer discipline plus the local
`make release-preflight` gate; a change that fails any gate must not
be merged or tagged.

### `main` branch

- **Require a pull request before merging.** Direct pushes are
  rejected by GitHub regardless of the actor's role.
- **Require at least one approving review.** A maintainer's
  self-approval does not count.
- **Dismiss stale approvals on push.** A force-pushed branch loses
  its review and must be re-approved.
- **Require linear history.** Merge commits on `main` are rejected;
  use rebase or squash.
- **Require signed commits** for repository contributors with
  write access. Sign with the key registered at GitHub Settings →
  SSH and GPG keys.
- **Restrict push for tags `v*`.** Only members of the `releasers`
  team may push a release tag. The `Release` workflow at
  `.github/workflows/release.yml` runs the release-accuracy gate and
  then goreleaser; it does not re-run the correctness gates, which the
  releaser has already run locally via `make release-preflight` before
  pushing the tag. The GitHub tag restriction prevents an unreviewed
  tag from reaching this workflow in the first place.

### `v*` tags

- **Restrict push.** Only the `releasers` team may push a tag
  matching `v[0-9]*`. Even a maintainer's regular push is
  rejected.
- **Require signed tags.** All release tags are
  `git tag -s` signed.

## Go toolchain upgrade workflow

GoGraph pins both a language version (`go 1.26`) and an explicit
toolchain version (`toolchain go1.26.3`) in `go.mod`. The release
workflow (`.github/workflows/release.yml`) consumes the toolchain via
`go-version-file: go.mod`, and the local gates (`make ci`,
`make release-preflight`) use the same `go.mod` directive, so a single
edit to `go.mod` propagates the bump everywhere — there is exactly one
source of truth.

To bump the toolchain to a new patch level (for example `go1.26.4`):

1. Install the new toolchain locally:

   ```bash
   go install golang.org/dl/go1.26.4@latest
   go1.26.4 download
   ```

2. Edit `go.mod` to set the new `toolchain` directive:

   ```diff
   -toolchain go1.26.3
   +toolchain go1.26.4
   ```

   Do not change the `go` directive in the same commit unless a new
   minor language version is also being adopted; the `go` directive
   gates language features and triggers a semver-MAJOR consideration
   pre-1.0.

3. Re-run the full validation pipeline:

   ```bash
   make ci
   make soak-smoke
   ./scripts/run_headline_bench.sh
   ```

4. Commit the `go.mod` change in isolation with a `chore(toolchain):`
   prefix so the bump is bisectable. Cite the upstream release notes
   (https://go.dev/doc/devel/release) in the commit body.

5. Cite the toolchain bump in the next CHANGELOG.md entry under
   `Changed`. If the new toolchain fixes a CVE relevant to GoGraph,
   also cite it under `Security`.

A minor language bump (for example moving from `go 1.26` to `go 1.27`)
follows the same workflow with two additions: a survey of new
language features the project chooses to adopt, and a check that no
direct or indirect dependency requires a still-newer minor that the
project is not ready to absorb.

## Dependency-update workflow between releases

Between tagged releases, dependency upgrades follow the steps in
[CONTRIBUTING.md](../CONTRIBUTING.md#dependency-policy). A
release-blocking upgrade (CVE in a pinned dependency, breaking change
in the standard library at the new Go toolchain) follows the same
workflow with the additional discipline of:

1. Landing the dependency bump as its own commit, separate from the
   release prep commit, so the diff is bisectable.
2. Re-running `make ci`, `make soak-smoke`, and the headline
   benchmarks (`./scripts/run_headline_bench.sh`) after the bump to
   confirm no behavioural or performance regression.
3. Citing the upstream advisory or changelog entry in the
   CHANGELOG.md entry for the next release under either `Security`
   (for CVEs) or `Changed` (for behavioural deltas).

## Tag and push

```bash
git tag -a vX.Y.Z -m "GoGraph vX.Y.Z"
git push origin vX.Y.Z
```

The `Release` workflow at `.github/workflows/release.yml` triggers
on the tag push and runs `VERSION=<tag> make release-accuracy` — the
release-doc consistency gate — and then invokes goreleaser with
`GITHUB_TOKEN` from the default actions secret. The result is a
**draft** release on GitHub — review the artefact list (source tarballs,
soak-harness binaries for linux/darwin × amd64/arm64, checksums) and
publish manually.

The workflow deliberately does **not** re-run the correctness gates.
Before pushing the tag, the releaser must run the canonical
`VERSION=<tag> make release-preflight` gate locally (see the gate list
below); that gate — not GitHub — is what guarantees the tagged commit
passes vet/build/-race/lint/TCK/coverage before it is published.

## Local fallback

If the workflow is unavailable, you can publish from a workstation:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
VERSION=vX.Y.Z make release
```

The local `release` target requires `goreleaser` on the PATH and a
clean working tree. It depends on the `release-preflight` target — the
single canonical gate the releaser runs before publishing (whether via
`make release` here or by pushing a tag for the workflow) — which runs,
in order, BEFORE goreleaser is invoked:

**Release-accuracy** (`make release-accuracy` — release-doc consistency):

1. `VERSION` is set.
2. CHANGELOG.md contains a `## [VERSION]` entry (the Unreleased
   section must have been promoted).
3. release-notes/VERSION.md exists.
4. README.md "Current release" names `VERSION`.
5. SECURITY.md supported-versions table names `VERSION`'s `vX.Y.x` line.
6. docs/benchmarks/VERSION.md exists (per-release benchmark/load-test
   numbers).

**Correctness + coverage** (`make ci`, run exactly once):

7. `make ci` is green — the full correctness+coverage gate: `go mod tidy`,
   `gofmt`/`goimports`, `go vet ./...`, `go build ./...`,
   `go test -race ./...` (which includes the `cypher/tck`
   `TestTCKExecution` =100 % execution baseline, so a TCK regression fails
   this gate), `golangci-lint run ./...`, and `make cover-gate`
   (aggregate ≥ 85 %, per-package ≥ 75 %). The suite runs once here — the
   gate does not re-run it. (`scripts/pre-release.sh` is a separate
   standalone convenience gate that runs vet/build/-race/lint without
   coverage; it is **not** invoked by `release-preflight`.)

**Performance** (informational on a release tag):

8. `scripts/run_headline_bench.sh` exits zero when present (informational
   per-tag run; the benchstat comparison gate `scripts/bench_gate.sh` is
   run locally before a change lands, comparing the candidate against its
   baseline).

Each failure exits non-zero with a one-line explanation of what is
missing. Run `make release-preflight` on its own to dry-run the gates
without invoking goreleaser.

## What goreleaser ships

Per the `.goreleaser.yaml` in the repo root:

- A source-tree tarball per (OS, arch) pair: `linux/amd64`,
  `linux/arm64`, `darwin/amd64`, `darwin/arm64`.
- The static `soak` binary for the same matrix — a single-file
  reliability driver that downstream consumers can drop on a host
  and run to validate their build.
- `checksums.txt` (SHA-256).
- Auto-generated changelog excerpt from `git log` between the
  previous and current tag (used only as the body header; the
  authoritative changelog is CHANGELOG.md).

## Software Bill of Materials (SBOM)

Every release artefact ships with a CycloneDX SBOM
(`<archive>.cdx.json`) alongside `checksums.txt`. The SBOM is
produced by `cyclonedx-gomod` and includes every direct and
indirect Go module the build pulled in, with license metadata for
each. Consumers who need supply-chain attestation (SLSA, audit,
procurement) read the SBOM rather than reverse-engineering
`go.mod`.

Local fallback to generate the SBOM against the current checkout:

```bash
go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest
cyclonedx-gomod mod -licenses -json -output gograph.cdx.json
```

The published SBOM is generated by the `Release` workflow at
release time (see `.github/workflows/release.yml`); the
`.goreleaser.yaml` `sboms:` stanza pairs one document per archive.

## Semver policy

GoGraph follows [Semantic Versioning](https://semver.org/):

- **MAJOR** bumps when a breaking change to the exported Go API
  ships. Pre-1.0 the minor digit absorbs breaking changes.
- **MINOR** bumps when net-new functionality (a new search algorithm,
  a new graph format) is added in a backwards-compatible way.
- **PATCH** bumps for bug fixes and performance improvements that
  preserve every previously-documented API contract.

See docs/semver.md for the policy in detail.
