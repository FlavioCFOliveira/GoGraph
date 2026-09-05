# The recipe shell, and WHY the flags are on SHELL itself and not only on
# .SHELLFLAGS (rmp #2672). `.SHELLFLAGS` was introduced in GNU Make 3.82, and
# macOS still ships GNU Make 3.81, where it is silently IGNORED — not warned
# about, ignored. Under 3.81 that left EVERY recipe line in this file without
# -e, -u and -o pipefail, and the consequence was measured, not theorised:
# `test-short` runs `go test ... | pkg_time_budget.sh`, so without pipefail the
# line reported only the budget script's status, and a run with a genuinely
# FAILING package (bench/audit352) came back as `MAKE_CI_EXIT=0` with make going
# on to run test-timing, lint and cover-gate. The gate could not fail on a test
# failure at all; every red it had ever produced was a cost red.
#
# Carrying the flags on SHELL works on BOTH 3.81 and 3.82+, so there is ONE
# regime instead of two, and it fixes all recipe lines at once rather than
# leaving each future pipeline to remember `set -o pipefail` for itself.
# .SHELLFLAGS is kept so a 3.82+ make is still configured the way it documents;
# the duplicated flags are idempotent in bash. `shell-guard` below PROVES the
# regime is active rather than trusting it.
SHELL := /usr/bin/env bash -o pipefail -e -u
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO              ?= go
GOFLAGS         ?=
PACKAGES        := ./...
BENCH_PATTERN   ?= .
BENCH_COUNT     ?= 5
RACE_FLAGS      := -race
COVER_PROFILE   := coverage.out

# Pin the IANA time-zone database for deterministic openCypher TCK conformance
# across hosts. Go's time package reads ZONEINFO before the system database;
# without this, a few temporal scenarios depend on whether the host ships a
# slim or fat tzdata build. See cypher/tck/testdata/README.md.
export ZONEINFO := $(CURDIR)/cypher/tck/testdata/zoneinfo-slim.zip

GOLANGCI_LINT_VERSION ?= v2.13.1

.PHONY: help
help: ## Show this help
	@awk 'BEGIN { FS = ":.*##"; printf "Available targets:\n" } /^[a-zA-Z_-]+:.*##/ { printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# shell-guard turns the assumption above into an assertion. It is a prerequisite
# of `ci` and of `test-short`, so the gate cannot run in a regime where a test
# failure would be invisible. It probes the ACTUAL recipe shell -- `$$-` carries
# the current option letters and `[[ -o pipefail ]]` reads the option directly --
# rather than comparing version strings, because the thing that matters is
# whether the flags are in force, not which make is installed.
.PHONY: shell-guard
shell-guard: ## Assert the recipe shell really has -e, -u and -o pipefail (rmp #2672)
	@missing=""; \
	 case "$$-" in *e*) ;; *) missing="$$missing -e";; esac; \
	 case "$$-" in *u*) ;; *) missing="$$missing -u";; esac; \
	 if [[ -o pipefail ]]; then :; else missing="$$missing -o=pipefail"; fi; \
	 if [ -n "$$missing" ]; then \
	   echo "shell-guard: FATAL - the recipe shell is missing:$$missing"; \
	   echo "  running make: $(MAKE_VERSION)"; \
	   echo "  .SHELLFLAGS requires GNU Make 3.82+; this Makefile therefore also"; \
	   echo "  carries the flags on SHELL itself so that 3.81 is covered too."; \
	   echo "  If you are seeing this, SHELL was changed or overridden. Restore it:"; \
	   echo "  without -o pipefail, a FAILING 'go test' inside test-short's pipeline"; \
	   echo "  reports exit 0 and the whole gate silently stops detecting failures"; \
	   echo "  (rmp #2672)."; \
	   exit 1; \
	 fi; \
	 echo "shell-guard: OK (-e -u -o pipefail active; make $(MAKE_VERSION))"

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format all Go sources
	$(GO) fmt $(PACKAGES)
# `&& goimports -w . || echo ...` used to report "not installed" when goimports
# was installed and FAILED, and exit 0 either way. The if/else distinguishes the
# two: a missing tool is skipped, a failing tool fails the target.
	@if command -v goimports >/dev/null 2>&1; then \
	   goimports -w .; \
	 else \
	   echo "goimports not installed; skipping (install: go install golang.org/x/tools/cmd/goimports@latest)"; \
	 fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PACKAGES)

.PHONY: build
build: ## Build all packages
	$(GO) build $(GOFLAGS) $(PACKAGES)

.PHONY: test
test: ## Run unit tests
	$(GO) test $(GOFLAGS) $(PACKAGES)

