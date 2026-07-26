# Per-statement write counters — measured cost

**Tasks #2212 and #2190** · sprint 321 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=^$ -bench='BenchmarkEngWriteAutocommit' -benchmem -count=10 ./bench/mtaudit/`

---

## 1. What was missing, and a premise corrected

The audit specified #2190 as populating Bolt's `stats` metadata "from the counters the
engine already maintains". Verified exhaustively, that premise is only **one third** true:

- the graph maintains **four** counters — nodes added/removed, edges added/removed —
  exposed as `lpg.Graph.SideEffectCounters` for the openCypher TCK side-effect comparator;
- but they are **graph-scoped running totals** with a snapshot/reset protocol, not
  per-statement attribution, and they cover neither properties, labels, nor schema
  objects;
- and `cypher.Result` had no statistics surface at all.

So the missing driver write counters were not a Bolt plumbing gap — the engine had nothing
per-statement to report. #2212 was created mid-cycle as the prerequisite and #2190 reduced
to the protocol half.

## 2. What the counters count

`exec.QueryCounters` models **openCypher's** eight side effects, because that is the
vocabulary the vendored TCK feature files use in their `Then the side effects should be`
tables: `+nodes`, `-nodes`, `+relationships`, `-relationships`, `+properties`,
`-properties`, `+labels`, `-labels`. Schema effects (index/constraint added/removed) are
carried too because Bolt reports them.

Two semantics were established from the spec's own tests rather than from memory:

- **A property removal is its own effect.** `REMOVE n.num` declares `-properties 1`
  (`cypher/tck/features/clauses/remove/Remove1.feature`). Bolt has no
  properties-removed, so the two are summed into `properties-set` **at the protocol
  boundary** — the one lossy step, kept in `bolt/server/result_stats.go` rather than in
  the counters.
- **A deleted node's labels and properties are not separate effects.** `DELETE n` and
  `DETACH DELETE n` declare `-nodes 1` and nothing else
  (`cypher/tck/features/clauses/delete/Delete1.feature` [1] and [2]). Deletion is
  implemented as strip-edges → strip-labels → strip-properties → tombstone, through the
  same calls a user's `REMOVE` uses, so those strips are wrapped in an
  `EffectCountingSuppressor` span. Without it, `DETACH DELETE` reported a phantom
  `-labels 1` — a real bug the TCK-grounded tests caught.

The node and relationship counters are incremented at the **same call sites** as the
graph's TCK counters, via one helper per effect, so the per-statement counts cannot drift
from the counts the TCK already verifies and no site can be missed by omission.

## 3. The wire contract, read from the decoder

Transcribed from the driver that must read it (neo4j-go-driver v5.28.4), as #2189 did for
the entity structures:

- key names are the constants in `neo4j/db/summary.go`;
- `contains-updates` is a **BOOLEAN** on the wire and every other key an **INTEGER** —
  the hydrator's `parseStatValue` switches on the key and reads everything else with
  `unp.Int()`, so an unknown key would be silently misread. The encoder therefore emits
  only names from that list.
- Zero counters are omitted and the whole map is omitted when nothing changed, matching
  Neo4j; the driver's `successStats` returns nil for an empty map, so a read-only query's
  SUCCESS is byte-identical to before.

## 4. Measured cost

`BenchmarkEngWriteAutocommit`, one autocommit write per operation, `-count=10`:

| | before | after | change |
|---|---|---|---|
| sec/op | 1.944 µs ± 1% | 2.035 µs ± 1% | **+4.68%** (p=0.000) |
| B/op | 2.432 KiB | 2.543 KiB | +4.58% |
| **allocs/op** | **33.00** | **33.00** | **~ (p=1.000)** |

**Allocations are unchanged.** The first implementation allocated the counters separately
and cost +1 alloc/op and +5.79%; folding them into the adapter's own allocation — the
adapter is already one heap object, so the pointer aims inside it — removed the extra
allocation entirely.

The residual +4.68% is the adapter growing from ~80 bytes to 176, which crosses a Go
size class (80 → 176) and so zeroes ~96 more bytes per statement. Measured directly:
`unsafe.Sizeof(lpgMutatorAdapter{})` is 176 bytes, 176 for the WAL adapter's 184.

**This regression is accepted and documented rather than eliminated**, because the feature
cannot be free: its entire purpose is to record per-statement data the engine did not
record before. The trade is 4.68% on write statements against a server that previously
told clients `ContainsUpdates=false` after a successful `CREATE` — returning
plausible-looking wrong data, which the project's decision framework ranks strictly worse
than being slower (correct → secure → fast). Read statements are untouched: a read-only
adapter leaves the counters nil.

A cheaper variant was considered and rejected: shrinking the counters to `int32` would
halve the struct but overflow at ~2.1 billion effects in a bulk load.

## 5. Correctness

`cypher/query_counters_test.go` asserts the exact effect set for CREATE (bare, labelled,
multi-label, with properties, relationships), SET, REMOVE, DELETE, DETACH DELETE and
MERGE, with the expected numbers taken from the openCypher TCK's declared side effects.
It covers the no-op cases that must count nothing — removing an absent property, adding a
label the node already has, removing an absent label — and cross-checks the counts against
direct graph inspection rather than against the counters themselves.

This gate is necessary because the TCK does **not** verify these: its own side-effect step
checks nodes and relationships as a *lower bound* and skips properties and labels entirely
(`cypher/tck/compare_test.go sideEffectsTable`, documented in `conformance_history.go`).

`bolt/server/e2e_result_stats_test.go` asserts the numbers the **official driver** reports
through `ResultSummary.Counters()` — the only assertion that proves the whole wire
contract, since the key names, the integer-versus-boolean value types and the placement on
the terminal SUCCESS all have to be right for the driver to surface them. It includes the
audit's headline case (a successful CREATE), MERGE-created versus MERGE-matched, the
DETACH DELETE phantom-label check, and a read-only query reporting no updates.

TCK 3897/3897. `make ci` green.
