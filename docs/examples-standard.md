# Examples standard

This document defines the single, reusable standard that **every** example
under `examples/` must follow. It exists so that each example fulfils the
two objectives the project sets for examples (see the **Examples** section
of [`../CLAUDE.md`](../CLAUDE.md)):

1. **Demonstration** — show, in a realistic end-to-end application, how
   GoGraph is used in practice.
2. **Exercise and evidence** — drive GoGraph through a diverse, realistic
   scenario *and gather evidence* about its behaviour: timing/throughput,
   memory and allocation, contention/scaling, and correctness.

The guiding principle is the one stated in `CLAUDE.md`: **treat every
example as a real simulation of the graph, not a throwaway toy.** A toy
graph of a handful of hard-coded nodes can demonstrate an API, but it
cannot *exercise* the module or *collect evidence* — at that scale every
operation is instant and free, so there is nothing to observe.

`examples/26_social_scale_bench` is the **reference end state**: a realistic,
seeded, scale-parametrised social network that reports build throughput,
per-query latency, and live-heap footprint while keeping a deterministic
data shape pinned by a regression test. Treat example 26 as the worked
example and this document as the recipe for bringing every other example
up to it.

---

## The rubric — when an example "fully fulfils its purpose"

An example meets the standard when it satisfies all five points:

1. **Realistic, reproducible data.** The dataset is produced by a
   **seeded generator** that models a genuine domain, not a handful of
   hard-coded nodes. Fixing the seed fixes the data shape exactly, so the
   run is reproducible across machines.
2. **Scale knobs.** Every scale and shape dimension is a flag (and a field
   on a `config` struct), so the same binary runs a small deterministic
   default *and* scales up to a size where the module's behaviour is
   actually observable.
3. **Subject-appropriate evidence.** The example measures and reports the
   evidence that matters for *its* subject (see the taxonomy below) — not
   just a correctness check.
4. **Deterministic facts vs. volatile telemetry.** Output is split into
   two kinds of line: deterministic *facts* (counts, results, invariants —
   reproducible for a fixed seed) and volatile *telemetry* (durations,
   throughput, heap — varies per run and per machine). Telemetry lines are
   prefixed with `# ` so a test can pin the facts and ignore the telemetry.
5. **Pinned by a regression test.** A small deterministic default is pinned
   by a regression test (a Go testable example for byte-stable output, or
   an assertion-based test for the deterministic invariants of
   non-deterministic output).

These five build on the three structural pillars that follow — testable
extraction, the regression test, and the per-example README — which remain
in force.

---

## Pillar 1 — Testable extraction with a config

Every example factors its logic out of `func main()` into a single
function that takes a context, a writer, and a configuration:

```go
func run(ctx context.Context, w io.Writer, cfg config) error
```

`run` does all the work and writes every byte of its output to `w`.
`main()` becomes a thin wrapper: it builds the default `config`, binds each
field to a flag, parses, and calls `run(context.Background(), os.Stdout, cfg)`,
converting a returned error into a fatal exit.

```go
func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.nodes, "nodes", cfg.nodes, "number of nodes to generate")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (fixes the data shape)")
	flag.Parse()
	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}
```

The `config` struct centralises every scale/shape knob; `defaultConfig()`
returns the small, deterministic default the regression test pins; and a
`validate()` method rejects impossible configurations once, at the
boundary, before any work.

The rule is absolute: **inside `run`, nothing writes to `os.Stdout` or
`os.Stderr` directly, and nothing calls `fmt.Println` / `fmt.Printf`
without a writer.** Use `fmt.Fprintf(w, …)` / `fmt.Fprintln(w, …)`
exclusively, so the output is capturable by a `bytes.Buffer` in a test.
`run` **returns** errors (`return fmt.Errorf("…: %w", err)`); only `main()`
may terminate the process.

A trivial example may have an empty-ish `config` (just a seed), but the
`run(ctx, w, cfg)` shape stays uniform across the set.

### Context and cancellation