# `race` is an ad-hoc developer convenience, not a gate — it is absent from `ci`'s
# dependency list (tidy fmt vet build test-short lint cover-gate), so this flag
# cannot alter any gate outcome. It carries SHORT_TIMEOUT because it runs the same
# corpus under the same detector as test-short and so has the same exposure to
# Go's 10-minute default (rmp #2584).
.PHONY: race
race: ## Run unit tests with the race detector (SHORT_TIMEOUT overridable)
	$(GO) test $(GOFLAGS) $(RACE_FLAGS) -timeout=$(SHORT_TIMEOUT) $(PACKAGES)

# ── Three-layer test targets ──────────────────────────────────────
# Each target is a strict superset of the one above it.
# See docs/test-layers.md for the full specification.

# SHORT_TIMEOUT — the short layer needs an EXPLICIT per-package timeout
# (rmp #2584).
#
# Without one, Go applies its default of 10 minutes per package, and two
# packages now run close enough to that ceiling that ordinary machine load
# turns a green gate red. Measured on the reference host (Apple M4, 10 cores,
# darwin/arm64, go1.26.6) on 2026-08-20:
#
#   * commit 147e28e4, in an isolated git worktree so contention could not be
#     the explanation, `go test -race -count=1 ./...`: the whole suite passes
#     (exit 0), but `ok internal/sim 545.794s` — only ~9% headroom under the
#     600 s default — and `ok cypher 433.236s`.
#   * with the rmp #2488 work in the tree, two consecutive `make ci` runs gave
#     `FAIL internal/sim 600.705s` and `FAIL internal/sim 601.675s`, both
#     `panic: test timed out after 10m0s`. Neither was hung: the second was
#     3 s into TestTypeCoverage_NonVacuous when the alarm fired. This is
#     cumulative budget exhaustion, not a deadlock.
#   * run-to-run variance is large enough to swamp the signal on its own:
#     cypher, which #2488 does not touch, measured 433.2 s / 589.5 s / 576.8 s
#     across three runs (+33%). internal/sim takes 487.6 s in isolation against
#     545.8 s inside `go test ./...`, because that command runs packages in
#     parallel and they contend with each other.
#
# It is therefore a HEADROOM problem, not an attribution problem. The value is
# chosen so the timeout is NOT the binding constraint — a backstop against a
# genuinely hung package, never a budget the suite is expected to approach:
# 30m is 3.05x the slowest package measured that actually completed (cypher,
# 589.5 s) and 1.5x the `-timeout=20m` the coverage pass of this same `make ci`
# gate already applies (scripts/cover_gate.sh), so a machine under sustained
# competing load reaches the same verdict as an idle one.
#
# This is not a relaxation of the per-package COST budget. That budget is a
# separate concern with its own instrument — `make test-short-timings`, 60 s
# soft / 240 s hard, plus the documented internal/sim override in
# docs/test-layers.md — and is deliberately untouched here.
#
# Overridable, so a slower or faster host needs no edit here.
# See docs/test-layers.md for the same measurements.
SHORT_TIMEOUT ?= 30m

# Per-package short-layer COST budget, enforced on the routine gate by
# scripts/pkg_time_budget.sh, which `test-short` pipes its output through
# (rmp #2577, #2599). Before this it was enforced by nothing: HARD_BUDGET
# defaulted to 0 (disabled), and the only target that ran the script at all was
# absent from `ci`. A ceiling nothing reads is decoration.
#
# SOFT_BUDGET warns; HARD_BUDGET fails the gate. The global ceiling stays at the
# documented 240 s: it is NOT relaxed to accommodate the three packages above it.
# Those three get a NAMED, measured override instead, so the accommodation is
# visible per package and cannot silently cover a fourth.
SOFT_BUDGET ?= 60
HARD_BUDGET ?= 240

