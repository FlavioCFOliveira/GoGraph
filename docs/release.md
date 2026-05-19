# Release process

GoGraph follows a tag-driven release process orchestrated by
[goreleaser](https://goreleaser.com/).

## Pre-flight (manual)

Before tagging a new release:

1. All tests pass:

   ```bash
   make ci
   ```

2. CHANGELOG.md has a new `## [vX.Y.Z] — YYYY-MM-DD` entry summarising
   the work landed since the previous tag. Follow the Keep-a-Changelog
   format: Added / Changed / Fixed / Removed / Performance / Security.

3. Release notes — long-form narrative for the
   `release-notes/vX.Y.Z.md` file — are drafted.

4. The `.goreleaser.yaml` config is rendered cleanly:

   ```bash
   make release-check
   ```

   This runs goreleaser in snapshot mode without publishing.

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
clean working tree.

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

## Semver policy

GoGraph follows [Semantic Versioning](https://semver.org/):

- **MAJOR** bumps when a breaking change to the exported Go API
  ships. Pre-1.0 the minor digit absorbs breaking changes.
- **MINOR** bumps when net-new functionality (a new search algorithm,
  a new graph format) is added in a backwards-compatible way.
- **PATCH** bumps for bug fixes and performance improvements that
  preserve every previously-documented API contract.

See docs/semver.md for the policy in detail.
