# Release process

GoGraph follows a tag-driven release process orchestrated by
[goreleaser](https://goreleaser.com/).

## Pre-flight (manual)

Before tagging a new release:

1. All tests pass:

   ```bash
   make ci
   ```

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
The policy below is enforced via repo Settings → Branches and is
mirrored here as the canonical reference; changes to the repo
configuration must be reflected in this file.

### `main` branch

- **Require a pull request before merging.** Direct pushes are
  rejected by GitHub regardless of the actor's role.
- **Require at least one approving review.** A maintainer's
  self-approval does not count.
- **Dismiss stale approvals on push.** A force-pushed branch loses
  its review and must be re-approved.
- **Require status checks to pass.** The following checks from
  `.github/workflows/ci.yml` are required:
  - `build + test + race`
  - `golangci-lint`
  - `concurrency stress (race)`
  - `govulncheck`
  - `coverage gate`
  - `benchstat regression gate` (PRs only)
- **Require linear history.** Merge commits on `main` are rejected;
  use rebase or squash.
- **Require signed commits** for repository contributors with
  write access. Sign with the key registered at GitHub Settings →
  SSH and GPG keys.
- **Restrict push for tags `v*`.** Only members of the `releasers`
  team may push a release tag. The `Release` workflow at
  `.github/workflows/release.yml` re-runs `go vet`, `go build`,
  `go test -race`, and the module-integrity check on the release
  commit before goreleaser publishes — the upstream protection
  prevents an unreviewed tag from reaching this step in the first
  place.

### `v*` tags

- **Restrict push.** Only the `releasers` team may push a tag
  matching `v[0-9]*`. Even a maintainer's regular push is
  rejected.
- **Require signed tags.** All release tags are
  `git tag -s` signed.

## Go toolchain upgrade workflow

GoGraph pins both a language version (`go 1.26`) and an explicit
toolchain version (`toolchain go1.26.3`) in `go.mod`. CI workflows
(`.github/workflows/ci.yml`, `release.yml`, `tck.yml`) all consume
the toolchain via `go-version-file: go.mod`, so a single edit to
`go.mod` propagates the bump to every CI job — there is exactly one
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
on the tag push and runs goreleaser with `GITHUB_TOKEN` from the
default actions secret. The result is a **draft** release on
GitHub — review the artefact list (source tarballs, soak-harness
binaries for linux/darwin × amd64/arm64, checksums) and publish
manually.

## Local fallback

If the workflow is unavailable, you can publish from a workstation:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
VERSION=vX.Y.Z make release
```

The local `release` target requires `goreleaser` on the PATH and a
clean working tree. It also depends on the `release-preflight` target,
which enforces the following gates BEFORE goreleaser is invoked:

1. `VERSION` is set.
2. CHANGELOG.md contains a `## [VERSION]` entry (the Unreleased
   section must have been promoted).
3. release-notes/VERSION.md exists.
4. `make lint` is clean (golangci-lint).
5. `make cover-gate` is green (aggregate ≥ 85 %, per-package ≥ 75 %).
6. `scripts/run_headline_bench.sh` exits zero when present (informational
   per-tag run; the canonical comparison gate is the PR-time
   `benchstat regression gate` in `.github/workflows/ci.yml`).

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
