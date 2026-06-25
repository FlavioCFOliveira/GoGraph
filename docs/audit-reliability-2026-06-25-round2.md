# Reliability Audit — 2026-06-25 (round 2)

A second empirical reliability pass after round 1 (`docs/audit-reliability-2026-06-25.md`)
fixed its 9 findings. This round goes **deeper and into different territory**:
the newly-added round-1 code, openCypher corners round 1 did not examine,
interconnection failure modes, and the support/infrastructure components.

## Method (empirical)

| Gate | Result |
|---|---|
| `go build ./...`, `go test -race ./...` | green (from round 1, re-confirmed) |
| Deep fuzz **60 s** each: `FuzzParse`, `FuzzDecodeValue`, GraphML, JSONL | **no crashers** (beyond round 1's 20 s smoke) |
| Cross-release back-compat (`-tags=soak`): current code recovers v0.2.0/v0.3.0 WAL images | **PASS** — round-1 storage changes are back-compatible with shipped releases |
| `internal/sim` differential | green |

Four specialist clusters:
1. **Round-1 new storage code** (indexdefs.bin, store-direct counters, recovery seeding) — **CERTIFIED sound**; DROP durability, last-writer-wins, corruption fail-stop, back-compat and counter-drift all verified correct. Only an *informational* note that the WAL-retention fail-safe (WAL not truncated when an index exists and `WithIndexSpecs` is unwired) is the intended documented behaviour.
2. **Concurrency & interconnections** — **no findings**. The new store-direct counter mutexes are leaf locks; `HasIndexes`/`HasConstraints` read atomics lock-free (torn read impossible by construction); the checkpoint phase-3 re-check has no missed-update window (proven for both the txn-apply and engine `CommitWALOnly` paths); Bolt teardown/RESET/mid-PULL-cancel/tx-timeout-reaper release resources exactly once; a 5 s DDL-churn soak under goleak leaked nothing. Suggested two hardening gates → #1774.
3. **Deeper openCypher conformance** — 6 TCK-uncovered deviations (below).
4. **Under-audited components** (io round-trip, generation determinism, metrics, search/extern, internal/clock/testfs/crashpoint/subproc) — 5 findings (below); generator determinism verified across 40 processes, metrics race-free, extern memory-bounded and correct, crashpoint/subproc sound.

## Findings → rmp sprint 243

No Critical. The two High items are clear conformance/observability defects.

| # | Sev | Finding | Location |
|---|---|---|---|
| 1764 | High | `toString()` of an integer-valued FLOAT drops the `.0` (`"1"` not `"1.0"`) | `cypher/funcs/essentials.go:677` |
| 1765 | Med | Runtime `ArithmeticOverflow` mapped to `Neo.DatabaseError.General.UnknownError` (server fault) + message hidden | `bolt/server/errors.go:181` |
| 1766 | Med | Integer `/0` and `%0` return `null` instead of raising (vs Neo4j `ArithmeticError`) — **decision fork** | `cypher/expr/eval.go:1383` |
| 1767 | Med | Invalid calendar date components silently normalized (`date('2020-13-01')→2021-01-01`) | `cypher/expr/temporal.go:587` |
| 1768 | Med | `substring`/`left`/`right` negative args clamp instead of `ArgumentError` | `cypher/funcs/essentials.go:1033` |
| 1769 | Med | IO temporal property time-zone offset silently normalized to UTC on export | `graph/io/graphml/writer_props.go:130`, `graph/io/jsonl/writer.go:261` |
| 1770 | Low | Map projection `n{.name, .*}` fully implemented but parser rejects it (dead code + missing feature) | parser / `cypher/expr/map.go` |
| 1771 | Low | `testfs.CorruptOnRead` doc/code mismatch (`^=0xFF` vs "MSB") + head-only fidelity | `internal/testfs/testfs.go:237` |
| 1772 | Low | Fake clock godoc overstates "never dropped" for tickers | `internal/clock/fake.go:13` |
| 1773 | Low | `search/extern` `TestPageRank_MatchesInMemory` vacuous (dead assertion loop) | `search/extern/pagerank_test.go:107` |
| 1774 | Low | Hardening: add gates for the checkpoint phase-2-DDL race window + BeginReadTx isolation | (new tests) |

### Decision deferred to execution time

- **#1766 (integer `/0`/`%0`):** openCypher 9 leaves this impl-defined; Neo4j
  raises `ArithmeticError`. Fix by aligning with Neo4j (fail-stop) **or**
  documenting the `null` result as a sanctioned divergence + gate.

## Verified sound (negative results)

Round-1 storage code (DROP durability, corruption fail-stop, counter-drift,
back-compat); all new concurrency code (`-race`/goleak clean); deep fuzz;
cross-release recovery; three-valued logic, comparison chaining, CASE/coalesce,
`IN`/list-slice/list-concat/`reduce`, cross-type numeric equality, predicate
functions, temporal component access + `date + duration`, `round`/`floor`/`%`
sign rules, `toInteger`/`toFloat` of bad strings; JSONL+GraphML round-trip of
every property type (int64 extremes, full-precision floats, unicode/control
chars, bytes) **except** the temporal-offset case (#1769); generator determinism
(40 processes); metrics race-freedom & bounded cardinality; extern memory bound
& correctness; crashpoint build-tag gating; subproc re-exec.
