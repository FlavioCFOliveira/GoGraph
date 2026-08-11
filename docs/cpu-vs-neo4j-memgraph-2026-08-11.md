# CPU efficiency: GoGraph vs Neo4j and Memgraph

**Date:** 2026-08-11 · **GoGraph at:** `6626a682` (branch `sprint-339`)
**Peers under test:** `neo4j:5.26-community`, `memgraph/memgraph:3.9.0`
**Peer source read at:** Neo4j tag `5.26.9`, Memgraph tag `v3.9.0`
**Harness:** `bench/comparison/cpu_test.go` (new) · **Report sibling of**
[`concurrency-vs-neo4j-memgraph-2026-08-11.md`](concurrency-vs-neo4j-memgraph-2026-08-11.md)
and [`memory-vs-neo4j-memgraph-2026-08-11.md`](memory-vs-neo4j-memgraph-2026-08-11.md).

---

## 1. The question, and the answer

The question was why GoGraph uses the processor less efficiently than its two
reference engines. **The measurement refutes half of that premise and confirms
the other half with a cause that no amount of reading would have guessed.**

> **GoGraph has the LOWEST fixed CPU cost per query of the three engines — it
> spends 47.8 µs of processor time to serve a query that touches no data, against
> Memgraph's 71.6 µs and Neo4j's 167.7 µs.**
>
> **GoGraph has the HIGHEST marginal CPU cost per row — 1.88 µs against
> Memgraph's 0.88 µs and Neo4j's 0.96 µs — and 97 % of that per-row cost is not
> computation at all. It is one `write(2)` syscall per returned row, caused by a
> single `Flush()` on the last line of `ChunkedWriter.WriteMessage`.**

Removing that one flush — correctly scoped, with the Bolt suite green — takes a
1 000-row query from **1 953 µs to 324 µs of CPU, a 6.04× reduction**, and turns
GoGraph from **2× worse than both peers into 3× better than both**.

There is a second, independent defect: evaluating a predicate over scanned nodes
costs GoGraph **263.6 ns per node against Memgraph's 56.5 ns**, because a
row's variables live in a `map[string]Value` that is rebuilt and re-hashed per
row, where both peers address row variables by a precomputed integer slot.

---

## 2. Method

### 2.1 The metric is processor time, not throughput

Throughput conflates efficiency with parallelism: an engine that serves twice
the operations while burning four times the CPU is faster and less efficient at
once. The primary metric here is **microseconds of processor time consumed by
the engine per operation completed**.

Each engine runs as a CPU-capped container (`--cpus=4`, `-m 8g`, one docker
network) and each container's own cgroup v2 accounting file is read before and
after every measured window:

```
/sys/fs/cgroup/docker/<container-id>/cpu.stat  →  usage_usec
```

