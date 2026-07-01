# Production-readiness audit under extreme hostility and load — 2026-07-01

**Scope.** A full-team, evidence-based audit of the **GoGraph module** (only the
module — the `examples/` tree is an exerciser, not a deliverable) to determine
what remained before the module can operate in production under **extreme
hostility** (adversarial/untrusted network clients, query text, import files, and
store files; resource-exhaustion / DoS) and **extreme load** (sustained high
concurrency, memory pressure, saturation), across three axes: **functional
completeness, performance, and security**.

**Method.** Eleven specialist agents ran in two phases. Phase 1: six domain
auditors (Cypher functional/robustness, ACID+durability vs adversarial disk,
graph-algorithm correctness+complexity, concurrency+resource bounds, performance
under load, and a security review of every untrusted-input boundary) each
produced structured findings with a concrete `file:line` and a reproducible
failure scenario. Phase 2: each CRITICAL/HIGH finding was handed to an
independent adversarial verifier instructed to **refute** it against the actual
code and the openCypher TCK / WAL-recovery sources. Only findings that survived
verification were remediated. The two compliance mandates were treated as
inviolable throughout: **100% openCypher TCK (3897 scenarios)** and **100% ACID**.

## Verdict

**Ready for production.** Five of the six domains were assessed
PRODUCTION-READY on entry (the module has already passed six reliability rounds
plus performance, multithread, and security audits). The residual gaps — three
HIGH-severity DoS/OOM vectors reachable from untrusted input, plus a set of
TCK-neutral robustness and functional-completeness items — have all been fixed,
each with a regression test, with the TCK held at 100.0% / 3897 and the full
`-race`/lint/staticcheck/govulncheck gate green.

## Findings and remediation (rmp sprint 257, #1828–#1837)

