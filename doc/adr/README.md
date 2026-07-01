# Architecture Decision Records

This directory holds the Architecture Decision Records (ADRs) for GoGraph.
An ADR captures one architecturally significant decision that is currently
**in force**: what was decided, why, and what it costs. ADRs are not a
change log — they hold only the decision as it stands today. When a
decision changes, the ADR is edited in place to reflect the new state, or
removed if the decision no longer applies; history lives in git, not in
the document.

## Scope

ADRs record decisions and their rationale — *why* something is the way it
is, and *whether it is still open, deferred, or in force*. They deliberately
do not restate design detail that already has a durable, actively
maintained home:

- `docs/*-design.md` — design and architecture specifications (e.g.
  `docs/isolation-design.md`, `docs/columnar-edge-properties-design.md`).
- `docs/algorithms.md`, `docs/cypher.md`, `docs/persistence.md` — subsystem
  reference documentation.
- `docs/benchmarks/` — measured performance evidence.
- `docs/audit-*.md` — dated audit reports.

An ADR may cite any of the above as evidence; it never duplicates their
content.

## Index

| ID | Title | Status |
|----|-------|--------|
| [ADR-0001](adr-0001-opencypher-tck-and-acid-compliance-mandates.md) | openCypher TCK 100% and ACID 100% compliance mandates | Accepted (in force) |
| [ADR-0002](adr-0002-cow-lock-free-read-path-deferred.md) | Copy-on-write lock-free read path | Accepted (deferred, not justified by measurement) |
| [ADR-0003](adr-0003-parallel-checkpoint-component-encode-deferred.md) | Parallel checkpoint component encode | Accepted (deferred) |
| [ADR-0004](adr-0004-leiden-parallelism-deferred.md) | Leiden parallelism | Accepted (deferred) |
| [ADR-0005](adr-0005-query-value-model-column-major-late-materialization.md) | Query value-model direction: column-major, late materialization | Accepted (planned) |

## Numbering and naming

Files follow `adr-XXXX-[slug].md`, where `XXXX` is a zero-padded
sequential number and `[slug]` is a short kebab-case summary of the
subject. Numbers are never reused or renumbered.

## Template

Use [`adr-template.md`](adr-template.md) as the starting point for a new
ADR.