# Overrides are "path-suffix=seconds", derived by one stated rule rather than a
# number fitted per package: the WORST in-suite figure ever recorded for that
# package in docs/test-layers.md, times 1.25, rounded up to the whole minute.
#
#   internal/sim    602.9s x 1.25 = 753.6 -> 780
#   cypher          321.7s x 1.25 = 402.1 -> 420
#   bench/audit352  328.7s x 1.25 = 410.9 -> 420
#
# Worst-observed, not last-measured: internal/sim has been recorded in-suite at
# 545.8s, 557.4s, 564.0s and 602.9s on this hardware — a 10.5% spread — so a
# ceiling fitted to the run that happened to be measured would false-red on a
# busier day. 780s leaves 29% headroom over the worst of them while still
# tripping on a genuine 25% cost regression, and stays far clear of
# SHORT_TIMEOUT (30m).
#
# The cypher figure was re-derived when its second observation arrived, exactly as
# the single-observation caveat required: two in-suite runs the same day, both
# with load recorded, gave 276.4s and 321.7s — a 16% swing, against internal/sim
# 0.3% across the same pair. Mid-sized packages vary far more run to run than the
# big one does, because their co-tenancy changes with scheduling order.
#
# bench/audit352 is NOT a regression: it is a ceiling that had never been
# exercised. The package carried no entry here, so its cost was only ever inferred
# from a STANDALONE figure (180.6s), which docs/test-layers.md marks as a lower
# bound precisely because it carries none of the co-tenancy the parallel suite
# adds. The first three in-suite measurements ever taken — 2026-08-29, all three
# with load recorded in docs/test-layers.md — gave 321.5s, 328.7s and 318.1s, a
# 3.3% spread and 1.76x the standalone lower bound. The rule then reads the worst
# of them, 328.7s, exactly as it reads the worst for the two packages above.
PKG_HARD_BUDGET_OVERRIDES ?= /internal/sim=780 /cypher=420 /bench/audit352=420
export SOFT_BUDGET HARD_BUDGET PKG_HARD_BUDGET_OVERRIDES

# GOGRAPH_PARALLEL_SUITE declares to the test binaries that packages are being
# tested IN PARALLEL, so any wall-clock, throughput, or CPU-time assertion is
# measuring the machine's load rather than the code (rmp #2517).
# testlayers.RequireQuietMachine reads it and skips those assertions LOUDLY, each
# printing the quantity it would have measured. Nothing stops being gated: the
# same assertions run and ASSERT in `make test-timing`, which `ci` invokes.
# The variable is exported by presence, not value, so an empty expansion cannot
# silently re-enable a gate under load.
.PHONY: test-short
# The pipe carries the per-package cost budget. The script echoes the go test
# output through VERBATIM, so what the developer sees is unchanged, and it reads
# the plain "ok<TAB>pkg<TAB>0.330s" summary lines rather than -json, because
# -json implies -v and would bury the run in per-test noise for the identical
# numbers. pipefail keeps a test failure failing, so the budget check cannot mask
# it -- but ONLY because the flags are carried on SHELL itself (see the header).
# This comment previously credited `.SHELLFLAGS`, which needs GNU Make 3.82+; on
# the 3.81 macOS ships it was inert, this pipeline reported the budget script's
# status alone, and a real `FAIL` read as exit 0 (rmp #2672). `shell-guard` is a
# prerequisite of this target so that regime cannot return unnoticed.
test-short: shell-guard ## [layer: short]   local default — race detector, no build tags, per-package cost budget (SHORT_TIMEOUT/SOFT_BUDGET/HARD_BUDGET overridable)
	GOGRAPH_PARALLEL_SUITE=1 $(GO) test $(RACE_FLAGS) -count=1 -timeout=$(SHORT_TIMEOUT) $(PACKAGES) | bash scripts/pkg_time_budget.sh

# TIMING_PKGS are the packages holding short-layer assertions whose subject is a
# duration, a throughput, or a ratio of them. The full inventory, with the
# measurement that motivated each, is docs/short-layer-wallclock-audit.md.
#
# The list is explicit rather than `./...` on purpose: a serial run of the whole
# repository would cost far more than the ~101 s these packages need, and the
# point of the phase is to give the timing gates a quiet machine, not to re-run
# the suite.
# It lists ONLY the packages whose gates are actually guarded today. The audit
# found 39 instances across 12 packages; the three filed as #2499, #2506 and #2517
# are guarded here, and each remaining instance extends this list and TIMING_RUN
# as its own task lands (#2568, #2569, #2572, #2573, #2574, #2588 are filed).
# An instance found AFTER that population audit extends the list the same way:
# bench/audit352's TestLabelCountPushdownIsConstantTime (#2673) is here because its
# ns/op ratio failed twice in one day in OPPOSITE directions -- 1.632 with the small
# graph slower under `test-short -race`, 1.629 the right way round under
# `cover-gate` -- while its allocation arms read flat and identical at all five
# sizes in both runs. Only the wall-clock half is guarded; the allocation half still
# asserts in the short layer.
# Listing a package before its gates are guarded only buys `[no tests to run]` and
# the build time to discover it.
TIMING_PKGS = \
	./bench/audit352 \
	./bench/cyclicjoin \
	./bench/mvccwrite \
	./bolt/server

# TIMING_RUN selects only the guarded gates. Running the whole package serially
# would reintroduce exactly the co-tenancy the phase exists to remove — the
# gate's neighbours are as capable of loading the machine as another package is.
TIMING_RUN ?= TestE2E_ConcurrentAutocommitReadsRunInParallel|TestCyclicJoin_FittedExponents|TestLabelCountPushdownIsConstantTime|TestWriteScalingGate|TestWALWriteScalingGate|TestWriteConcurrencyGate|TestWriteScalingInstrument_SeesConcurrency|TestWriteScalingInstrument_SeesSerialisation

