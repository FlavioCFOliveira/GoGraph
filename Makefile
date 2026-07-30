SHELL := /usr/bin/env bash
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

GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: help
help: ## Show this help
	@awk 'BEGIN { FS = ":.*##"; printf "Available targets:\n" } /^[a-zA-Z_-]+:.*##/ { printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## Format all Go sources
	$(GO) fmt $(PACKAGES)
	@command -v goimports >/dev/null 2>&1 && goimports -w . || echo "goimports not installed; skipping (install: go install golang.org/x/tools/cmd/goimports@latest)"

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PACKAGES)

.PHONY: build
build: ## Build all packages
	$(GO) build $(GOFLAGS) $(PACKAGES)

.PHONY: test
test: ## Run unit tests
	$(GO) test $(GOFLAGS) $(PACKAGES)

.PHONY: race
race: ## Run unit tests with the race detector
	$(GO) test $(GOFLAGS) $(RACE_FLAGS) $(PACKAGES)

# ── Three-layer test targets ──────────────────────────────────────
# Each target is a strict superset of the one above it.
# See docs/test-layers.md for the full specification.

.PHONY: test-short
test-short: ## [layer: short]   local default — race detector, no build tags
	$(GO) test $(RACE_FLAGS) -count=1 $(PACKAGES)

.PHONY: test-short-timings
test-short-timings: ## [layer: short] Run the short layer (race, -json) and report packages over the 60s/pkg budget (SOFT_BUDGET/HARD_BUDGET overridable)
	bash -o pipefail -c '$(GO) test $(RACE_FLAGS) -count=1 -json $(PACKAGES) | bash scripts/pkg_time_budget.sh'

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
	GO=$(GO) MIN_TOTAL=85.0 MIN_PER_PKG=75.0 bash scripts/cover_gate.sh

.PHONY: bench
bench: ## Run benchmarks ($(BENCH_PATTERN), count=$(BENCH_COUNT))
	$(GO) test -bench=$(BENCH_PATTERN) -benchmem -count=$(BENCH_COUNT) -run=^$$ $(PACKAGES)

.PHONY: lint
lint: ## Run golangci-lint (auto-install if missing)
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found; installing $(GOLANGCI_LINT_VERSION) to $$($(GO) env GOPATH)/bin"; \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi
	golangci-lint run $(PACKAGES)

.PHONY: ci
ci: tidy fmt vet build test-short lint cover-gate ## Full CI pipeline: tidy + fmt + vet + build + test-short + lint + cover-gate

.PHONY: ci-soak
ci-soak: tidy fmt vet build test-soak lint cover-gate ## CI pipeline with soak layer: like ci but runs test-soak

.PHONY: ci-nightly
ci-nightly: tidy fmt vet build test-nightly lint cover-gate ## CI pipeline with nightly layer: like ci but runs test-nightly

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
	@test -n "$$VERSION" || { echo "set VERSION=vX.Y.Z"; exit 1; }
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
release-preflight: ## Canonical LOCAL release gate (`make release` calls this) — release-accuracy + the full `make ci` correctness+coverage gate + headline bench. `make ci` runs the suite ONCE (tidy/fmt/vet/build/test-short[-race,./...]/lint/cover-gate; the TCK =100% baseline in TestTCKExecution runs inside the -race and coverage passes), so release-preflight SUBSUMES `make ci` — do not run both. The release.yml CI job runs only `release-accuracy`.
	@$(MAKE) release-accuracy
	@echo "release-preflight: running the full correctness + coverage gate (make ci: tidy/fmt/vet/build/test-short[-race]/lint/cover-gate; TCK =100% baseline enforced inside)…"
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
clean: ## Remove build artefacts
	rm -f $(COVER_PROFILE) coverage.html cover.out cover.lib.out
	rm -rf bin build dist
	$(GO) clean -testcache
