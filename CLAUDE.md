# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

GoGraph is a Go module for working with graphs: persistence, manipulation, and — above all — fast search. The API surface should stay small and ergonomic; performance is a first-class concern.

## Compliance Mandates

These two properties are non-negotiable invariants of the module. Every change — feature work, refactor, bug fix, performance tuning — must preserve them. A change that regresses either property is not acceptable.

### 1. 100% Cypher TCK Compliant

The module is **100% compliant with the openCypher TCK** (Technology Compatibility Kit) at the execution level, as published at <https://opencypher.org/>. Every development must guarantee that the module remains 100% compatible with the openCypher specification.

- The full openCypher TCK execution suite is fully green: every scenario in `cypher/tck/features/` passes, with no `failed`, no `undefined`, and no `pending` steps.
- The regression gate in `cypher/tck/runner_test.go` (`const tckExecutionBaseline`) is set to the full scenario count. Any pull request that lowers the passing count is rejected by CI.
- Conformance is evidence-based: do not claim openCypher behaviour from memory. When a question arises, consult the openCypher 9 specification, the relevant TCK feature file, or the upstream openCypher reference implementation before changing behaviour.
- New features that the openCypher TCK does not cover are allowed only when they do not conflict with any TCK-covered semantics.

### 2. 100% ACID Compliant

The module guarantees the **ACID** transactional properties — **Atomicity**, **Consistency**, **Isolation**, and **Durability** — across every feature, to provide **RELIABILITY** and **INTEGRITY** of stored data.

- **Atomicity** — every transaction is all-or-nothing: either every write becomes visible together or none of them do. Partial application after a crash or error is forbidden.
- **Consistency** — every committed transaction leaves the graph in a state that satisfies every declared invariant (schema constraints, uniqueness, label/property typing, referential integrity for edges, index correctness). Reads never observe a state that violates an invariant.
- **Isolation** — concurrent transactions behave as if serialised. Readers never observe the partial writes of an in-flight transaction; writers never silently overwrite each other.
- **Durability** — once a commit acknowledgement is returned, the change survives process crash, host crash, and `kill -9`. Verified by the deterministic crash-injection battery in `internal/crashinject/` and the WAL recovery tests in `store/wal/` and `store/recovery/`.

These properties must be preserved both for the in-memory engine and for every persistence backend. Any code path that could compromise an ACID property — a non-atomic multi-step write, a read that could observe partial state, a commit that does not durably flush — must be rejected at code review and must not be merged.

## Behavioural Rules

### Decision autonomy

You are **not authorised** to make decisions unilaterally. Whenever instructions are insufficient, unclear, non-specific, ambiguous, or contradictory, you **must always ask the user** how to proceed before taking any action.

When asking for clarification:
- Present multiple options labelled `a)`, `b)`, `c)`, … and explicitly state which option you recommend.
- When there are multiple open questions, ask them **one at a time**, sequentially — never bundle several questions into a single prompt.

### Documentation language and quality

All project documentation must be written in **English**, at the highest standard: no spelling, grammar, or syntactic errors. Use clear, simple, unambiguous technical language aimed at human readers.

Documentation must be **accurate and faithful to the code** — never document intent, only what is actually implemented.

### Development workflow

Every piece of work must follow this exact sequence:

```
Specify → Implement → Test → Document
```

No step may be skipped or reordered.

---

## Planning and Task Execution

### Single source of truth

Use the `rmp` CLI (available system-wide) as the **sole source of truth** for all planning and task tracking in this project. No other tool or method should be used for this purpose.

### Planning

Before writing any code, analyse the proposed scope and determine whether multiple development phases are needed. Each phase must deliver a solid, standalone deliverable.

**Phase/sprint definition (first pass):**
Define the phases (sprints in `rmp`) and the objective of each sprint before enumerating individual tasks.

**Task definition (second pass):**
For every task, document clearly:
- **Objective** — what it accomplishes.
- **Functional requirements** — observable behaviour expected.
- **Technical requirements** — constraints, interfaces, performance targets.
- **Acceptance criteria** — the concrete, verifiable conditions that must be met to close the task.