TIMING_TIMEOUT ?= 20m

# test-timing deliberately does NOT set GOGRAPH_PARALLEL_SUITE, and runs with
# -p 1 so the packages do not compete with each other. This is the phase in which
# every guarded wall-clock assertion actually asserts.
.PHONY: test-timing
test-timing: ## [layer: short] Serially re-run the wall-clock/throughput gates on a quiet machine, where their measurement is valid (rmp #2517)
	$(GO) test $(RACE_FLAGS) -count=1 -p 1 -timeout=$(TIMING_TIMEOUT) -run '$(TIMING_RUN)' $(TIMING_PKGS)

# test-short-timings DELEGATES to test-short rather than restating its command.
# It used to carry its own copy, and the two drifted: the copy omitted
# GOGRAPH_PARALLEL_SUITE, so it was the one target that ran the whole parallel
# suite with the quiet-machine gates ASSERTING — precisely the contention rmp
# #2517 removed everywhere else. Delegation makes that class of drift impossible;
# the budget now lives on test-short itself, so there is nothing left to restate.
# The knobs still work: SOFT_BUDGET/HARD_BUDGET/PKG_HARD_BUDGET_OVERRIDES are
# exported above, so `SOFT_BUDGET=30 make test-short-timings` behaves as before.
.PHONY: test-short-timings
test-short-timings: ## [layer: short] Alias for test-short, kept as the named entry point for ad-hoc budget exploration (SOFT_BUDGET/HARD_BUDGET/SHORT_TIMEOUT overridable)
	$(MAKE) test-short

# ── The uninstrumented phase ──────────────────────────────────────
# UNINSTR_PKGS are the packages holding short-layer assertions whose SUBJECT is
# the Go runtime's own allocation behaviour, and which therefore cannot run
# under either instrumentation the rest of `ci` applies.
#
# bolt/packstream is here because rmp #2709 found that its
# TestDecoder_ChargeUpperBoundsGoAllocation — the self-guarding half of security
# finding #1849, the proof that the decoded-memory charge UPPER-BOUNDS real Go
# allocation — ran in NO phase of `make ci` at all:
#
#   test-short   -race                → the file is //go:build !race: compiled out
#   test-timing  -race                → same, and the package is not in TIMING_PKGS
#   cover-gate   -covermode=atomic    → compiled in, then skipped by the test's own
#                                       testing.CoverMode() guard
#
# Each of those three guards is individually correct, and each is documented at
# its site. Their INTERSECTION was the defect: two locally-sound decisions that
# between them left a security invariant asserting nowhere. Neither guard can be
# relaxed — the race detector disables the tiny allocator and adds shadow memory,
# the coverage counters allocate on their own account, and the charge bounds
# PRODUCTION memory — so the only correct fix is a phase that applies neither.
#
# The list holds PACKAGES, not -run patterns, so a future allocation assertion
# added to one of them is picked up without editing this file. Adding a package
# here costs a full uninstrumented run of it: bolt/packstream measures 0.39-0.53 s
# (Apple M4, darwin/arm64, go1.26.6, 2026-09-03, host at loadavg 6.45), which is
# why the whole package runs rather than a single -run filter.
#
# -p 1 serialises the package test binaries. runtime.MemStats is per-PROCESS, so
# a second package cannot pollute the subject's counters directly; what -p 1
# removes is CPU and memory CONTENTION between concurrently running binaries
# while one of them is measuring. With a single package in the list today it is
# a no-op that costs nothing and stops the list growing into a measurement
# hazard.
UNINSTR_PKGS = ./bolt/packstream

UNINSTR_TIMEOUT ?= 5m

.PHONY: test-uninstrumented
test-uninstrumented: ## [layer: short] Run the allocation-measuring packages with NEITHER the race detector NOR coverage instrumentation — the only phase in which they assert (rmp #2709)
	$(GO) test -count=1 -p 1 -timeout=$(UNINSTR_TIMEOUT) $(UNINSTR_PKGS)

