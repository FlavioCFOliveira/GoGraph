# Example 36 — Snapshot isolation on the topology dimension

## Scenario

An application whose graph **structure** changes while it is being read. One
ingest goroutine commits relationships one transaction at a time — sometimes
linking a node that already exists, sometimes creating the node and its
relationship in the same transaction — while a pool of readers repeatedly asks
the graph a structural question:

```cypher
MATCH (:Hub {id: 0})-[r:LINK]->(s:Spoke)
RETURN count(r) AS links, count(DISTINCT s) AS spokes
```

This is not an arbitrary query. It is an **anchored, relationship-typed expand**,
which is the shape that drives the forward/reverse CSR pair and the per-arc
relationship-type filter derived from it. An untyped or unanchored variant
exercises neither, and would observe nothing.

## Objective

Answer one question with evidence: **does a read observe a structurally
consistent graph while the structure is being rewritten beneath it?**

Isolation is usually demonstrated on *property* values — example 27 does exactly
that, with a conserved ledger total that no interleaving may tear. That leaves
the other half of the graph untested. A property invariant cannot detect a
relationship that appears under a running read, disappears from it, or is
counted against the wrong endpoint, because none of those change any property.
This example covers that half.

## The invariant, and why it is a bracket

A reader running concurrently with a writer has **no single correct answer** to
"how many `LINK` edges are there". The honest answer is a range, and the range
is precisely what snapshot isolation promises. So each reader brackets its
observation against the ingest's acknowledged-commit counter:

| Sample | When | Meaning |
|---|---|---|
| `lo` | **before** the query starts | every one of those commits had already returned to the writer, so a transaction starting later **must** see it |
| `hi` | **after** the query returns | the snapshot began before this sample, so it cannot legitimately contain a write acknowledged after it |

The rule is `lo ≤ observed ≤ hi`:

- `observed < lo` — a **committed, acknowledged write was made invisible**.
- `observed > hi` — the read **observed the future**.
- anything in between is a legal serialisation and is not a finding.

Writes acknowledged *during* the query may or may not be visible, and either is
correct. That is why pinning a single expected number would manufacture false
failures.

A second, independent invariant runs alongside it: the ingest gives every spoke
exactly one `LINK`, so `count(r)` must equal `count(DISTINCT s)`. This is not
redundant with the count check — it catches a derived per-arc structure that has
desynchronised from the topology it indexes, which a count alone cannot see.

### What this example deliberately does NOT do

An earlier draft counted the same pattern **twice inside one query** and compared
the two results. That finds nothing: both counts are served from one derived
snapshot built once per query, so they agree even when both are wrong. The
bracket against an external, acknowledged-commit counter is what gives the check
its power.

## Purpose

This is the instrument for rmp **#2293**, and it is validated in both
directions — the property every regression instrument needs and few have:

**Against the defective engine** (before the fix), one 120 ms run reported:

```
invisible_commits=10
future_reads=0
read_errors=57
snapshot_topology_invariant_holds=0
example 36: 56 observation quer(ies) FAILED; first error:
  cypher: internal panic: runtime error: index out of range [4] with length 4
```

Three distinct defects, all invisible to a fully green test suite:

1. **Queries failed outright.** The CSR pair was built by two passes over the
   *live* adjacency; a writer landing between them made the second pass find
   arcs the first had not counted. Recovered into an error rather than a crash,
   so the process survived and every such query simply returned nothing.
2. **Committed edges were invisible.** The pair, and the relationship-type
   filter indexed by its arc positions, were cached under the topology epoch
   alone. Under MVCC the pair also depends on the reader's instant, so two
   readers holding different snapshots were served each other's.
3. **Reads observed the future.** The pair's arcs came from the present
   adjacency, so a read could traverse an edge committed after its snapshot
   started.

**Against the fixed engine**, the same run reports `invisible_commits=0`,
`future_reads=0`, `misaligned_far_endpoints=0`, `read_errors=0`,
`snapshot_topology_invariant_holds=1`.

## Running it

```bash
go run ./examples/36_mvcc_snapshot_topology/

# A longer, harder run: more structural commits and more readers.
go run ./examples/36_mvcc_snapshot_topology/ -spokes 4000 -readers 16

# Every commit creates its node and its edge together, so every write
# exercises node-birth visibility as well as edge-birth visibility.
go run ./examples/36_mvcc_snapshot_topology/ -new-node-every 1

# Never create a node inside the linking transaction, so both endpoints always
# pre-exist and nothing about node liveness can mask an arc.
go run ./examples/36_mvcc_snapshot_topology/ -new-node-every 0
```

| Flag | Default | Meaning |
|---|---|---|
| `-spokes` | 400 | structural writes the ingest commits, one edge per commit |
| `-readers` | 4 | size of the observing reader pool |
| `-duration` | 30s | upper bound on the concurrent phase |
| `-new-node-every` | 2 | create the spoke node in the same transaction as the edge every Nth commit (0 = never) |

The run exits non-zero and names the violation if any invariant fails, so it is
usable as a gate and not only as a report.

## Evidence collected

Bare `key=value` lines are **deterministic facts**, pinned by `main_test.go`.
Lines prefixed with `# ` are **volatile telemetry** and are never pinned.

| Fact | What it establishes |
|---|---|
| `links.committed` / `links.final` | every acknowledged commit is present at the end |
| `final_read_sees_every_commit` | read-your-writes across the whole run |
| `final_far_endpoints_align` | the count and the endpoint set agree |
| `invisible_commits` | committed writes a reader could not see (must be 0) |
| `future_reads` | writes a reader saw before they were acknowledged (must be 0) |
| `misaligned_far_endpoints` | per-arc side data desynchronised from topology (must be 0) |
| `read_errors` | observation queries that failed (must be 0) |
| `snapshot_topology_invariant_holds` | the headline verdict |

Telemetry covers reader latency (p50/p99/max), writer throughput, the peak live
MVCC version count, and heap allocation and growth, so the isolation guarantee
can be read together with what it costs.

### Deeper measurement

The example is an ordinary Go program, so the standard instruments apply:

```bash
# CPU and heap profiles, rendered as flame graphs.
go test -run TestRun -cpuprofile cpu.out -memprofile mem.out ./examples/36_mvcc_snapshot_topology/
go tool pprof -http=:0 cpu.out

# Scheduling, goroutine blocking and GC pauses over the run's timeline.
go test -run TestRun -trace trace.out ./examples/36_mvcc_snapshot_topology/
go tool trace trace.out

# GC behaviour while the version chains fill and are reclaimed.
GODEBUG=gctrace=1 go run ./examples/36_mvcc_snapshot_topology/ -spokes 4000

# Which GoGraph paths this example actually drives.
go test -run TestRun -coverpkg=github.com/FlavioCFOliveira/GoGraph/... \
  -coverprofile cover.out ./examples/36_mvcc_snapshot_topology/
go tool cover -func cover.out | tail -20
```

## Relationship to the other MVCC examples

| Example | Dimension | Invariant |
|---|---|---|
| [27_concurrent_txn](../27_concurrent_txn/) | **property** values | a conserved ledger total is never observed torn |
| [35_mvcc_mixed_workload](../35_mvcc_mixed_workload/) | **latency** | a point query keeps its latency beside a long read and an ingest stream |
| **36 (this one)** | **topology** | a read never sees a structure no serial schedule produces |

The three are complementary: 27 covers what an object *contains*, 36 covers how
objects are *connected*, and 35 covers what the isolation costs.