When the work spans multiple sprints, complete the full sprint list first, then populate tasks sprint by sprint.

### Execution

Task execution is the natural continuation of planning. Before starting any work, use `rmp` to:

1. Check whether any open task is already in progress and continue it.
2. Identify the next task to start.
3. Read and fully understand the task description, requirements, and acceptance criteria.

During execution:
- Verify **all** acceptance criteria are satisfied before closing a task.
- Close the task with a concise summary of what was done.
- After closing a task and before moving to the next, create a **git commit** following conventional commit conventions, describing what was done.

**Sequencing rules:**
- Sprints are always executed **sequentially**.
- Tasks within a sprint may run in **parallel** only when there is clear justification (no shared state, no dependency between them).

---

## Performance-First Engineering

### Research methodology before any implementation

Before writing a single line of code for any non-trivial component, conduct a **cross-language, cross-paradigm survey** of every known approach. This means:

1. **Survey the academic and engineering literature** — consider how the problem is solved in C, C++, Rust, Java (JVM JIT tricks), Python (CPython/PyPy), and specialised graph databases (Neo4j, DGraph, JanusGraph, TigerGraph). Extract the structural insight, not the syntax.
2. **Identify the performance ceiling** — determine what the theoretically optimal time and space complexity is for the problem, and whether any real-world implementation reaches it.
3. **Evaluate data structure alternatives** — for every hot-path structure, explicitly compare at least two candidates (e.g., adjacency matrix vs. CSR vs. adjacency list; binary heap vs. Fibonacci heap vs. pairing heap for priority queues) with measured or cited trade-offs.
4. **Translate to idiomatic Go** — implement the winning approach using Go idioms: no `interface{}` in hot paths, avoid unnecessary heap allocations, favour value semantics for small structs, use `unsafe` only when justified and documented.

### Go-specific performance mandates

- **Prefer flat, cache-friendly data structures** — a `[]Edge` slice with index arithmetic beats a `map[NodeID][]Edge` in cache-miss-sensitive traversal.
- **Avoid interface dispatch in inner loops** — use concrete types internally; expose interfaces at package boundaries only.
- **Pre-size all slices and maps** — always pass a capacity hint when the upper bound is knowable.
- **Use `sync.Pool` for ephemeral allocations** — priority queues, visited sets, and path buffers that are created per query must be pooled.
- **Benchmark before and after every structural change** — use `go test -bench=. -benchmem -count=5` and compare with `benchstat`. A change that regresses allocations/op or ns/op without a documented justification must not be merged.
- **Profile with `pprof`** — CPU and heap profiles must be checked for any algorithm operating on graphs with more than 10k nodes.

### Idiomatic Go requirements

- **Error handling** — return `(T, error)`; never panic on recoverable conditions; never swallow errors.
- **Generics** — use type parameters (`[N comparable, W constraints.Ordered]`) for node IDs and edge weights so the library is not tied to `int64`/`float64`.
- **Concurrency** — prefer channels for coordination between goroutines; use `sync.RWMutex` for shared graph state; document every exported type's concurrency contract.
- **Package naming** — single-word, lowercase, no underscores; package names must not stutter with their exported identifiers (`graph.Graph` is acceptable; `graph.GraphGraph` is not).
- **Tests** — table-driven tests with `t.Run`; property-based tests with `testing/quick` or `pgregory.net/rapid` for algorithms where invariants can be expressed generically.
- **Test layers** — every test belongs to one of three layers:
  - `short` — the default; runs on `go test ./...` with no tags. Every PR runs this layer; each package must stay under 60 s.
  - `soak` — minutes-long workloads. Activated by the `soak` build tag or by setting `SOAK_FULL=1`. The pre-existing `stress` and `soakfull` build tags are considered part of the soak family.
  - `nightly` — hours-long workloads. Activated by the `nightly` build tag or by setting `GOGRAPH_NIGHTLY=1`; implies soak.

  Prefer compile-time gating with a `//go:build soak` or `//go:build nightly` header on a dedicated file; when that is impractical, call `gograph/internal/testlayers.RequireSoak(t)` or `RequireNightly(t)` at the top of the test body. The full specification, including sample invocations and the helpers' API, lives in [`docs/test-layers.md`](docs/test-layers.md).

  The production-readiness test battery — shape generators, invariant checkers, fault-injection packages, dataset loaders, and the add-new-shape recipe — is documented in [`docs/test-battery.md`](docs/test-battery.md).

