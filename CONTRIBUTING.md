# Contributing to GoGraph

This document captures the policies that complement the runtime
contracts already documented in `CLAUDE.md`.

## Task tracking via the local rmp CLI

GoGraph's planning is owned by the `rmp` CLI (the Groadmap tool),
installed locally and available at `~/.local/bin/rmp`. It is the
sole source of truth for sprints, tasks, dependencies, and audit
history; no GitHub Issues, Notion pages, or spreadsheets parallel
it. Every change that lands on `main` traces back to an `rmp` task
identifier referenced in the commit footer (`Closes rmp task #NNN`).

Common workflows:

```bash
# What's next?
rmp task next -r gograph

# Look at the current open sprint
rmp sprint list -r gograph --status OPEN

# Read a specific task before starting
rmp task get <id> -r gograph

# Move a task through its lifecycle
rmp task stat <id> DOING     -r gograph
rmp task stat <id> TESTING   -r gograph
rmp task stat <id> COMPLETED -r gograph --summary "..."

# Audit history (who changed what, when)
rmp audit history TASK <id> -r gograph
```

The roadmap database for this project is `gograph` (file at
`~/.roadmaps/gograph.db`). All `rmp` commands require `-r gograph`.
Refer to the binary's `--help` output for the full command surface
and to the project's `CLAUDE.md` for the planning rituals.

## Use of `unsafe`

GoGraph reserves the `unsafe` package for the small set of patterns
that genuinely require it: zero-copy reinterpretation of memory-
mapped regions and the implementation of the lock-free
[`adjlist`](graph/adjlist/) slot table. Every use of `unsafe` is
expected to satisfy the following:

1. The site has a `//nolint:gosec` comment that succinctly states
   *why* the reinterpretation is sound.
2. The exported helper carries a documentation note that lists the
   invariants the caller must uphold (e.g. lifetime, mutability,
   alignment).
3. Race-detector tests cover the helper.
4. `go vet`, `golangci-lint`, and the project's CI pipeline are
   green.

The public helper [`csrfile.Reinterpret`](store/csrfile/reinterpret.go)
is the recommended primitive for new code that needs to retype the
body of a memory-mapped region.

## Validation pipeline

Every change must pass `make ci`:

- `gofmt`
- `go vet ./...`
- `go build ./...`
- `go test ./...`
- `go test -race ./...`
- `golangci-lint run ./...`

Benchmarks must be run for hot-path changes; the per-package
README or task summary should record the measured numbers.

## Branch and tag protection

The `main` branch and the `v*` tag namespace are protected on GitHub.
Contributors cannot push directly to `main`; every change lands via
a pull request that must pass the required CI checks and obtain at
least one approving review. Release tags (`v[0-9]*`) can only be
pushed by the `releasers` team and must be signed. The full policy
is documented in [docs/release.md](docs/release.md#branch-and-tag-protection-policy);
any change to the repo settings must be reflected there.

## Dependency policy

GoGraph treats every change to `go.mod` or `go.sum` as a deliberate
decision. The policy below applies to both direct and indirect
dependencies:

1. **Exact pinning.** Versions recorded in `go.mod` are the exact
   minimum versions the build must use. Go modules already pin
   minimum versions by default; this policy adds the discipline that
   bumping a version is a discrete, reviewable change rather than an
   incidental side-effect of `go get -u ./...`.

2. **No incidental drift.** The `Tidy check` step in
   `.github/workflows/ci.yml` runs `go mod tidy` on every PR and
   fails when `go.mod` or `go.sum` is not already idempotent. Anyone
   adding, removing or upgrading a dependency must commit the
   resulting tidy delta together with their code change so reviewers
   see the dependency move alongside the code that needs it.

3. **Integrity check.** The same workflow runs `go mod download` and
   `go mod verify` after the tidy check. Verification fails if any
   downloaded module's content does not match its checksum in
   `go.sum`, catching tampered proxies, mid-flight corruption, and
   any forged `go.sum` entries.

4. **Periodic CVE scan.** `govulncheck ./...` runs daily (cron
   `13 4 * * *`) and on every PR. A new CVE against a pinned version
   surfaces as a CI failure, prompting an explicit bump.

5. **Upgrade workflow.** To upgrade a dependency:

   ```bash
   go get -u <module>@<version>     # bump just that module
   go mod tidy                      # propagate to indirect graph
   go mod verify                    # confirm checksums
   make ci                          # rerun the full pipeline locally
   ```

   The PR description must cite the upstream changelog or release
   notes covering the new version (so reviewers can see what changed
   between the old and the new pin).

6. **Indirect dependencies.** Indirect entries in `go.mod` are
   managed by `go mod tidy`; do not edit them by hand. If an
   indirect dependency needs a specific version (for example, to
   pick up a security fix that has not yet been required by a
   direct dependency), add the explicit `require` block with a
   `// indirect` comment and document the reason in the PR.