# SOAK_TIMEOUT / NIGHTLY_TIMEOUT — the deferred layers need an EXPLICIT
# per-package timeout (rmp #2259).
#
# Without one, Go applies its default of 10 minutes per package, which the soak
# layer cannot satisfy: measured on an Apple M4 under -race, graph/io/csv alone
# takes 800.8 s (13.3 min) and passes, so that package could never finish inside
# the default however quiet the machine. Three further packages — cypher,
# internal/shapegen and internal/sim — did not complete even at 45 min under
# -race. They are SLOW, not hung: cypher/TestDetachDelete_Hub1M_Soak builds
# 1 000 000 nodes and 1 000 000 edges and completes in 724.2 s with the race
# detector OFF, against >44 min with it on.
#
# The values below are therefore chosen so the timeout is NOT the binding
# constraint, rather than to encode a known total runtime — the three long
# packages have not been measured to completion under -race. Both are
# overridable, so a slower or faster host needs no edit here.
# See docs/test-layers.md for the measurements.
SOAK_TIMEOUT    ?= 4h
NIGHTLY_TIMEOUT ?= 12h
# The CI-safe nightly subset still excludes only ./search/extern/..., so it
# retains the three long packages above and needs the same headroom; its former
# 50m was below what the evidence requires.
NIGHTLY_CI_TIMEOUT ?= 4h

.PHONY: test-soak
test-soak: ## [layer: soak]    short + soak — race detector, -tags=soak (SOAK_TIMEOUT overridable)
	$(GO) test $(RACE_FLAGS) -count=1 -timeout=$(SOAK_TIMEOUT) -tags=soak $(PACKAGES)

.PHONY: test-nightly
test-nightly: ## [layer: nightly] short + soak + nightly — race detector, -tags=soak,nightly,soakfull (full; includes the multi-hour endurance tests; NIGHTLY_TIMEOUT overridable)
	$(GO) test $(RACE_FLAGS) -count=1 -timeout=$(NIGHTLY_TIMEOUT) -tags=soak,nightly,soakfull $(PACKAGES)

# test-nightly-ci: CI-safe nightly subset for scheduled GitHub Actions runs.
#
# Excluded: ./search/extern/... contains large-scale graph tests (RMAT
# scale-20 ~1 M vertices / ~8-16 M edges, DIMACS9 full 24 M vertices /
# 60 M edges) that exceed the 7 GB RAM budget of GitHub free runners under
# the race detector. All other nightly-layer packages — including the
# shapegen SNAP test (which self-skips when offline) — are included.
#
# Also excluded — by build tag rather than by package, because the package holds
# many fast and valuable nightly tests that must keep running on CI: the two
# multi-hour endurance scenarios in internal/sim/phase4_long_running_soak_test.go
# (2,000,000 / 1,000,000 ticks). They are gated behind the `soakfull` tag, which
# test-nightly passes but this CI subset does not, so they compile out here.
# Under the race detector they alone exceed the 600 s go-test default timeout on
# a fast workstation. Their scenario run-path is still covered by the short layer
# (part of `make ci`: TestCatalogue_SmokeSubsetRunsClean) and at a small budget by
# the soak-layer TestCatalogue_EachScenarioRunsClean; the endurance budget is
# a periodic stability watch, not a release gate (see the soak-gate policy in the
# project instructions).
#
# The "-ci" name is now historical (rmp #2259). All correctness gating is LOCAL:
# .github/workflows/ contains only release.yml, nothing invokes this target, and
# the manual-heavy.yml workflow this comment used to point at no longer exists.
# The former 50m timeout was sized for a GitHub job budget that no longer applies
# — and was in any case below what the layer needs, since cypher alone did not
# complete in 45 min under -race. NIGHTLY_CI_TIMEOUT now matches SOAK_TIMEOUT.
#
# Use test-nightly (no -ci suffix) for a complete local run, or on machines with
# ≥ 16 GB RAM; this subset remains useful for skipping the two memory-hungry
# excluded packages.
NIGHTLY_CI_PKGS := $(filter-out \
	github.com/FlavioCFOliveira/GoGraph/search/extern, \
	$(shell $(GO) list -tags=soak,nightly ./...))

.PHONY: test-nightly-ci
test-nightly-ci: ## [layer: nightly-ci] CI-safe nightly subset — excludes search/extern (pkg, >7 GB under race) and the soakfull multi-hour sim endurance tests (tag); NIGHTLY_CI_TIMEOUT overridable
	$(GO) test $(RACE_FLAGS) -count=1 -timeout=$(NIGHTLY_CI_TIMEOUT) -tags=soak,nightly $(NIGHTLY_CI_PKGS)

.PHONY: test-crashinject
test-crashinject: ## Run crash-injection battery (requires gograph_crashinject build tag; may need elevated process limits)
	$(GO) test -tags=gograph_crashinject -count=1 -timeout=10m \
		./internal/crashinject/... \
		./internal/crashpoint/... \
		./store/recovery/...

