# Contributing to GoGraph

This document captures the policies that complement the runtime
contracts already documented in `CLAUDE.md`.

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
