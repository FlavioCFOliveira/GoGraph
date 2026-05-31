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
test-short: ## [layer: short]   PR-CI default — race detector, no build tags
	$(GO) test $(RACE_FLAGS) -count=1 $(PACKAGES)

.PHONY: test-soak
test-soak: ## [layer: soak]    short + soak — race detector, -tags=soak
	$(GO) test $(RACE_FLAGS) -count=1 -tags=soak $(PACKAGES)

.PHONY: test-nightly
test-nightly: ## [layer: nightly] short + soak + nightly — race detector, -tags=nightly
	$(GO) test $(RACE_FLAGS) -count=1 -tags=nightly $(PACKAGES)

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

.PHONY: release-preflight
release-preflight: ## Pre-flight checks that gate `make release` — CHANGELOG entry, release-notes file, lint, coverage, bench regression
	@test -n "$$VERSION" || { echo "set VERSION=vX.Y.Z"; exit 1; }
	@echo "release-preflight: VERSION=$$VERSION"
	@v_no_prefix=$$(echo "$$VERSION" | sed 's/^v//'); \
	  grep -q "## \[$$v_no_prefix\]" CHANGELOG.md \
	  || { echo "release-preflight: CHANGELOG.md is missing a '## [$$v_no_prefix]' entry — promote the Unreleased section first"; exit 1; }
	@test -f "release-notes/$$VERSION.md" \
	  || { echo "release-preflight: release-notes/$$VERSION.md does not exist — draft the long-form notes first"; exit 1; }
	@echo "release-preflight: running golangci-lint…"
	@$(MAKE) lint
	@echo "release-preflight: running coverage gate…"
	@$(MAKE) cover-gate
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
	$(JAVA) -jar "$(ANTLR_JAR)" \
	  -Dlanguage=Go \
	  -package gen \
	  -visitor \
	  -o "$$(pwd)/$(CYPHER_GEN_DIR)" \
	  "$$(pwd)/$(CYPHER_GRAMMAR_DIR)/CypherLexer.g4" \
	  "$$(pwd)/$(CYPHER_GRAMMAR_DIR)/CypherParser.g4"
	python3 scripts/fix-antlr-gen.py "$(CYPHER_GEN_DIR)/cypher_parser.go"
	$(GO) vet ./$(CYPHER_GEN_DIR)/...

.PHONY: clean
clean: ## Remove build artefacts
	rm -f $(COVER_PROFILE) coverage.html cover.out cover.lib.out
	rm -rf bin build dist
	$(GO) clean -testcache
