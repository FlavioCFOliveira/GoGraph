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
4. `go vet`, `golangci-lint`, and the project's local validation
   pipeline (`make ci`) are green.

The public helper [`csrfile.Reinterpret`](store/csrfile/reinterpret.go)
is the recommended primitive for new code that needs to retype the
body of a memory-mapped region.

## Validation pipeline

Every change must pass `make ci`, which runs:

- `go mod tidy` (fails on `go.mod` / `go.sum` drift)
- `gofmt`
- `go vet ./...`
- `go build ./...`
- the short test layer under the race detector (`go test -race ./...`)
- `golangci-lint run ./...`
- the coverage gate (`cover-gate`): **≥ 85 % aggregate** and
  **≥ 75 % per-package** statement coverage

In addition, the deferred test layers must be compiled via
`make check-soak-build` (build + `go vet` under `-tags=soak` and
`-tags=soak,nightly`). The short layer never compiles `soak`/`nightly`-tagged
files, so without this guard a compile break in those long-running ACID and
crash-safety tests would pass the short gate and surface only when the soak
or nightly layer is next run. Run `make check-soak-build` locally before
pushing a change that touches any `//go:build soak` or `//go:build nightly`
file.

Benchmarks must be run for hot-path changes; the per-package
README or task summary should record the measured numbers.

## Branch and tag protection

**There is none.** Neither `main` nor the `v*` tag namespace carries
any GitHub-native protection: classic branch protection, rulesets and
tag protection were all probed on 2026-09-05 and all three are absent.
A direct push to `main`, or of a release tag, by an account with write
access **succeeds**.

There are likewise no GitHub status checks. Correctness and compliance
are enforced **locally** via `make ci` (and `make release-preflight`
before tagging), which every contributor must run green before
pushing. That local gate is the only real control, so treat running it
as obligatory rather than advisory — nothing downstream will catch a
change that skipped it.

Release tags are annotated (`git tag -a`) but **not signed**, and no
`releasers` team exists. The intended protection regime, the evidence
that it is not yet active, and what adopting it would require are
recorded in
[docs/release.md](docs/release.md#branch-and-tag-protection-policy);
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

2. **No incidental drift.** `make ci` runs `go mod tidy` (the `tidy`
   step) and fails when `go.mod` or `go.sum` is not already idempotent;
   run it locally before pushing. Anyone adding, removing or upgrading
   a dependency must commit the resulting tidy delta together with their
   code change so reviewers see the dependency move alongside the code
   that needs it.

3. **Integrity check.** The release pre-flight (see
   [docs/release.md](docs/release.md#pre-flight-manual)) runs
   `go mod download` and `go mod verify` after the tidy check.
   Verification fails if any downloaded module's content does not match
   its checksum in `go.sum`, catching tampered proxies, mid-flight
   corruption, and any forged `go.sum` entries.

4. **Periodic CVE scan.** Run the scan locally before pushing a
   dependency change and before tagging a release. A new CVE against a
   pinned version surfaces as a failure, prompting an explicit bump.

   ```bash
   make vulncheck                                        # the gate: run this, not govulncheck by hand
   go install golang.org/x/vuln/cmd/govulncheck@latest   # only if the gate says no usable binary
   ```

   **`make vulncheck` is the supported route and it runs inside `make ci`.**
   It resolves a `govulncheck` built for the current toolchain — ignoring
   `PATH` order, which may shadow a stale one — and then asserts that the
   scan *actually analysed the module* rather than reading the exit
   status. Do not invoke `govulncheck` by hand for a release decision;
   the whole point of the gate is that the failure below is invisible to
   a human reading a green exit code.

   **Two traps, both hit while preparing `v0.12.0`.** A `govulncheck`
   binary built against an older Go minor than the one on `PATH`
   **exits 0 while performing no analysis at all** — it prints
   "Loading packages failed, possibly due to a mismatch between the Go
   version used to build govulncheck and the Go version on PATH" and
   returns success, so the scan silently does not happen. Always
   reinstall it after a toolchain bump, and require the run to print
   either a finding or the literal `No vulnerabilities found.`; treat
   empty output as a failed scan, never as a clean one. Second,
   `govulncheck@v1.3.0`'s source-processing packages are built for
   go1.26 and **cannot parse go1.27 source**.

   **Both traps are now closed by `make vulncheck` (rmp #2722), and the
   guidance that stood here is superseded.** This section previously
   prescribed `-scan=module` "until an upstream release built for the
   pinned Go minor is available" — **`govulncheck@v1.7.0` is that
   release**, so module-level scanning is no longer necessary and would
   now be a *weaker* scan than the gate performs. The gate asserts
   `scan_level=symbol` precisely so a module-level run cannot pass as
   full reachability analysis. It also asserts that every package
   `go list ./...` reports was actually scanned, so a narrowed pattern
   fails rather than certifying from a fraction of the module.

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