`run` accepts a `context.Context` and honours cancellation. A generator or
query loop that can run for more than a moment polls `ctx.Err()` on a
coarse interval (see example 26's `checkEvery`), so a cancelled large run
stops promptly without making the check measurable.

---

## Pillar 2 — Realistic seeded data and scale

### Seeded generator

Replace hard-coded toy data with a generator driven by `math/rand` seeded
from `cfg.seed`. The generator models a genuine domain shape for the
subject — for example:

- a coordinate road network for routing examples (so A*'s heuristic is
  meaningful);
- a scale-free / authority-hub web for PageRank and centrality;
- planted-partition communities for community detection;
- a layered DAG for build-dependency / topological-sort examples;
- a bipartite instance with a realistic cost structure for assignment.

Where the realistic topology is not obvious, consult the
`graph-theory-expert` sub-agent before fixing the generator.

The seed makes the data reproducible: the same `-seed` yields byte-identical
deterministic facts. Use a seeded `math/rand` (annotated `//nolint:gosec`
G404 — a deterministic dataset is the point; `crypto/rand` would defeat it).

### Scale knobs and the deterministic default

Every dimension is a flag bound to a `config` field. `defaultConfig()`
returns a **small** default whose deterministic facts the regression test
pins; the same binary scales up with flags:

```sh
go run ./examples/NN_name                       # small deterministic default
go run ./examples/NN_name -nodes 1000000 -seed 7 # observable-scale run
```

Document the default and a representative large invocation in the leading
doc comment and the README. The default must stay fast (well under the
60 s per-package short-test budget) and deterministic; the large run is
where evidence becomes interesting.

---

## Pillar 3 — Evidence collection

Each example measures and reports the evidence appropriate to its subject.
Pick from this taxonomy the dimensions that matter for the example:

| Subject | Evidence to collect |
|---|---|
| Search / traversal / path-finding (`search/*`) | wall-clock latency per query; nodes settled/expanded (e.g. A* vs Dijkstra); many-query throughput and a p50/p95/p99 latency distribution at scale |
| Centrality / community (`centrality`, `community`) | per-algorithm wall-clock; convergence iterations; transient allocations (`runtime.MemStats.Mallocs` delta) and live heap |
| Graph structures (`adjlist`, `csr`, `lpg`) | live heap (`HeapAlloc`) and **bytes per node/edge**; build throughput (nodes/s, edges/s) |
| Persistence / recovery (`store/*`, `recovery`) | commit throughput; on-disk snapshot/WAL **bytes**; recovery wall-clock; live heap before vs after recovery |
| Out-of-core (`csrfile`, mmap) | on-disk size vs live heap (the out-of-core footprint advantage); mmap and query wall-clock |
| Interchange (`io/csv`, `io/graphml`, `io/jsonl`, `io/dot`) | parse and serialise throughput (rows/s, MiB/s); bytes in/out; round-trip invariant |
| Cypher / Bolt (`cypher`, `bolt`) | per-query latency; query throughput across sessions; live heap |
| Concurrency (`20_concurrent_reads`, servers) | aggregate throughput; scaling across worker counts / `GOMAXPROCS`; evidence of the lock-free read contract (no synchronisation on the snapshot) |

Helpers worth copying from example 26: `readMem()` (forces a GC, then
`runtime.ReadMemStats`, so `HeapAlloc` reflects live bytes), `humanBytes`,
`rate`, and `safeDiv`.

### The `# ` telemetry convention

Print **deterministic facts as bare lines** and **volatile telemetry as
`# `-prefixed lines**:

```
nodes.users=20000            # fact — pinned by the test
edges.friend=3500000         # fact — pinned by the test
# build.elapsed=1.2s         # telemetry — varies, never pinned
# mem.heap_alloc=512.00 MiB  # telemetry — varies, never pinned
# q.count_users.latency=4ms  # telemetry — varies, never pinned
```

A regression test asserts the bare-line facts and ignores every `# ` line.
This is the convention example 26 established; keep the `key=value` shape so
the facts are easy to assert and the telemetry is easy to grep.

---

## Pillar 4 — Regression test standard

Each example carries exactly one regression test that pins its
deterministic default. Which form depends on whether stdout is byte-stable
*once the telemetry lines are removed*.

### Deterministic facts only → Go testable example

When, at the default config, the **fact** lines are byte-for-byte stable
(the usual case once `# ` telemetry is excluded), add an `example_test.go`
in `package main` with a Go **testable example** whose `// Output:` block
contains only the fact lines. Because a testable example compares *all*
captured stdout, the example must **not print telemetry at the default
config used by the test** — either gate telemetry behind a config field the
test leaves off, or use the assertion form below.

### Mixed or non-deterministic stdout → assertion-based `Test*`

When the example prints telemetry inline with facts, or its output varies
with timing/network/scheduling, write a normal test that drives `run` into a
`bytes.Buffer` and asserts only the deterministic invariants — counts, the
presence of expected fact lines, recovered data, conservation laws (e.g.
`friend_since_filled == edges.friend`) — never the `# ` telemetry. This is
the form example 26 uses, and it is the recommended default for the scaled
examples because most of them report telemetry inline.

```go
func TestRun(t *testing.T) {
	var buf bytes.Buffer
	cfg := defaultConfig() // small, deterministic
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	mustContain(t, out, "nodes.users=20000")
	mustContain(t, out, "q.count_users=20000")
	// Never assert on "# " telemetry lines.
}
```

Optionally add a `func Benchmark*` that runs `run` (or the hot inner
operation) at a chosen scale, so `go test -bench` produces the evidence
mechanically alongside the human-readable report.

### Temp-directory caveat (persistence examples)

Examples that write to an `os.MkdirTemp` directory must keep the temp path
**out of any pinned output** (it differs every run). Print only
deterministic content, or use the assertion form and assert on the stable
parts.

### goleak — examples that spawn goroutines or a server

Examples that start goroutines or a server must double as a goroutine-leak
check. Add a `TestMain` wrapping `goleak.VerifyTestMain(m)` and run the
package under `-race`:

```go
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
```

```sh
go test -race ./examples/<NN_name>/...
```

The examples that need it include the background-checkpointer, concurrent-read,
and Bolt-server examples (17, 20, 23) and the flagship apps (24, 25).

---

## Pillar 5 — Per-example README

Every example gets a `README.md` from the uniform template below. Keep
every heading present and in order so the set reads consistently.

```markdown
# Example NN — <Title>

## What it demonstrates

<One or two sentences: which GoGraph capability does this example show?>

## Domain / scenario

<The realistic domain modelled and how the seeded generator shapes it.>

## How to run

\`\`\`sh
go run ./examples/<NN_name>                 # small deterministic default
go run ./examples/<NN_name> -nodes 1000000  # observable-scale run
\`\`\`

## Scale and flags

<Each flag, its meaning, the default, and a representative large value.>

## Expected output

\`\`\`
<The exact deterministic FACT lines at the default config. Show a
representative "# " telemetry line too and note that telemetry varies
per run and per machine.>
\`\`\`

## Evidence it collects

<Which evidence dimensions (from the taxonomy) this example reports, and
what a reader should look at when they scale it up.>

## Key APIs

- `pkg/path.Symbol` — <what it is used for here>

## Further reading

- [`pkg/path`](../../pkg/path) — package documentation
- [Example MM — <Title>](../MM_name) — related example
- [docs/<topic>.md](../../docs/<topic>.md) — relevant design note
```

The **Expected output** block, the leading doc comment, and the test's
`// Output:` (or asserted lines) must all describe the same facts.

---

## How to bring an example to the standard

A checklist, in order:

1. **Generator.** Replace hard-coded toy data with a seeded generator that
   models a realistic domain shape for the subject. Consult
   `graph-theory-expert` when the topology choice is non-obvious.
2. **Config + flags.** Introduce a `config` struct, `defaultConfig()`,
   `validate()`, and bind every dimension to a flag in `main`.
3. **Refactor.** Extract the logic into `func run(ctx, w, cfg) error`;
   route every write through `w`; return errors instead of `log.Fatal`.
4. **Evidence.** Add subject-appropriate measurement (timing/throughput,
   heap & bytes-per-element, allocations, contention) and print it as `# `
   telemetry, with the deterministic results as bare fact lines.
5. **Test.** Pin the default config's facts with a testable example or an
   assertion-based `Test*`; add `goleak`/`-race` for goroutine/server
   examples; keep temp paths out of pinned output. Optionally add a
   `Benchmark*`.
6. **README + doc comment.** Update both to describe the realistic domain,
   the flags, the expected facts, and the evidence collected — faithfully
   to the code.
7. **Verify green.** Run, in order:

   ```sh
   gofmt -l ./examples/<NN_name>          # must print nothing
   go vet ./examples/<NN_name>/...
   go test ./examples/<NN_name>/...       # add -race for goroutine/server examples
   golangci-lint run ./examples/<NN_name>/...
   ```

---

## References

- `examples/26_social_scale_bench` — the reference end state: seeded,
  scale-parametrised, reports build throughput / per-query latency / live
  heap / bytes-per-edge, with deterministic facts pinned and `# ` telemetry
  separated.
- `examples/24_social_network_cli` and `examples/25_software_house_api` —
  multi-file flagship apps with full test matrices (golden files, concurrency
  under `-race`, cross-process `kill -9` restart) and `goleak` guards.
- `bolt/server/example_test.go` — the in-repo precedent for an
  assertion-based test that drives a non-deterministic round-trip with clean
  teardown and a no-leak guarantee.
</content>
</invoke>