---

## Reliability and Concurrency Mandates

This module must operate **without failure under sustained high load and high concurrency**. Every component — public or internal — must satisfy the following non-negotiable contract.

### Correctness under concurrency

- **Zero data races.** `go test -race ./...` must pass on every change. No exceptions. CI must block merges if the race detector reports any access.
- **Explicit concurrency contract.** Every exported type carries a godoc clause stating whether it is safe for concurrent use, and if so under which operations. Ambiguity is a defect.
- **No hidden global state.** Package-level mutable variables are forbidden outside of carefully reviewed registries; every shared resource is passed explicitly.
- **Context-aware blocking.** Every public API that may block on I/O, a channel, a lock, or a long computation accepts a `context.Context` and honours cancellation and deadlines.

### Robustness under load

- **Bounded resources.** No unbounded queues, no unbounded goroutine spawn, no unbounded caches. Every queue, pool, semaphore, and cache declares an explicit upper bound surfaced in its constructor.
- **Backpressure, never panic.** When a downstream is saturated, callers either receive a typed error or block on a bounded channel; the library must never crash, deadlock, or silently drop work.
- **No goroutine leaks.** Every goroutine spawned by the library has a defined lifecycle and is terminated on `Close`/`Shutdown`/context cancellation. Verified via `go.uber.org/goleak` in test teardown for every package that spawns goroutines.
- **Graceful degradation.** Under memory pressure or saturation, the library degrades predictably (slower, fewer concurrent operations) rather than failing catastrophically.

### Performance under contention

- **Lock contention budget.** Hot paths must not hold a global lock. Use sharded structures (`hash(NodeID) mod N` shards), lock-free atomics, or copy-on-write snapshots. Any new global mutex requires explicit justification in code review.
- **Readers do not block writers and vice versa where avoidable.** Prefer `atomic.Pointer[Snapshot]` swap patterns for read-heavy workloads; use RW-mutexes only when a fully lock-free design is impractical.
- **Lock-free read paths on immutable structures.** Traversal of an immutable CSR snapshot must require zero synchronisation primitives in the hot loop.
- **Fair scheduling.** Long-running operations yield (`runtime.Gosched` or chunked work) to keep latency tails bounded for concurrent short queries.

### Failure handling

- **Fail-stop, never fail-silent.** Errors are returned, logged, and counted via metrics. Corrupted state triggers a clean shutdown via a sentinel error rather than continuing with silent inconsistency.
- **Defensive validation at boundaries only.** Internal code trusts its callers; external inputs are validated once at the public API boundary and never again.
- **Recoverable panics are forbidden.** The library does not `recover()` to hide bugs; panics indicate programmer error and must surface immediately. Exception: goroutines that the library owns may recover **only** to log the panic, record it as a metric, and terminate cleanly.
- **Crash safety.** Any persistent state survives `kill -9` mid-write. Verified by deterministic crash-injection tests (Sprint 3 acceptance criterion).

### Observability

- **Every long-lived goroutine is observable.** Name (via `pprof.SetGoroutineLabels`), lifecycle metrics (started, running, exited), and recent activity timestamp.
- **Every cache, pool, and bounded queue exports utilisation metrics.** Size, capacity, hit ratio, eviction count, blocked-acquire count.
- **Latency histograms on every public blocking API.** Prometheus exposition format, with consistent label names across the module.
- **Race-detector and `goleak` integration in CI.** Both run on every PR; both must be green to merge.

### Acceptance gates

