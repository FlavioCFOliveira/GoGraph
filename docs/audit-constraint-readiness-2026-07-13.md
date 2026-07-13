# CONSTRAINT feature — completeness & production-readiness audit (2026-07-13)

## Scope

Evaluate and remediate the **CONSTRAINT** feature of GoGraph for exemplary
production functioning: `CREATE CONSTRAINT` / `DROP CONSTRAINT` for node-scoped,
single-property `UNIQUE` and `NOT NULL` constraints — parser, IR, executor,
registry, enforcement, durability, recovery, listing, and the Bolt error
surface.

The work was driven by a four-specialist audit at commit `588b325`:

- **Cypher-conformance** — syntax/semantics vs openCypher and the Neo4j Cypher Manual.
- **Storage/ACID** — durability, atomicity, isolation, crash-recovery.
- **Go/concurrency** — idiom, race-safety, resource bounds, test coverage.
- **Security** — attack surface from untrusted DDL text and corrupt on-disk defs.

Constraints are DDL and are **not** covered by the openCypher execution TCK, so
every change here is TCK-neutral; the full TCK suite remained green throughout.

## Verdict

The constraint **enforcement engine** was already correct (commit-time NOT NULL
final-state semantics, UNIQUE null-exemption, delete-frees-value, at-creation
validation, durability, isolation). The defects were on the **DDL surface**, in
two UNIQUE **write-path** correctness bugs, one **durable-width/length-cap** ACID
breach, and durability/documentation/idiom hardening. All confirmed findings
have been remediated with regression tests; the feature is production-ready.

## Findings and remediation

| # | Sev | Finding | Resolution | Commit |
|---|-----|---------|-----------|--------|
| H-A | HIGH | Constraint identifier crossed the durable boundary with a `uint16` WAL prefix + snapshot write(`uint32`)/read(64 KiB-cap) mismatch and **no length cap** → silent loss/truncation after crash, or a poison-pill snapshot | Cap identifiers at the DDL boundary (`maxSchemaIdentifierLen = 4096`); WAL encoders fail-stop on overflow; snapshot writer rejects a field wider than the reader accepts | `46257de` |
| H-B | HIGH | MERGE `ON MATCH/CREATE SET` never released the replaced UNIQUE value → permanent phantom reservation (unbounded dead-value leak) | Release the old value before check+overwrite in `merge.go` and `merge_pattern.go` (incl. the `= null` path) | `10c65ba` |
| H-C | HIGH | Idempotent self-set of a UNIQUE property (`SET n.x = n.x`, `+=`, ETL re-runs) falsely rejected | Release-before-check on all four `set.go` paths | `cd62d1e` |
| H-D | HIGH | Modern `FOR … REQUIRE` syntax entirely rejected (only removed-in-Neo4j-5 `ON … ASSERT` parsed); `FOR` mis-parsed as a name | Accept `FOR … REQUIRE` (primary) + `ON … ASSERT` (legacy alias); reject relationship/composite/NODE KEY/type constraints with specific errors | `1cbed1c` |
| M-B | MED | Constraint-name collisions unchecked → non-deterministic `DROP CONSTRAINT <name>` | Reject duplicate names (`ConstraintWithNameAlreadyExists`); deterministic `ResolveByName` | `687f11b` |
| M-C | MED | Duplicate-CREATE inconsistent (NOT NULL silently overwrote name; UNIQUE leaked the `__uniq__` index name) | Uniform constraint-level already-exists error (`ConstraintAlreadyExists`); no index-name leak | `687f11b` |
| M-D | MED | `db.constraints()` `name` column returned the synthetic `label.prop` key | Return the declared name (fallback to `label.prop` only when anonymous) | `9bbea37` |
| M-E | MED | UNIQUE treated int `1` ≠ float `1.0`, diverging from `=` and MERGE (#1240) | **Decision: align.** Value-equivalence folding (1 ≡ 1.0, ±0 ≡ 0, NaN ≡ NaN; transitive, exact-value based) | `9e25973` |
| M-F | MED | Commit-time NOT NULL check re-scanned + re-allocated per touched node | Copy-on-write `label → props` index; O(1), 0 allocs/op | `37ee63a` |
| M-G | MED/LOW | `DROP INDEX __uniq__…` could drop a constraint's backing index; `CREATE INDEX` could squat it | Reserve the `__uniq__` prefix against user index DDL | `455386f` |
| M-H | MED/LOW | `NewEngineWithStore` silently dropped recovered constraints (enforcement lost) | Godoc caveat + a construction-time warning when durable constraints are unregistered | `b9ae87a` |
| M-A | MED | Failed CREATE/DROP CONSTRAINT under fsync-fault + concurrent-checkpoint race could persist across restart (atomicity of a *failed* DDL) | **Decision: fix now via #1902** — checkpoint's phase-1 capture aborts when the WAL is poisoned, via new `wal.Writer.Poisoned()`; closes CONSTRAINT + INDEX (backlog #1902 closed) | `991abab` |
| L-A | LOW | `docs/cypher.md` showed only legacy syntax and falsely claimed existing data isn't checked | Rewrote the CONSTRAINT section faithfully | `ea31f88` |
| L-B | LOW | Violation `Detail` leaked the internal `\x00s\x00…` encoded key | Render the human value | `6fb989a` |
| L-C | LOW | `readVerifiedConstraints` unbounded read + eager `cap=count` pre-alloc | Bounded reader + incremental growth (parity with the indexdefs reader) | `3f5a91a` |
| L-D | LOW | `label.prop` string key aliased dotted label/property pairs | Struct-key the registry maps; removed the fragile split | `cffb39d` |
| L-E | LOW | `HasConstraints` count updated only on defer (a checkpoint fail-safe TOCTOU) | Flip the count inside the CREATE barrier | `205b2cd` |
| L-G | LOW | Idiom nits: loose concurrency doc; `ReseedFromGraph` shadow | Fixed (doc in `6fb989a`, shadow via `cffb39d`); double-encode micro-opt assessed and deferred | — |

Regression coverage added across `cypher/`, `cypher/exec/`, `cypher/ir/`,
`store/snapshot/`, and `store/checkpoint/` (self-set, MERGE release/self-set/
cross-node, identifier cap, `FOR … REQUIRE` + rejected forms, name collision,
declared-name listing, numeric equivalence, backing-index guard, dotted-key
isolation, DROP-not-resurrected, and the failed-DDL crash window).

## Decisions (user-approved)

1. **UNIQUE numeric identity (M-E):** align to canonical value-equivalence, so
   `1` and `1.0` are the same value under a UNIQUE constraint (consistent with
   `=` and MERGE). Exact-value based, hence transitive — the property a value-set
   membership check requires — so integers beyond 2^53 are not folded with their
   float neighbour.
2. **Failed-DDL atomicity (M-A):** fix now via the holistic #1902 approach (the
   checkpoint verifies WAL health before publishing), closing the window for both
   constraint and index DDL rather than deferring.