.PHONY: check-soak-build
check-soak-build: ## Verify soak- AND nightly-tagged files compile and vet clean (pre-push guard for the deferred test layers)
	# The short layer (go test ./..., no tags) never compiles soak/nightly
	# files, so a compile break in those ACID-gating tests would otherwise
	# surface only when the soak or nightly layer is next run, long after the
	# offending change landed.
	# -tags=soak covers `soak` and `soak || nightly` files; -tags=soak,nightly
	# additionally covers `nightly`-only files. Running both also catches any
	# `soak && !nightly` file.
	$(GO) build -tags=soak $(PACKAGES)
	$(GO) vet -tags=soak $(PACKAGES)
	$(GO) build -tags=soak,nightly $(PACKAGES)
	$(GO) vet -tags=soak,nightly $(PACKAGES)

.PHONY: cover
cover: ## Run tests with coverage
	$(GO) test $(GOFLAGS) -coverprofile=$(COVER_PROFILE) -covermode=atomic $(PACKAGES)
	$(GO) tool cover -func=$(COVER_PROFILE) | tail -1

.PHONY: cover-gate
cover-gate: ## Enforce aggregate (>=85%) and per-package (>=75%) coverage gates
	GOGRAPH_PARALLEL_SUITE=1 GO=$(GO) MIN_TOTAL=85.0 MIN_PER_PKG=75.0 bash scripts/cover_gate.sh

.PHONY: bench
bench: ## Run benchmarks ($(BENCH_PATTERN), count=$(BENCH_COUNT))
	$(GO) test -bench=$(BENCH_PATTERN) -benchmem -count=$(BENCH_COUNT) -run=^$$ $(PACKAGES)

# ── Vulnerability gate ─────────────────────────────────────────────
# Until rmp #2722 there was NO gate. `govulncheck` appeared in no Makefile
# target, no `make ci` path and no `.sh`/`.yml`/`.yaml` file in the repository —
# only as prose in CONTRIBUTING.md §4 and SECURITY.md describing a command a
# human was expected to remember to type. That is why it could stop working for
# an entire toolchain bump without anyone noticing: a gate nobody invokes cannot
# fail loudly, it simply never runs.
#
# The gate asserts that ANALYSIS HAPPENED — a non-empty set of loaded root
# packages covering every package `go list ./...` reports — and evaluates that
# assertion BEFORE it looks at the exit status, because the failure mode
# recorded against v0.12.0 was a scanner that "exits 0 while performing no
# analysis at all" (CONTRIBUTING.md §4). scripts/vulncheck_gate.sh carries the
# full reasoning; scripts/test_vulncheck_gate.sh proves the assertion can fail
# by feeding the gate deliberately broken scanners, including a real
# govulncheck built against another Go minor.
#
# It needs the network: govulncheck consults https://vuln.go.dev. An
# unreachable database FAILS the gate rather than skipping it — a scan that
# could not consult the vulnerability database is not a clean scan. Point
# VULNCHECK_DB at a mirror on an air-gapped host.
GOVULNCHECK_VERSION ?= v1.7.0

.PHONY: vulncheck
vulncheck: ## Vulnerability gate: govulncheck over the module, asserting analysis really happened rather than trusting the exit code (rmp #2722)
	GO=$(GO) GOVULNCHECK_VERSION=$(GOVULNCHECK_VERSION) bash scripts/vulncheck_gate.sh

.PHONY: test-vulncheck-gate
test-vulncheck-gate: ## Prove `vulncheck` can FAIL: feed it deliberately broken scanners and require it to reject each one (rmp #2722)
	GO=$(GO) bash scripts/test_vulncheck_gate.sh

.PHONY: lint
lint: ## Run golangci-lint (auto-install if missing)
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found; installing $(GOLANGCI_LINT_VERSION) to $$($(GO) env GOPATH)/bin"; \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi
	golangci-lint run $(PACKAGES)

.PHONY: ci
ci: shell-guard tidy fmt vet build vulncheck test-short test-timing test-uninstrumented lint cover-gate ## Full CI pipeline: tidy + fmt + vet + build + vulncheck + test-short + test-timing + test-uninstrumented + lint + cover-gate

.PHONY: ci-soak
ci-soak: shell-guard tidy fmt vet build vulncheck test-soak test-timing test-uninstrumented lint cover-gate ## CI pipeline with soak layer: like ci but runs test-soak

.PHONY: ci-nightly
ci-nightly: shell-guard tidy fmt vet build vulncheck test-nightly test-timing test-uninstrumented lint cover-gate ## CI pipeline with nightly layer: like ci but runs test-nightly

.PHONY: smoke
smoke: ## Quick PR pre-flight: tidy + fmt + vet + build + short unit tests (no race, no lint, no cover-gate)
	$(MAKE) tidy
	$(MAKE) fmt
	$(MAKE) vet
	$(MAKE) build
	$(GO) test -count=1 -short -timeout 60s $(PACKAGES)

