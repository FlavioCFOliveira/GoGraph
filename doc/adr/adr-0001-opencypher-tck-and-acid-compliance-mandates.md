# ADR-0001: openCypher TCK 100% and ACID 100% compliance mandates

## Status

Accepted (in force)

## Context

GoGraph is a graph persistence, manipulation, and search module. Two
properties are treated as non-negotiable invariants of the module,
independently of any individual feature, refactor, or performance change:
query-language conformance and transactional reliability. Until now these
mandates existed only as prose in `CLAUDE.md` ("Compliance Mandates"); they
had no durable, project-scoped architectural record independent of that
operating file.

Two forces motivate treating both as *architectural* decisions rather than
ordinary test coverage:

- The module's value proposition to a caller is that it behaves like a
  real openCypher-conformant graph database, not an approximation of one.
  Silent semantic drift from the specification would be indistinguishable
  from a bug to any caller that relies on standard Cypher behaviour.
- The module persists data. A graph engine that loses or corrupts
  committed data under crash, concurrency, or partial failure is
  categorically unfit regardless of how fast or how Cypher-conformant it
  is otherwise.

## Decision

GoGraph maintains, as a permanent architectural invariant:

1. **100% openCypher TCK conformance at the execution level.** Every
   scenario in the openCypher Technology Compatibility Kit
   (`cypher/tck/features/`) must pass, with no `failed`, `undefined`, or
   `pending` steps. The regression gate is a hard-coded baseline,
   `tckExecutionBaseline`, in `cypher/tck/runner_test.go` (currently
   `3897`, the full scenario count). The test asserts
   `passed >= tckExecutionBaseline` and fails the build otherwise
   (`cypher/tck/runner_test.go:2030`, `:2136`). No pull request may lower
   this baseline; raising it (as scenario coverage grows) is the only
   permitted direction of change.
2. **100% ACID compliance** — Atomicity, Consistency, Isolation, and
   Durability — across the in-memory engine and every persistence
   backend. Atomicity and Durability are verified empirically, not by
   code inspection alone, via:
   - a deterministic, subprocess-based crash-injection harness
     (`internal/crashinject/`), which spawns a child process, drives it to
     a precisely chosen execution point, and delivers `SIGKILL` at that
     point so the write path is tested against a genuine abrupt
     termination rather than a simulated one;
   - the write-ahead log package (`store/wal/`), which owns frame format,
     CRC validation, fsync ordering, and injected filesystem faults
     (partial write, ENOSPC, read-only, torn frames);
   - the recovery package (`store/recovery/`), which replays the WAL and
     snapshot state after a simulated crash and asserts the recovered
     graph is indistinguishable from the pre-crash committed state
     (`store/recovery/crash_injection_test.go`,
     `store/recovery/checkpoint_crashinject_test.go`, and the broader
     `*_crashinject_test.go` and `*_test.go` battery in that package).

Any code path that could compromise an ACID property — a non-atomic
multi-step write, a read that could observe partial state, a commit that
does not durably flush — is rejected at review and must not merge,
regardless of the performance gain it offers.

## Rationale

Encoding the TCK baseline as an executable, numeric regression gate rather
than a prose aspiration makes conformance self-enforcing: a regression is a
CI failure, not something that must be remembered or re-litigated per
change. The crash-injection design (real `SIGKILL` on a real subprocess,
not a mocked failure) was chosen over synthetic fault simulation alone
because it exercises the actual kernel/process boundary where durability
guarantees are made or broken; the WAL fault-injection and recovery-replay
tests complement it by covering the filesystem-level failure modes that a
single-process crash cannot exhaustively express.

## Consequences

- Every feature, refactor, and performance optimization in the module is
  constrained to preserve both properties; a change that regresses either
  is rejected regardless of its other merits. This includes deferred
  performance work — see ADR-0002, ADR-0003, and ADR-0004, each of which
  cites TCK-3897 and/or the crash battery as a condition of the deferred
  approach eventually being accepted.
- New Cypher-adjacent features not covered by the TCK are permitted only
  when they do not conflict with TCK-covered semantics — the TCK is the
  ceiling on divergence, not just a floor on coverage.
- The performance cost of ACID durability (fsync ordering, WAL framing,
  group commit) is treated as a fixed cost of correctness, not a tuning
  knob; performance work targets the parts of the write and read paths
  that do not trade away durability or isolation.

## Evidence / links

- `cypher/tck/runner_test.go:18-21`, `:2030`, `:2049-2052`, `:2136` —
  `tckExecutionBaseline` definition, rationale comment, and enforcement.
- `cypher/tck/features/` — the full openCypher TCK scenario corpus.
- `internal/crashinject/crashinject.go:1-17` — package doc describing the
  parent/child `SIGKILL` crash-injection architecture.
- `store/wal/` — frame format (`FORMAT.md`), CRC and fsync fault-injection
  tests (`crc_corruption_test.go`, `fs_fault_*_test.go`).
- `store/recovery/` — WAL replay and crash-recovery battery
  (`crash_injection_test.go`, `checkpoint_crashinject_test.go`,
  `snapshot_promote_crashinject_test.go`, and related files).
- `docs/acid-audit.md` — ACID property audit and evidence trail.
- `CLAUDE.md`, "Compliance Mandates" section — the originating operating
  rule this ADR gives a durable architectural home to.
