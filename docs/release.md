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

**No GitHub-native branch or tag protection is in force on this
repository.** That is the measured present state, not the intent, and
it is stated first because a release document that asserts a control it
does not have is worse than one that admits the gap — the same
reasoning this file already applies to commit and tag signing below.

Probed against the live repository on **2026-09-05**, at the
`v0.13.0` release:

```console
$ gh api repos/FlavioCFOliveira/GoGraph/branches/main/protection
{"message":"Branch not protected","status":"404"}

$ gh api repos/FlavioCFOliveira/GoGraph/rulesets
[]

$ gh api repos/FlavioCFOliveira/GoGraph/tags/protection
{"message":"Not Found","status":"404"}
```

Classic branch protection is absent, the newer rulesets mechanism is
empty, and tag protection is absent. The repository's own history
corroborates it: `main` carries `f97bbfec`, a **merge commit**, which
the "require linear history" rule described below as active would have
rejected outright.

Consequently, on `main` and on the `v*` tag namespace, a direct push
by an account with write access **succeeds**. Nothing but developer
discipline stops it.

### What actually gates a release

Correctness and compliance are **not** enforced by GitHub status
checks — there is no per-push or per-PR CI. The only GitHub Actions
workflow is `.github/workflows/release.yml`, which runs on a `v*` tag
push and executes the release-accuracy gate plus goreleaser; it does
not re-run the correctness gates. Every correctness and compliance
gate (`go vet`, `go build`, `go test -race ./...`, `golangci-lint`,
the openCypher TCK execution + conformance gate, the coverage gate,
the crash-injection battery, `govulncheck`, and `go mod tidy`) runs
**locally** before a developer pushes or tags — via `make ci` for
day-to-day work and the canonical `make release-preflight` gate before
tagging a release.

This is the one control that is real, and it is verified per release:
`make release-preflight` exits non-zero on any failing gate, and the
releaser reads that exit status from inside the run log. Enforcement
is by developer discipline plus that local gate; a change that fails
any gate must not be merged or tagged.

### Intended controls, none of them yet active

The rules below are the **intended** protection regime. They are
recorded here as a target to configure, explicitly **not** as a
description of the repository's present state. Adopting any of them
means changing the repo configuration first and then updating this
section to match — never the reverse.

On `main`:

- **Require a pull request before merging**, so a direct push is
  rejected regardless of the actor's role.
- **Require at least one approving review**, with a maintainer's
  self-approval not counting.
- **Dismiss stale approvals on push**, so a force-pushed branch loses
  its review and must be re-approved.
- **Require linear history.** Note that this one conflicts with the
  project's gitflow model, which merges `release/*` into `main` with
  `--no-ff`; adopting it means changing the branching model too, and
  that decision has not been taken.
- **Require signed commits.** `git log --pretty=%G?` reports `N` — no
  signature — for **all 99 commits** in the `v0.12.0..v0.13.0` window,
  because no signing key is configured on the release workstation.

On the `v*` tag namespace:

- **Restrict push** to a `releasers` team, so that an unreviewed tag
  cannot reach the `Release` workflow in the first place. No such team
  exists today.
- **Signed tags.** Release tags are currently annotated but
  **unsigned** — `v0.10.0`, `v0.11.0`, `v0.12.0` and `v0.13.0` are all
  `git tag -a` objects, verified with `git cat-file -t`. Adopting
  `git tag -s` requires a signing key, a documented key-custody
  process, and the matching GitHub tag rule; until those exist, do not
  claim signed tags anywhere.

## Go toolchain upgrade workflow

GoGraph pins both a language version (`go 1.26`) and an explicit
toolchain version (`toolchain go1.27.0`) in `go.mod`. The release
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

Per the `.goreleaser.yaml` in the repo root, a tag release publishes
**five** assets:

- One source-and-tools tarball per (OS, arch) pair — `linux/amd64`,
  `linux/arm64`, `darwin/amd64`, `darwin/arm64`. Each tarball bundles,
  for that platform, the static `soak` binary (a single-file
  reliability driver consumers can drop on a host and run to validate
  their build), the project docs (`README.md`, `CHANGELOG.md`,
  `LICENSE`, `SECURITY.md`, `docs/**`), and the CycloneDX SBOM
  (`gograph.cdx.json`). The `soak` binary and the SBOM ship **inside**
  each tarball, not as separate downloadable assets.
- `checksums.txt` (SHA-256 over the four tarballs).

The GitHub release body is headed by an auto-generated changelog
excerpt from `git log` between the previous and current tag (used
only as the body header; the authoritative changelog is
CHANGELOG.md).

## Software Bill of Materials (SBOM)

Each release tarball embeds a single CycloneDX SBOM,
`gograph.cdx.json`, produced by `cyclonedx-gomod`. It includes every
direct and indirect Go module the build pulled in, with license
metadata for each. Consumers who need supply-chain attestation
(SLSA, audit, procurement) read the SBOM rather than
reverse-engineering `go.mod`. The Go module graph does not vary by
OS/arch, so the same document is embedded into all four archives
instead of being emitted once per archive.

Local fallback to generate the SBOM against the current checkout:

```bash
go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0
cyclonedx-gomod mod -licenses -json -output gograph.cdx.json
```

At release time the SBOM is generated by the `Release` workflow
(see `.github/workflows/release.yml`), which installs
`cyclonedx-gomod` pinned to `v1.10.0` and lets goreleaser invoke it
through the `before:` hook in `.goreleaser.yaml`. There is no
`sboms:` stanza: the document is generated once by that hook and
embedded into every archive via the `archives.files` list.

## Semver policy

GoGraph follows [Semantic Versioning](https://semver.org/):

- **MAJOR** bumps when a breaking change to the exported Go API
  ships. Pre-1.0 the minor digit absorbs breaking changes.
- **MINOR** bumps when net-new functionality (a new search algorithm,
  a new graph format) is added in a backwards-compatible way.
- **PATCH** bumps for bug fixes and performance improvements that
  preserve every previously-documented API contract.

See docs/semver.md for the policy in detail.