- **Soak test before any release.** A multi-hour mixed-workload run under `GODEBUG=gctrace=1` showing zero growth in heap, file descriptors, and goroutine count after warm-up.
- **Concurrency stress test in CI.** A short variant of the soak workload runs on every PR with the race detector enabled.
- **Load-test report alongside benchmarks.** Each release ships latency and throughput numbers at multiple concurrency levels (1, 8, 64, 256, 1024 goroutines), recorded in `docs/benchmarks/`.

---

## Sub-Agents (Specialists)

The following sub-agents are available and **must be actively consulted** to maximise output quality. Do not implement a component in isolation when a relevant specialist can provide material input.

| Agent | When to invoke |
|---|---|
| `graph-theory-expert` | Graph model selection, algorithm correctness, complexity analysis, data structure trade-offs for graphs specifically. Consult **before** choosing any algorithm or graph representation. |
| `go-developer` | Go implementation, idiomatic patterns, module structure, Go toolchain. The primary implementation agent. |
| `rust-elite-developer` | Cross-language performance insight: zero-copy patterns, arena allocation, SIMD, lock-free structures. Translate findings to Go. |
| `rust-perf-engineer` | Hot-path profiling methodology, cache behaviour, concurrency bottleneck diagnosis. Apply findings to Go benchmarks. |
| `Plan` | Architectural decisions before any sprint begins. Use for evaluating alternative designs when the stakes are high. |

### Mandatory consultation rules

- **`graph-theory-expert` must be consulted** before finalising the representation of any graph type and before selecting any search or traversal algorithm.
- **`go-developer` must validate** all Go code for idiom conformance before a task is closed.
- Specialists may be invoked **in parallel** when their inputs are independent (e.g., consulting `graph-theory-expert` on algorithm choice while `go-developer` reviews an adjacent module).
- Findings from specialists must be summarised in the task description or in a code comment when they influence a non-obvious design decision.

---

## Common Commands

```bash
# Initialise the module (first time only)
go mod init github.com/xumiga/gograph

# Build
go build ./...

# Test all packages (short layer only — PR-CI default)
go test ./...

# Test all packages — short + soak
go test -tags=soak ./...

# Test all packages — short + soak + nightly
go test -tags=nightly ./...

# Test a single package
go test ./graph/...

# Run a specific test
go test -run TestBFS ./graph/...

# Race detector (always use for concurrent code)
go test -race ./...

# Benchmark
go test -bench=. -benchmem ./...

# Lint (requires golangci-lint)
golangci-lint run ./...

# Format
gofmt -w .
goimports -w .

# Vet
go vet ./...
```

---

## Intended Architecture

The module is organised around three concerns:

| Layer | Responsibility |
|---|---|
| `graph/` | Core types: `Graph`, `Node`, `Edge`, `Weight`. Directed and undirected variants. |
| `search/` | Traversal and path-finding algorithms: BFS, DFS, Dijkstra, A\*, Bellman-Ford. |
| `store/` | Persistence adapters (in-memory, file, optional external backends). |

### Key design rules

- **Interfaces over concrete types** — callers depend on `graph.Graph`, not on an adjacency-list struct, so backends and algorithms remain swappable.
- **Zero-allocation hot paths** — search algorithms must avoid heap allocations in their inner loops; use pre-allocated slices and `sync.Pool` where needed.
- **No global state** — every `Graph` instance is self-contained; concurrent read access must be safe without external locking.
- **Persistence is pluggable** — `store.Store` is an interface; the default implementation is in-memory. File/DB adapters live in subdirectories (`store/file/`, `store/postgres/`, etc.) and are compiled in only when imported.

### Algorithm conventions

- Each algorithm lives in its own file inside `search/` (e.g., `search/dijkstra.go`).
- Algorithms accept a `graph.Graph` interface and return a typed result struct (path, distance map, etc.) — never raw `interface{}` or `any`.
- Provide both a simple one-shot function (`ShortestPath(g, src, dst)`) and a stateful struct for repeated queries on the same graph.