.PHONY: soak
soak: ## Run the 4-hour mixed-workload soak harness (use SOAK_FLAGS to override)
	GODEBUG=gctrace=1 $(GO) run ./bench/soak $(SOAK_FLAGS)

.PHONY: soak-smoke
soak-smoke: ## 60-second smoke run of the soak harness — exercises the harness without the full 4h
	$(GO) run ./bench/soak -duration=60s -sample-interval=15s

# Default Python interpreter for the cross-library comparison harness.
# Override with PYTHON=/path/to/venv/bin/python3 to point at a venv that
# has python-graphblas and graphblas-algorithms installed.
PYTHON ?= python3

.PHONY: comparison-graphblas
comparison-graphblas: ## Run the SuiteSparse:GraphBLAS comparison baseline (via python-graphblas)
	@command -v $(PYTHON) >/dev/null 2>&1 || { echo "$(PYTHON) not on PATH; set PYTHON=..."; exit 1; }
	@$(PYTHON) -c "import graphblas_algorithms" >/dev/null 2>&1 || { \
	  echo "graphblas-algorithms not installed. To install in a venv:"; \
	  echo "  python3 -m venv /tmp/graphblas_venv"; \
	  echo "  /tmp/graphblas_venv/bin/pip install --upgrade pip"; \
	  echo "  /tmp/graphblas_venv/bin/pip install 'numpy<2' scipy networkx python-graphblas graphblas-algorithms"; \
	  echo "  make comparison-graphblas PYTHON=/tmp/graphblas_venv/bin/python3"; \
	  exit 1; }
	$(PYTHON) bench/comparison/lagraph_baseline.py

.PHONY: release-check
release-check: ## Dry-run goreleaser against the local checkout (snapshot mode, no publish)
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not installed; install: brew install goreleaser or see https://goreleaser.com/install/"; exit 1; }
	goreleaser release --snapshot --clean --skip=publish

.PHONY: release-accuracy
release-accuracy: ## Release-accuracy checks only (Phase A): CHANGELOG/release-notes/README/SECURITY/benchmark-doc consistency for VERSION. This is the only gate the release.yml CI job runs; correctness (vet/build/-race/lint/TCK), coverage and the crash battery are enforced LOCALLY by `make release-preflight` before the tag is pushed.
	@test -n "$${VERSION:-}" || { echo "set VERSION=vX.Y.Z"; exit 1; }
	@echo "release-accuracy: VERSION=$$VERSION"
	@v_no_prefix=$$(echo "$$VERSION" | sed 's/^v//'); \
	  grep -q "## \[$$v_no_prefix\]" CHANGELOG.md \
	  || { echo "release-accuracy: CHANGELOG.md is missing a '## [$$v_no_prefix]' entry — promote the Unreleased section first"; exit 1; }
	@test -f "release-notes/$$VERSION.md" \
	  || { echo "release-accuracy: release-notes/$$VERSION.md does not exist — draft the long-form notes first"; exit 1; }
	@echo "release-accuracy: checking README 'Current release' names $$VERSION…"
	@pat="Current release: \`$$VERSION\`"; grep -qF "$$pat" README.md \
	  || { echo "release-accuracy: README.md 'Current release' does not name $$VERSION — update the Status block"; exit 1; }
	@echo "release-accuracy: checking SECURITY.md supported-versions table names $$VERSION's release line…"
	@minor_line=$$(echo "$$VERSION" | sed -E 's/^v([0-9]+)\.([0-9]+)\..*/v\1.\2.x/'); \
	  grep -qF "$$minor_line" SECURITY.md \
	  || { echo "release-accuracy: SECURITY.md supported-versions table does not mention $$minor_line — update the table"; exit 1; }
	@echo "release-accuracy: checking per-release benchmark report docs/benchmarks/$$VERSION.md exists…"
	@test -f "docs/benchmarks/$$VERSION.md" \
	  || { echo "release-accuracy: docs/benchmarks/$$VERSION.md does not exist — record the per-release benchmark/load-test numbers first"; exit 1; }
	@echo "release-accuracy: all accuracy checks passed"

.PHONY: release-preflight
release-preflight: ## Canonical LOCAL release gate (`make release` calls this) — release-accuracy + the full `make ci` correctness+coverage gate + headline bench. `make ci` runs the suite ONCE (tidy/fmt/vet/build/vulncheck/test-short[-race,./...]/lint/cover-gate; the TCK =100% baseline in TestTCKExecution runs inside the -race and coverage passes), so release-preflight SUBSUMES `make ci` — do not run both. The release.yml CI job runs only `release-accuracy`.
	@$(MAKE) release-accuracy
	@echo "release-preflight: running the full correctness + coverage gate (make ci: tidy/fmt/vet/build/vulncheck/test-short[-race]/lint/cover-gate; TCK =100% baseline enforced inside)…"
	@$(MAKE) ci
	@if [ -x scripts/run_headline_bench.sh ]; then \
	  echo "release-preflight: running headline bench regression gate (informational on a release tag — see docs/release.md for the canonical PR-time gate)…"; \
	  ./scripts/run_headline_bench.sh > /tmp/release-preflight-bench.txt || { echo "release-preflight: headline bench failed; see /tmp/release-preflight-bench.txt"; exit 1; }; \
	else \
	  echo "release-preflight: scripts/run_headline_bench.sh not present — skipping bench gate"; \
	fi
	@echo "release-preflight: all checks passed"