| # | Sev | Domain | Finding | Fix | Commit |
|---|-----|--------|---------|-----|--------|
| F1 (#1828) | HIGH | Cypher / Bolt | Autocommit RUN had **no default statement-timeout floor**; a default-configured server let an authenticated client pin a CPU core indefinitely with a super-linear-runtime, single-output-row query (disconnected Cartesian product whose result-row/byte caps never fire). | Mandatory `Options.DefaultStatementTimeout` (30 s, operator-overridable) applied to autocommit via `resolveStmtTimeout`, mirroring the explicit-tx `DefaultTxTimeout` floor. A follow-up (`1ce2b11`) keeps the deadline armed across the RUN→PULL boundary — an autocommit write/DDL commits during the PULL drain — by firing the cancel from the cursor-close funnel instead of at RUN return. | `9cf6b79`, `1ce2b11` |
| F2 (#1829) | HIGH | Storage / ACID | `edgehandles.bin` reader allocated the per-record property map from a raw untrusted `propCount` (bounded only to 1<<40); `propCount=MaxUint32` forced a tens-of-GiB eager map allocation → OOM at recovery from a hostile/corrupt snapshot. | Clamp the map hint with `capHint(propCount, edgeHandlesCapHintMax)`, restoring the package-wide discipline; truncated body now fails fast with `ErrEdgeHandlesCorrupted`. | `1b859f1` |
| F3 (#1830) | HIGH | Concurrency / read path | `ParallelScanProject` materialised the **entire** result set before the first row reached the drain, so `MaxResultBytes/MaxResultRows` could not bound peak memory on the parallel path — a memory-DoS reachable by default over any graph past the 50k-node threshold. | Thread the engine budget into the operator via `WithResultBudget`; workers stop accumulating (shared atomics) at the budget, emitting a bounded prefix from which the drain produces the canonical cap error. | `7496423` |
| F4 (#1831) | MEDIUM¹ | Cypher parser | The pre-parse operator guard excluded `-`/`*`, so a byte-tight arithmetic chain (`1-1-…-1`, ~500k ops) bypassed it and forced an uninterruptible ~0.9 s / ~1.2 GB parse+visitor pass before the sema depth backstop fired. | Count arithmetic-context `-`/`*` (operand-adjacency; `*` gated outside `[...]`/digit-left) so no relationship/VLE token is ever miscounted — TCK-neutral by construction. | `94bad71` |
| F5 (#1832) | MEDIUM | Cypher functions | Common openCypher functions absent (fail-stop `UnknownFunction`): `elementId`, `timestamp`, `randomUUID`, `isNaN`, `toStringList`/`toIntegerList`/`toFloatList`/`toBooleanList`. | Registered all, under openCypher 9 semantics; `timestamp()` statement-frozen via the per-query now-aware registry; `randomUUID()` crypto/rand v4. TCK-neutral. | `36bd181` |
| F8 (#1833) | MEDIUM | Security / storage | `manifest.json` decoded with no byte ceiling — the one untrusted store-file decode lacking the `DefaultMaxBytes` bound every sibling loader has. | Wrap the file-backed reader in a `manifestLimitReader` capped at `DefaultMaxManifestBytes` (32 MiB); typed `ErrManifestTooLarge`. | `08e7aa0` |
| F10 (#1834) | LOW | Algorithms | MCMF Bellman-Ford potential bootstrap ran O(V·E) with no `ctx` poll → cancellation latency on a large dense negative-cost network. | Thread `ctx` into `bellmanFordBootstrap`; poll at each relaxation-pass boundary. | `1b23887` |
| F11 (#1835) | LOW | Cypher functions | `coalesce()` with zero arguments returned null (fail-open) instead of the openCypher-mandated arity error. | Return a typed `ArityError` for zero args. TCK-neutral. | `23bfc21` |
| F7 (#1836) | MEDIUM | Performance / isolation | A write `ExplicitTx` holds the engine-wide visibility barrier for its whole lifetime across round-trips → head-of-line blocking of all transactional readers; the tail was uncharacterised. | Documented the operational contract on the `ExplicitTx` godoc; added `BenchmarkReaderLatencyUnderHeldWriteTx`. Isolation unchanged (the copy-on-write cure is the deferred #1671 epic). | `78a4c3b` |
| Cleanup (#1837) | — | — | staticcheck U1000 on `estimateRecordSize` was a **false positive** caused by an audit-agent scratch file breaking test-binary compilation. | No code change; removed the scratch file. `staticcheck ./cypher/...` clean. | — |

¹ F4 was reported HIGH by the Cypher auditor; the adversarial verifier reproduced
the mechanism but measured the true cost at ~0.9 s CPU / ~1.2 GB transient (≈95×
lower than the auditor's estimate) with no crash/hang/corruption, so it was
downgraded to MEDIUM. The fix was applied regardless because it is cheap and
TCK-neutral.

### Findings refuted by adversarial verification (no action)

- **Performance HIGH — "the mandated 1/8/64/256/1024-goroutine load benchmark only
  exercises `count(n)`".** Refuted: `bench/soak/bolt_soak_test.go` drives
  `MATCH (n:Person) RETURN n` over the real Bolt wire path under concurrency, and
  `bench/cypher_ldbc` runs parallel row-returning/property-projection benchmarks.
  The claim was false; and `bench/` is measurement tooling, not the shipped
  module (analogous to `examples/`).

## Domain verdicts (post-remediation)

- **Graph algorithms** — PRODUCTION-READY. Full defensive envelope on degenerate
  graphs, documented complexity bounds, ctx-cancellation throughout; the sole
  item was the LOW F10, now fixed.
- **Security (untrusted-input boundaries)** — PRODUCTION-READY. Every boundary
  (packstream, Bolt chunking/handshake/idle, parser pre-parse guard, IO
  importers, WAL/csrfile loaders) was already hardened; F8 closed the one
  remaining unbounded store-file decode.
- **Concurrency / resource bounds** — PRODUCTION-READY after F3. Bolt server
  bounds connections/deadlines/in-flight, goroutine spawn is cores-bounded and
  goleak-covered, the checkpoint loop cannot block permanently.
- **Storage / ACID durability** — CERTIFIED after F2. WAL CRC + torn-frame
  detection, 3-phase non-blocking checkpoint, atomic WAL truncation, bounds-checked
  on-disk formats.
- **Cypher (functional + query robustness)** — PRODUCTION-HARDENED after F1/F4;
  functional surface widened by F5; F11 tightened arity.
- **Performance / graceful degradation** — PRODUCTION-READY; the write-tx reader
  tail is now documented and benchmarked (F7).

## Gate evidence

- `go build ./...`, `go vet ./...`, `gofmt`/`goimports`: clean.
- `go test -race ./...` (all packages, includes the ACID crash-injection battery
  and WAL-recovery tests): 0 races.
- openCypher TCK: **100.0% / 3897**.
- `golangci-lint run ./...`, `staticcheck ./...`: clean.
- `govulncheck ./...`: no vulnerabilities.
- coverage gate (`scripts/cover_gate.sh`): OK — aggregate **86.4%** ≥ 85%, every package ≥ 75%.
- `scripts/pre-release.sh`: PASSED 5/5.

Every fix carries its own regression test, so each gate above is also that
finding's guard.