That counter is exact rather than sampled, per-cgroup (so a neighbouring
container's load cannot be charged to the engine under test), and counts every
thread the engine runs, background ones included.

### 2.2 The instrument was validated before use

An instrument that has not been shown to read true is not evidence. Against
loads of known size — busy threads for five seconds — the counter read:

| busy threads | CPU expected | CPU measured |
|---|---|---|
| 1 | 5 s | **5.08 s** |
| 2 | 10 s | **10.58 s** |
| 4 | 20 s | **19.62 s** |

So it tracks parallelism and does not saturate at one core.

### 2.3 The idle baseline is subtracted

An engine burns CPU when idle: JIT compilation, garbage collection,
checkpointing. Charging that to the query would report housekeeping as
per-operation cost, and unequally — a JVM idles far more expensively than a
static binary. Every arm therefore measures an idle window with no load offered,
immediately before the measured window, and reports

```
cpu_per_op = (cpu_delta − idle_rate × wall) / ops
```

Measured idle rates: GoGraph **0–6 µs/s**, Memgraph **476–888 µs/s**, Neo4j
**3 121–6 688 µs/s** (and **41 686 µs/s** while re-planning rotated literals —
see §5). Gross and net are both reported in the harness output; the tables below
are net.

### 2.4 The work per query is swept, so fixed and marginal cost separate

A single per-operation number cannot say *where* the time goes. Query families
are run at several result sizes K and fitted as `cpu_per_op(K) = a + b·K`, where
`a` is the cost of accepting a query at all (parse, plan, bind, transaction
setup, protocol) and `b` is the marginal cost of one more row. They are different
defects with different fixes.

### 2.5 Controls

- **The client runs inside the VM**, on the same docker network. From the host,
  colima's port-forward costs ~170 µs per round trip and pins unrelated engines
  to a common floor (concurrency report §2.1).
- **Every engine is warmed at the arm's own concurrency and query shape** before
  the window opens, plus one longer per-target prewarm. A JVM measured cold
  reports its interpreter as the engine.
- **A row-count oracle is asserted on every single operation.** An arm whose
  oracle cannot fail would report throughput for a query that silently returned
  nothing.
- **The client is a compiled test binary**, not `go test`, so the cached-replay
  hazard that corrupted the memory audit (a parameterised run replaying an
  earlier parameter's results) cannot occur.
- Fixture: 5 000 `:Person` nodes, out-degree 8 (40 000 `:KNOWS`), index on
  `sid`, byte-identical across engines and asserted by node/edge counts before
  any timing.
- Ambient load in the VM was 0.41 on 8 CPUs; the per-cgroup metric is in any
  case robust to neighbours.

---

## 3. Results — CPU per operation, concurrency 1

Median of 3 rounds, 4 s windows. Lower is better; **bold** marks the best engine.

| workload | what it isolates | GoGraph | Memgraph | Neo4j |
|---|---|---|---|---|
| `noop` — `RETURN 1` | fixed cost of accepting a query | **47.8** | 71.6 | 167.7 |
| `seek` — indexed point lookup | + one index probe, one property | **52.6** | 91.9 | 142.6 |
| `seek_literal` — rotating literal | + plan-cache behaviour | 86.7 | **80.8** | 908.0 |
| `expand` — 1 hop, 8 rows returned | + traversal + 8 rows shipped | **86.3** | 101.4 | 148.7 |
| `expand2` — 2 hops, aggregated | + 64 edges walked, 1 row | **65.3** | 82.2 | 154.5 |
| `scan_count` — 5 000 nodes | scan primitive | 255.7 | 292.2 | **131.8** ¹ |
| `scan_filter` — 5 000 nodes + predicate | + one predicate per node | 1 573.7 | 574.6 | **534.8** |
| `unwind` K=1 000 | 1 000 rows produced and shipped | 1 943.0 | **979.4** | 1 099.6 |

¹ Neo4j's `count(n)` over a label is served from its count store rather than by
scanning, so this cell is not a scan measurement. `scan_filter` cannot use it and
is the honest scan comparison.

**GoGraph wins five of eight workloads outright.** It loses two, and both losses
are per-element costs.

### 3.1 The fit: fixed vs marginal

`unwind` swept over K ∈ {1, 10, 100, 1 000}, r² ≥ 0.99 on every engine:

| engine | fixed cost `a` (µs CPU/query) ² | marginal cost `b` (µs CPU/row) |
|---|---|---|
| **GoGraph** | **47.8** | 1.878 |
| Memgraph | 71.6 | **0.882** |
| Neo4j | 167.7 | 0.962 |

² Quoted from the direct `noop` measurement rather than the regression
intercept: with K = 1 000 in the fit the intercept carries high leverage
(fitted 66.9 / 102.6 / 136.9). The ordering is the same either way.

**GoGraph is 1.50× more CPU-efficient than Memgraph and 3.51× more than Neo4j on
fixed per-query cost, and 2.13× / 1.95× LESS efficient per row.** The crossover
is at roughly 25 rows: below it GoGraph is the cheapest engine of the three,
above it the most expensive.

---

## 4. Why the per-row cost is high — one syscall per row

### 4.1 The profile

A CPU profile of `ggserver` under the `unwind` load (100 rows per query),
captured live via pprof:

| symbol | flat | cum |
|---|---|---|
| `internal/runtime/syscall/linux.Syscall6` | **70.19 %** | 70.19 % |
| `bolt/server.writeResponse` | — | 77.37 % |
| `bolt/server.sendResponse` | — | 72.42 % |
| `bufio.(*Writer).Flush` | — | 69.64 % |
| `syscall.write` | — | 67.90 % |
| `runtime.futex` | 6.06 % | — |

Seventy per cent of all processor time is a write syscall. Nothing in the query
engine appears until far below.

### 4.2 The cause, in one line

`bolt/proto/chunking.go:260-288` — `ChunkedWriter.WriteMessage` ends:

```go
	return cw.w.Flush()
```

Every Bolt RECORD is one message, so **a result of K rows issues K `write(2)`
syscalls**. The `bufio.Writer` in front of the connection can never accumulate
more than a single record: the buffer exists but is defeated on every message.
The behaviour is documented in the method's own godoc ("and flushes the
underlying writer"), so it is a design choice rather than an accident — but it is
the dominant cost of returning rows.

### 4.3 The causal probe: producing rows vs shipping them

`unwind_agg` is a matched pair with `unwind` — `UNWIND range(1,K) AS i RETURN
count(i)`. Identical rows are produced internally; the aggregation collapses them
so **one** record is shipped instead of K. The difference is the cost of
delivery, isolated from the cost of production:

| engine | produce (µs CPU/row) | **deliver (µs CPU/row)** |
|---|---|---|
| GoGraph | 0.052 | **1.99** |
| Memgraph | 0.015 | **0.88** |
| Neo4j | 0.018 | **0.85** |

**97 % of GoGraph's per-row cost is delivery, not computation.** Producing a row
costs GoGraph 0.05 µs — three times the peers' 0.015–0.018 µs, but negligible in
absolute terms.

### 4.4 The counterfactual, measured

A prototype in a throwaway worktree keeps `WriteMessage`'s auto-flush **on by
default** and lets the Bolt server alone turn it off, flushing explicitly in
`sendResponse` on every message that is **not** a RECORD. Every RECORD run is
terminated by SUCCESS/FAILURE/IGNORED, so the client still receives every record
before the server waits for its next request.

Both builds measured **in one process**, against the same fixture, same client,
same containers, 3 rounds each:

| workload | baseline | prototype | gain |
|---|---|---|---|
| `unwind` K=1 | 51.5 | 46.1 | 1.12× |
| `unwind` K=10 | 83.1 | 51.3 | 1.62× |
| `unwind` K=100 | 280.6 | 91.2 | 3.08× |
| `unwind` K=1 000 | 1 953.2 | **323.5** | **6.04×** |
| `expand` (8 rows, realistic 1-hop) | 84.9 | **57.2** | **1.48×** |
| `seek` (1 row) | 52.8 | 49.6 | 1.06× |
| `unwind_agg` K=1 000 (1 row shipped) | 104.5 | 97.8 | 1.07× |

The gain scales with rows shipped and vanishes when one row is shipped — exactly
the signature the mechanism predicts. Delivery cost falls **1.85 → 0.226 µs/row,
8.2×**, which is **3.9× better than Memgraph and 3.8× better than Neo4j**. On
`unwind` K=1 000 GoGraph goes from 2× worse than both peers to **3.0× better**.

`go test ./bolt/...` is **green** on the prototype.

> **A first, wrongly-scoped version of this prototype failed 45 Bolt tests**, and
> the reason is worth recording: `ChunkedWriter` is used by the in-repo *test
> client* as well as the server (`bolt_test_client_test.go:37,225`), so removing
> the flush from `WriteMessage` stranded the client's own requests in its buffer
> and every test using it hung. The engine measurement was unaffected — it drives
> the engine through `neo4j-go-driver` and asserts 1 000 rows on every operation —
> but the episode is why the shipped design must keep auto-flush on by default and
> let one caller opt out, rather than change the default for everyone.

### 4.5 Memgraph fixed this exact defect, and published the numbers

This is not a novel diagnosis; it is a defect a reference engine already hit and
repaired. Memgraph commit **`241b4ce4`, 2025-02-04, "Reduce contention around
socket send"** (verified an ancestor of `v3.9.0`): `ChunkedEncoderBuffer` **wrote
to the socket every time one Bolt chunk filled**; it was rewritten around
`pos_`/`chunk_start_` so that many chunk headers pack into one buffer and one
`write()`. Their published measurement, on the workload
`UNWIND range(1,1000000) AS a WITH a, 1 AS b, 1 AS c RETURN *`:

| concurrent clients | before | after |
|---|---|---|
| 1 | 1.715 s | 1.725 s |
| 12 | 2.490 s | 1.990 s |
| 24 | 4.810 s | 2.925 s |

Two independent confirmations of the same mechanism: their fix is flat at one
client and grows with concurrency, because the syscall's cost is contention on
the socket, not the syscall alone.

---

## 5. Why per-node predicate evaluation is expensive — the row context is a map

`scan_filter` is the second loss, and it is unrelated to §4: the query returns
**one** row, so no delivery cost is involved.

| engine | scan only (ns/node) | scan + predicate (ns/node) | predicate adds |
|---|---|---|---|
| **GoGraph** | 51.1 | **314.7** | **263.6** |
| Memgraph | 58.4 | 114.9 | 56.5 |
| Neo4j | — ³ | 107.0 | — |

³ Neo4j's `scan_count` is count-store served, so its scan cost is not separable
here; its total is the comparable figure.

**GoGraph's bare scan is the fastest of the three** (51.1 ns/node vs Memgraph's
58.4). Adding one predicate — `WHERE n.age > $a` — costs it **4.67× what it costs
Memgraph**.

### 5.1 The profile

CPU profile under `scan_filter`, self time:

| symbol | flat | note |
|---|---|---|
| `internal/runtime/maps.(*Iter).Next` | **7.18 %** | iterating the schema map, per row |
| `aeshashbody` | 3.87 % | hashing string keys |
| `encoding/binary.littleEndian.Uint64` | 3.15 % | property byte-stream decode |
| `runtime.mallocgc*` (3 symbols) | 6.71 % | 14.75 % cumulative |
| `graph/lpg.bagDecodeAt` | 2.89 % | **22.00 % cumulative** |
| `mapaccess2_faststr` / `mapassign_faststr` / `getWithoutKeySmallFastStr` / `putSlotSmallFastStr` / `mapaccess1_fast64` | 7.31 % | string-keyed map traffic |
| `runtime.convTstring` | 1.14 % | 11.93 % cum — boxing into `any` |

Summed, **≈19.3 % of all processor time is Go map machinery** and a further
~15 % is allocation.

### 5.2 The cause

- `cypher/expr/eval.go:30` — `type RowContext map[string]Value`. A row's
  variables are a **string-keyed hash map**, and expression evaluation reads
  them by name.
- `cypher/api.go:12819` — `populateRowCtx` **iterates `schema map[string]int`
  and performs a map assignment per variable, per row**:

  ```go
  for varName, colIdx := range schema {
      ...
      ctx[varName] = pv
  }
  ```

  Its cumulative cost is 18.91 %.
- `graph/lpg.bagDecodeAt` (22.00 % cum) — reading one property walks the node's
  byte-stream property bag record by record. The records are deliberately left
  **unsorted** (rmp #2408, to keep append O(1)), so lookup is a linear decode.

### 5.3 What both peers do instead

Neither peer ever sees a variable name at runtime. Both resolve names to
**integer slot offsets at plan time** and index an array at execution time:

- **Memgraph** — `Frame::operator[](const Symbol &s) → elems_[s.position()]`
  (`interpret/frame.hpp:67`) over a `pmr::vector<TypedValue>` sized once from the
  cached symbol table. The evaluator bypasses even the table:
  `Visit(Identifier&)` is `frame_->elems().at(ident.symbol_pos_)`
  (`interpret/eval.hpp:274`) — a direct index, no hashing, no strings. Schema
  names are likewise `{name, ix}` pairs of which only `ix` is used at runtime, so
  **an indexed point lookup does zero name resolution during execution**.
- **Neo4j** (slotted runtime, which is what Community actually gets — the
  pipelined and parallel runtimes are Enterprise-only) — two flat arrays per row,
  one of `long` and one of references, with entity ids kept as **raw `long`**
  rather than boxed objects, and bulk copies via `System.arraycopy`.
  `SlottedRewriter.scala:384` states the intent outright: specialise so as to
  avoid creating `NodeValue`/`RelationshipValue` objects at all.

The transferable lesson, in Neo4j's own design: `Values.longValue(long)` is
`return new LongValue(value)` with **no small-integer cache**
(`Values.java:212-214`) — the win is not a cheaper box, it is arranging not to
need one.

---

## 6. The plan cache — GoGraph is middle, and Neo4j is worst

`seek_literal` is a matched pair with `seek`: identical semantics and work, the
changing key travelling in the query **text** instead of the parameters.

| engine | parameterised | rotating literal | penalty |
|---|---|---|---|
| **Memgraph** | 91.9 | **80.8** | **none** ⁴ |
| **GoGraph** | 52.6 | 86.7 | **+65 %** |
| **Neo4j** | 142.6 | 908.0 | **+537 %** |

⁴ Within run-to-run noise; the rotating arm measured marginally cheaper.

- **GoGraph** keys its plan cache on the **raw query text** — `cypher/plan_cache.go:31`
  ("a bounded LRU keyed by query text"), consulted at `cypher/api.go:4577` with
  `e.cache.get(query)`. There is no literal normalisation, so a rotating literal
  misses on every execution and re-parses and re-translates.
- **Memgraph** strips literals into parameters **unconditionally, before any
  cache lookup**, with a hand-written lexer (`frontend/stripped.hpp:24-27`,
  `cypher_query_interpreter.cpp:78`); the AST builder then emits a
  `ParameterLookup` keyed on token position **instead of** a literal
  (`cypher_main_visitor.cpp:3457-3469`). `{id:1}` and `{id:2}` are one cache
  entry. This is why its penalty is zero, and it is a direct confirmation of the
  mechanism rather than an inference.
- **Neo4j** pays 6.4×, and its idle rate during that arm rose to **41 686 µs/s**
  — a JIT compilation storm driven by a thrashing cache.

Even so, GoGraph is the cheapest of the three on the parameterised path, and
**10.5× cheaper than Neo4j on the rotating one**. This is a real gap against
Memgraph, not against the field.

Related and already recorded: GoGraph rebuilds the **physical** plan on every
execution while both peers cache an executable tree
(`docs/audit-examples-pprof-2026-08-10.md` §3, rmp #2383). That work is inside
the 47.8 µs fixed cost — which is still the lowest of the three.

---

## 7. CPU efficiency under concurrency — the advantage widens

Everything above is at one client. Processor cost per operation was also measured
at 8 and 64 concurrent clients, which is where contention shows up: contention
does not idle a core, it **burns** one, on futex wakeups and cache-line
ping-pong.

**`seek` — µs CPU per operation**

| engine | c=1 | c=8 | c=64 | c=1 → c=64 |
|---|---|---|---|---|
| **GoGraph** | 53.3 | **32.0** | **30.8** | **−42 % (improves)** |
| Memgraph | 65.7 | 118.2 | 77.4 | +18 % |
| Neo4j | 135.1 | 144.7 | 112.7 | −17 % |

**`expand` — µs CPU per operation**

| engine | c=1 | c=8 | c=64 | c=1 → c=64 |
|---|---|---|---|---|
| **GoGraph** | 85.4 | **61.7** | **62.3** | **−27 % (improves)** |
| Memgraph | 79.8 | 140.0 | 128.2 | +61 % |
| Neo4j | 147.8 | 163.1 | 150.5 | +2 % |

**GoGraph gets cheaper per operation as clients are added; Memgraph gets
markedly more expensive.** At 64 clients GoGraph serves a point lookup for
**2.51× less CPU than Memgraph and 3.66× less than Neo4j**, and a 1-hop expansion
for **2.06× / 2.42× less** — and the expansion figure still carries the §4
per-row syscall defect, so the true margin is wider.

Memgraph's degradation is consistent with the mechanism the concurrency audit
identified at source: its "shared" lock is a full `std::mutex`
(`utils/resource_lock.hpp:131-138`), acquired twice per autocommit query.

This is the same property the concurrency audit measured as throughput scaling
(GoGraph 8.76× vs Neo4j 6.44× vs Memgraph 3.86× from 1→64) seen from the cost
side: GoGraph scales best **because** it does not spend more processor time per
operation as contention rises.

---

## 8. What this says about the state of the module

**On processor efficiency GoGraph is not behind its reference engines; it is
ahead of them on the axis that dominates OLTP, and behind them on two specific,
localised mechanisms.**

| axis | verdict |
|---|---|
| Fixed CPU per query | **Best of three** — 1.50× better than Memgraph, 3.51× better than Neo4j |
| Point lookup, 1-hop, 2-hop | **Best of three** |
| Bare label scan | **Best of three** (51.1 ns/node) |
| Plan-cache robustness to literals | Middle — worse than Memgraph, 10.5× better than Neo4j |
| Marginal CPU per row shipped | **Worst of three, 2.1×** — one syscall per row (§4) |
| Predicate evaluation per row | **Worst of three, 2.7–2.9×** — map-keyed row context (§5) |
| CPU per operation at 64 clients | **Best of three** — 2.5× better than Memgraph, 3.7× than Neo4j; the only engine whose per-operation cost FALLS with concurrency (§7) |

Both losses are per-element costs, and neither is intrinsic to Go:

1. **§4 is one line of code.** The counterfactual is measured, the fix design
   passes the Bolt suite, and a reference engine already made the identical fix.
2. **§5 is an architecture change** — resolving row variables to integer slots at
   plan time instead of a `map[string]Value` per row. It is the same boundary
   both peers draw, and the same boundary already identified for the physical
   plan in rmp #2383. It needs a decision before work starts.

The honest summary is that the premise behind this audit was half wrong, and the
half that was right had a cause in the **transport layer**, not the query engine.

---

## 9. Filed

| id | kind | pri | summary |
|---|---|---|---|
| **#2410** | BUG | 9 | Bolt `ChunkedWriter` flushes per message ⇒ one `write(2)` syscall per returned row (§4). Fix design already validated, `go test ./bolt/...` green. |
| **#2411** | SPIKE | 7 | Slot-addressed row context to replace the per-row `map[string]Value` (§5). **Architecture — the user decides before work starts.** |
| **#2412** | IMPROVEMENT | 5 | Normalise literals into parameters before the plan-cache lookup (§6). |

#2410 is the highest-value item in this audit: the largest measured gain, the
smallest change, a validated design, and a reference engine that made the same
fix. #2411 is the larger win in the long run but is an architecture change.

---

## 10. Reproduce

```bash
docker network create ggbench
docker run -d --name gg-neo4j --network ggbench --cpus=4 -m 8g \
  -e NEO4J_AUTH=neo4j/gographbench \
  -e NEO4J_server_memory_heap_initial__size=2G -e NEO4J_server_memory_heap_max__size=2G \
  neo4j:5.26-community
docker run -d --name gg-memgraph --network ggbench --cpus=4 -m 8g \
  memgraph/memgraph:3.9.0 --telemetry-enabled=false
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ggserver ./bench/comparison/ggserver
# image gograph-bench:local
docker run -d --name gg-gograph --network ggbench --cpus=4 -m 8g -e GOMAXPROCS=4 gograph-bench:local

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -tags=threeway -c -o ccbench ./bench/comparison/
# image ccbench:local, then (note the cgroup mount — that is the instrument):
docker run --rm --network ggbench --cpus=3 -m 4g -v /sys/fs/cgroup:/hc:ro \
  -e CC_ONLY="gograph-bolt,neo4j-bolt,memgraph-bolt" \
  -e CC_NODES=5000 -e CC_DEGREE=8 \
  -e CC_GOGRAPH_URI="bolt://gg-gograph:7689" \
  -e CC_NEO4J_URI="bolt://gg-neo4j:7687" \
  -e CC_MEMGRAPH_URI="bolt://gg-memgraph:7687" \
  -e CPU_CG_gograph-bolt="/hc/docker/$(docker inspect gg-gograph --format '{{.Id}}')/cpu.stat" \
  -e CPU_CG_neo4j-bolt="/hc/docker/$(docker inspect gg-neo4j --format '{{.Id}}')/cpu.stat" \
  -e CPU_CG_memgraph-bolt="/hc/docker/$(docker inspect gg-memgraph --format '{{.Id}}')/cpu.stat" \
  -e CPU_ROUNDS=3 -e CPU_DURATION_MS=4000 -e CPU_LEVELS=1 \
  ccbench:local -test.run TestCPUEfficiency -test.v -test.timeout 170m
```

The client must run **inside** the VM. `CPU_EXTRA_NAME`/`CPU_EXTRA_URI` add a
fourth target, which is how the §4.4 prototype was measured in the same process
as the baseline.