.PHONY: release
release: release-preflight ## Run goreleaser to publish a release for the current tag — requires VERSION and a clean tree
	@test -z "$$(git status --porcelain)" || { echo "working tree dirty"; exit 1; }
	@test -n "$${VERSION:-}" || { echo "set VERSION=vX.Y.Z"; exit 1; }
	@git rev-parse "$$VERSION" >/dev/null 2>&1 || { echo "tag $$VERSION does not exist; create it first"; exit 1; }
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser not installed"; exit 1; }
	GOVERSION=$$($(GO) version | awk '{print $$3}') goreleaser release --clean

ANTLR_VERSION ?= 4.13.1
ANTLR_JAR     ?= $(HOME)/.antlr/antlr-$(ANTLR_VERSION)-complete.jar
JAVA          ?= java
CYPHER_GRAMMAR_DIR := cypher/parser/grammar
CYPHER_GEN_DIR     := cypher/parser/gen

.PHONY: install-antlr
install-antlr: ## Download the ANTLR $(ANTLR_VERSION) jar to ~/.antlr/ (requires curl + java)
	bash scripts/install-antlr.sh $(ANTLR_VERSION)

.PHONY: generate-cypher-parser
generate-cypher-parser: ## Regenerate cypher/parser/gen/ from ANTLR grammar (requires java + ~/.antlr jar)
	@test -f "$(ANTLR_JAR)" || { \
	  echo "ANTLR jar not found at $(ANTLR_JAR)."; \
	  echo "Run 'make install-antlr' first."; \
	  exit 1; \
	}
	@command -v goimports >/dev/null 2>&1 || { \
	  echo "goimports not found (install: go install golang.org/x/tools/cmd/goimports@latest)"; \
	  exit 1; \
	}
	# 1. Generate the parser/lexer from the vendored grammar.
	$(JAVA) -jar "$(ANTLR_JAR)" \
	  -Dlanguage=Go \
	  -package gen \
	  -visitor \
	  -o "$$(pwd)/$(CYPHER_GEN_DIR)" \
	  "$$(pwd)/$(CYPHER_GRAMMAR_DIR)/CypherLexer.g4" \
	  "$$(pwd)/$(CYPHER_GRAMMAR_DIR)/CypherParser.g4"
	# 2. 'go vet' clean-up + checkout-independent header normalisation.
	python3 scripts/fix-antlr-gen.py "$(CYPHER_GEN_DIR)"
	# 3. Canonical import grouping (matches the checked-in gen).
	goimports -w "$(CYPHER_GEN_DIR)"
	# 4. Re-apply the hand-written parser patches that cannot live in the grammar
	#    (numeric-ID workarounds, chained-WITH, optional CALL parens, reduce()).
	#    See cypher/parser/grammar/README.md and docs/tck/parser-report.md.
	git apply --whitespace=nowarn "$(CYPHER_GRAMMAR_DIR)/gen-patches.patch"
	$(GO) vet ./$(CYPHER_GEN_DIR)/...

.PHONY: clean
# The second rm reclaims per-invocation coverage temporaries left by a run that
# was cancelled (rmp #2549). scripts/cover_gate.sh now reclaims its own, but a run
# killed with SIGKILL cannot, and files stranded before that fix exist in working
# trees today: one measured 248,840,121 bytes and was seven days old, and clearing
# it took this tree from 2.4 GB to 791 MB. .gitignore hides them from
# `git status`, so `make clean` is the only thing that surfaces them.
#
# The patterns name the TEMPORARIES only. cover.out.failed.*.log is deliberately
# NOT matched: it is preserved failure evidence (rmp #2347), and a re-run chasing
# a rare failure must not destroy the record of it.
clean: ## Remove build artefacts, including coverage temporaries stranded by a cancelled gate
	rm -f $(COVER_PROFILE) coverage.html cover.out cover.lib.out
	rm -f cover.out.tmp.* cover.out.testlog.tmp.* cover.lib.out.tmp.* \
	      cover.out.pub.* cover.lib.out.pub.*
	rm -rf bin build dist
	$(GO) clean -testcache
