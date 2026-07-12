# INDEX feature — completeness & readiness audit (2026-07-12)

## Scope and method

Goal: evaluate and correct the **completeness** and **readiness** of the
**INDEX** feature — node-property secondary indexes (`hash` for equality,
`btree` for range with a unified numeric companion), the `CREATE INDEX` /
`DROP INDEX` DDL, `db.indexes()` introspection, index-seek planning, and
durability/recovery — while preserving the two non-negotiable invariants:
100% openCypher TCK (baseline **3897**) and full **ACID**.

Method (evidence-based, no assumptions):

- Three specialists audited in parallel, read-only, each adversarially
  verifying its own findings before reporting:
  - **cypher-expert-consultant** — openCypher conformance and the
    seek-equals-scan invariant.
  - **storage-engine-auditor** — durability / crash-safety / ACID of index
    defs and contents.
  - **graph-theory-expert** — correctness/completeness of the B+ tree, hash,
    label, and `NodeSet` structures and their maintenance.
- Own DDL-parser fuzzing (caught a finding the conformance pass did not).
- Empirical gates at baseline and after every fix: `go build ./...`, the
  index package suites, `-race` on `graph/index`, TCK 3897, the DST
  index-diversity scenario, and the range-seek differential harness.

## Verdict

**Broadly complete and production-grade for its declared scope.** The seek
paths never diverge from scan+filter for any realistic input (a residual
Filter is always retained on range seeks; the hash-equality seek fires only on
a genuine single equality). All three specialists converged; two HIGH findings
and one MEDIUM were confirmed and **fixed**, plus six LOW hardening/faithfulness
items. After remediation the feature is **PRODUCTION-READY**; TCK 3897, ACID,
and `-race` held throughout.

## Findings and remediation (rmp sprint 265)

| # | Sev | Finding | Fix | Commit |
|---|-----|---------|-----|--------|
| 1894 | HIGH | A CREATE/DROP INDEX racing a background checkpoint could **resurrect a dropped index** (or lose a second created one): `recordIndexDef`/`forgetIndexDef` ran *after* `commitIndexTx` released the single-writer semaphore, so a checkpoint could capture a WAL watermark past the durable DDL while the def registry lagged; `snapshotIsSelfSufficient` is presence-only. **ACID-Durability.** | Moved the registry update *before* `commitIndexTx`, inside the DDL's single-writer window, in all three WAL paths; CREATE unwind forgets the def on WAL failure. The checkpoint now always captures a consistent `(watermark, defs)` pair — no checkpoint change. storage-engine-auditor **CERTIFIED**. | `872633d` |
| 1895 | HIGH | String range-seek upper bound used a fixed 32×`0xFF` sentinel, not a true maximum → a byte-string sorting above it was **silently dropped** (seek≠scan). UTF-8-unreachable, so TCK stayed green. **Correctness.** | Added open-ended `btree.RangeFrom`/`RangeCountFrom`; the string adapter and planner route the unbounded-above case through them. Numeric ranges already used true `±Inf`. | `1cc6504` |
| 1896 | MED | The hand-written DDL parser **panicked** (index-out-of-range) on truncated input (`CREATE INDEX x FOR (`, `… ON (`, `CREATE CONSTRAINT c ON (`); recovered into an opaque error at `Run`, raw panic for direct `ir.ParseDDL` callers. **Robustness.** | Bounds-safe `tokAt` across `parseNodePattern`/`parsePropAccess`; truncation returns a typed `SyntaxError`. Malformed-input sweep regression. | `71f73f3` |
| 1897 | LOW | `NodeByIndexRangeScan` enforced exclusive bounds by comparing the emitted **NodeID** to the property-value bound — meaningless; a fail-silent trap for direct exec callers with exclusive int64 bounds. A test masked it with a NodeID==key fixture. | Removed the post-filter; the operator honestly emits the inclusive `[lo,hi]` superset (the planner's residual Filter enforces exactness). Tests rewritten to break the NodeID==key coincidence. | `acf0537` |
| 1898 | LOW | The `Subscriber` "order-independent on the change stream" godoc overclaimed (chained same-property SETs). Not a live defect (delivery preserves mutation order; recovery rebuilds via BulkLoad). | Restated the real ordering contract. Doc-only. | `94c5afc` |
| 1899 | LOW | DDL accepted a user index name carrying the reserved `_btree_num` companion suffix (would hide it from `db.indexes()`); composite-index attempts gave a bare "expected ')'". | Reject the reserved suffix at parse time; specific composite-index error message. | `255e2c0` |
| 1900 | LOW | `docs/cypher.md` claimed a btree index "supports ORDER BY" (no such optimization); the Go-API `int64` hash cross-type contract was undocumented. | Corrected the doc (range predicates only; dialect/scope); documented the `int64`-hash Go-API contract in code. | `765aa59`, `c573121` |
| 1901 | LOW | `readVerifiedIndexDefs` allocated from a length field before the manifest CRC was verified. | Bounded the reader by the manifest size (as the properties/labels components do) and capped the pre-allocation hint. | `4a87b2e` |

## Certified-sound sub-areas (no change needed)

- **B+ tree** total order incl. float64 NaN placement; `BulkLoad` sorts
  internally (safe on the unsorted mapper-order input the backfill passes);
  duplicate-key postings; delete/tombstone posting removal; the numeric
  companion's monotone `int64→float64` superset property (0 misses in 200k
  trials). *(graph-theory-expert)*
- **Hash** equality-seek exactness without a residual Filter (fires only on a
  genuine single equality); NULL / type-mismatch / NaN / list / temporal-tagged
  seek values all resolve consistently with scan+filter. *(cypher-expert)*
- **Durability** of the CREATE path, recovery replay ordering, indexdefs.bin
  framing, numeric-companion re-derivation determinism, and def-registry
  isolation. *(storage-engine-auditor)*
- **db.indexes()**, `IF [NOT] EXISTS` idempotency, plan-cache correctness
  across schema changes, and the case-sensitive coverage gate (auto-name
  collisions can never serve a wrong index). *(cypher-expert)*

## Non-blocking follow-up

- **#1902** (backlog): a rare WAL-commit-*failure* edge where a racing
  checkpoint could publish a transient failed-CREATE schema def before the
  DDL unwinds. Bounded to transparent index metadata (query correctness
  preserved) and a strict improvement over the original bug; the clean closure
  is checkpoint-side and also covers the more-serious constraint-DDL instance,
  so it is tracked separately rather than widening this cycle.

## Deliberate scope boundaries (documented, not defects)

Single node-property indexes only; kinds are `hash`/`btree` (not Neo4j
`RANGE`/`TEXT`/…); composite and relationship-property indexes are rejected
with a clear error; `SHOW INDEXES` is offered as `CALL db.indexes()`. The
openCypher TCK does not cover index DDL, so none of these affect conformance.