## The failed-DDL atomicity fix (#1902), in detail

The non-blocking checkpoint captures the constraint/index registry under the
store quiesce boundary (`RunUnderCommitLock`, after in-flight commits drain to
zero). A DDL whose WAL commit fails at fsync poisons the writer *inside*
`SyncGroup`, **before** its in-flight token is released, and discards its frame
(`DurableOffset` excludes it); its compensator runs *outside* the store lock and
can therefore lag the checkpoint's capture. So any interleaving in which the
checkpoint captures the transient registry (a phantom CREATE, or an
un-re-registered DROP) necessarily observes a poisoned writer — the condition is
both necessary and sufficient.

The fix adds `wal.Writer.Poisoned()` (a lock-guarded read of the sticky
`syncErr`) and consults it as the first action of the checkpoint's phase-1
capture: if poisoned, the checkpoint aborts before capturing the registry or
publishing any component. This closes the window for **both** the constraint and
the index DDL paths at the correct layer, and is a no-op on the healthy path, so
the snapshot output is byte-identical when nothing failed. Every prior invariant
(snapshot self-sufficiency before truncation, the snapshot→suffix→truncate fsync
ordering, `TruncatePrefix` crash-safety, the #1464/#1508 fail-safe) is preserved.
Certified by the storage-engine specialist; three deterministic regression tests
(`store/checkpoint/failed_ddl_poison_no_publish_test.go`) inject one fsync
failure, checkpoint the transient registry, reopen via recovery, and assert the
failed DDL left no durable effect — they fail against the unfixed code.

## Scope explicitly not implemented

Relationship constraints, composite (multi-property) constraints, NODE KEY /
relationship key, and property type constraints (`IS :: <TYPE>`) remain out of
scope for this module. They are now **rejected with a specific, actionable
error** rather than a misleading parse failure. `SHOW CONSTRAINTS` / `SHOW
INDEXES` (Neo4j 5) are tracked in the backlog (#1922); the asymmetric
CREATE-CONSTRAINT cost note is #1923.
