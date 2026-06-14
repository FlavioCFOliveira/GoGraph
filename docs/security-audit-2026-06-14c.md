# GoGraph Security Audit — 2026-06-14c (Fourth Audit)

This report documents an exhaustive, evidence-based security audit of the
GoGraph module conducted on 2026-06-14 against `main` (HEAD `0f45a8e`,
release line v0.3.x). It is the **fourth** security audit of the module and
builds directly on the three prior audits — 2026-06-14 (findings #1467–#1479,
sprints S181–S183) and 2026-06-14b (findings #1480–#1489, sprints S184–S186),
both fully remediated and merged to `main`.

It follows the five-phase red-team methodology requested for this engagement
(Mapping & Planning → Reconnaissance → Exploitation → Post-Exploitation &
Lateral Movement → Reporting), mapped onto the Cyber Kill Chain / MITRE ATT&CK
model, and is grounded only in verified evidence: every finding cites the
source location, a CWE, and a bounded reproduction; every "verified-solid"
claim names the attack that was attempted and repelled.

All findings are tracked individually in the `gograph` roadmap (`rmp`),
tasks **#1490–#1496**, grouped into sprint **187**. A reproducible security
test battery accompanies this report; see
[Security test battery](#security-test-battery).

> **Remediation status (2026-06-14).** All seven findings have been **fixed**
> on branch `security/sec-2026-06-14c-audit` and merged to `main`. Every
> contained "current-behaviour" demonstration in the battery was converted into
> a strict secure-behaviour regression assertion. The integrated tree is green:
> `go build ./...` clean, the **openCypher TCK holds at 3897/3897**
> (`tckExecutionBaseline` untouched, fidelity baseline 121), `go test -race`
> reports 0 races across all touched packages, `golangci-lint`/`staticcheck`/
> `govulncheck` are clean, and the ACID crash behaviour is preserved (the store
> fix is decode-only). Remediation commits: `73f5d03` (substring/percentileCont/
> replace), `103a557` (Bolt reader panic boundary), `df360f3` (tzdata SHA-256
> pin), on top of the in-cycle `6c9c15b`.

## Engagement scope and rules

- **Target.** The entire GoGraph Go module (~135 kLOC across ~70 packages):
  core graph types, search algorithms, the Cypher engine, the Bolt protocol
  server, the persistence/storage layer, the import/export (serialization)
  layer, and the build/supply-chain pipeline.
- **Threat model.** Cypher query text and parameters are attacker-controlled;
  every byte on the Bolt wire is attacker-controlled (malicious or
  unauthenticated client); every on-disk artifact (WAL, snapshot, `.bak`, CSR,
  index dumps) is untrusted input; every imported file (CSV/GraphML/JSON/JSONL/
  DOT) is untrusted, and every exported file may be re-opened in a third-party
  tool (Excel, Graphviz, Neo4j); third-party dependencies and the CI/release
  pipeline are part of the trust boundary.
- **Rules of engagement.** Read-only on production behaviour; bounded,
  non-destructive reproductions only — no workload was permitted to OOM, hang,
  or crash the host. Zero-day-class issues are demonstrated with contained
  tests (short deadlines, small bounds), never with a live denial of service.
- **Method.** Six specialized auditors ran in parallel, one per attack surface,
  each cross-checking the openCypher / Neo4j / Go ecosystems and the public
  vulnerability corpora (CVE, NVD, OSV.dev, GitHub Security Advisories, CISA
  KEV, the Go vulnerability DB) for relevant attack patterns, then tracing each
  candidate to source before filing.

## Summary of findings

Seven findings were filed. The module is in a strongly hardened state after
three prior audits; the residual findings are concentrated in arithmetic
edge-cases of Cypher built-in functions and in defense-in-depth gaps. **No
Critical finding was found. No prior fix had regressed.**

| # | Severity | CWE | Surface | Title | Status |
|---|----------|-----|---------|-------|--------|
| [#1492](#1492-high--substring-integer-overflow-panic) | **High** | CWE-190 → CWE-129 | Cypher | `substring()` integer-overflow panic on huge length arg | **FIXED** |
| [#1493](#1493-medium--percentilecont-nan-bypasses-01-validation) | Medium | CWE-704 + CWE-129 | Cypher | `percentileCont()` NaN bypasses `[0,1]` validation (platform-dependent index panic) | **FIXED** |
| [#1494](#1494-medium--replace-empty-search-quadratic-amplification) | Medium | CWE-789 | Cypher | `replace(s,'',r)` quadratic output amplification, unbudgeted | **FIXED** |
| [#1490](#1490-low-fixed--txn-proplist-decoder-unclamped-capacity) | Low | CWE-789 | Store | `decodeTxnListProp` unclamped capacity hint (latent OOM) | **FIXED** |
| [#1491](#1491-low--bolt-reader-goroutine-panic-recovery-gap) | Low | CWE-248/755 | Bolt | Bolt reader goroutine has no panic-recovery boundary | **FIXED** |
| [#1495](#1495-low-fixed--stale-goreleaser-dependabot-reference) | Low | CWE-1104 | Supply chain | Stale `.goreleaser.yaml` Dependabot reference | **FIXED** |
| [#1496](#1496-low--tzdata-fixture-script-no-sha-pin) | Low | CWE-494 | Supply chain | `gen_tck_tzdata.sh` downloads tzdata with no SHA-256 pin | **FIXED** |

Two findings discovered with clear, low-risk fixes (#1490, #1495) were
remediated in-cycle together with their regression gates; the remaining five
are filed for scheduled remediation.

## Findings in detail

### #1492 — High — `substring()` integer-overflow panic

- **CWE-190 (integer overflow) → CWE-129 (improper array-index validation).**
- **Location.** `cypher/funcs/essentials.go` `fnSubstring` (~line 1028).
- **Repro.** `RETURN substring('hello', 2, 9223372036854775807)`.
- **Trace.** `end := start + length`, where `start ∈ [0, len(runes)]` and
  `length` is the attacker's `int64`. `start + length` overflows `int64` to a
  **negative** value; the `if end > len(runes)` clamp does not fire (a negative
  is `< len`); `runes[start:end]` then panics with
  `slice bounds out of range [:-9223372036854775805]`. The engine's recover
  boundary (`cypher/api.go convertQueryPanic`) converts it to
  `ErrInternalPanic`, but this violates the project's fail-stop / no-recover-to-
  mask-bugs mandate and is a cheap per-query DoS amplifier (full `debug.Stack()`
  + error log + stack unwind on every malicious query). The conforming Neo4j
  behaviour is to return the truncated tail (`'llo'`).
- **Fix.** Compute `end` overflow-safely:
  `if length > len(runes)-start { end = len(runes) } else { end = start + length }`.

### #1493 — Medium — `percentileCont()` NaN bypasses `[0,1]` validation

- **CWE-704 (incorrect type conversion) + CWE-129.**
- **Location.** `cypher/api.go` `validPercentileParam` (~line 5386);
  `cypher/funcs/aggregators.go` `PercentileContAgg.Result` (~line 604).
- **Repro.** `UNWIND [1,2,3,4,5] AS x RETURN percentileCont(x, 0.0/0.0)`.
- **Trace.** `if f < 0.0 || f > 1.0` is false for `NaN`, so a NaN percentile
  passes validation. In `Result()`, `int(math.Floor(NaN))` / `int(math.Ceil(NaN))`
  index the value slice without re-clamping. `int(NaN)` is
  implementation-defined per the Go spec: on arm64 it yields `0` (observed,
  no crash), on amd64 (CI/production) it yields `MinInt64` → index panic.
  `PercentileDiscAgg.Result` is safe because it clamps the index after the
  conversion; only `percentileCont` indexes unclamped. The codebase already
  uses the correct non-finite guard in `fnToInteger`, so `validPercentileParam`
  is inconsistent with its own convention.
- **Fix.** Reject non-finite `p` in `validPercentileParam` (mirror
  `fnToInteger`) and defensively clamp `lo`/`hi` in `PercentileContAgg.Result`.

### #1494 — Medium — `replace()` empty-search quadratic amplification

- **CWE-789 (memory allocation with excessive size).**
- **Location.** `cypher/funcs/essentials.go` `fnReplace` (~line 1052).
- **Repro.** `RETURN replace($a, '', $b)` with large string parameters.
- **Trace.** `strings.ReplaceAll(s, "", r)` inserts `r` between every rune, so
  the output size is `(len(s)+1)*len(r)` — measured ~8000× amplification
  (2×16 KiB inputs → 268 MB output). The per-evaluation string-byte budget
  (`expr.DefaultMaxStringEvalBytes`) is not charged because built-in funcs run
  via the generic `evalFunction` dispatch, and **parameters bypass the parser's
  1 MiB query-text guard** (there is no parameter-size cap at the engine
  boundary), so two ~1 MB params attempt a ~1 TB allocation → OOM-kill.
- **Fix.** Compute the worst-case output byte count overflow-safely in
  `fnReplace` and return a typed `NumberOutOfRange` `EvalError`
  (cap = `DefaultMaxStringEvalBytes`) before calling `strings.ReplaceAll`.
  Charging the per-evaluation budget on the generic funcs dispatch path, and
  adding an engine-boundary parameter-size cap, are recommended companion
  hardenings.

### #1490 — Low (FIXED) — txn PropList decoder unclamped capacity

- **CWE-789.** `store/txn/txn.go decodeTxnListProp` did
  `make([]lpg.PropertyValue, 0, count)` on an unclamped untrusted `uint32`
  (`count = 2^32-1` reserves ~103 GiB). **Latent**: this canonical encode/decode
  pair is currently referenced only by a round-trip test helper; the live
  WAL-replay path uses recovery's already-hardened `decodeRecoveryListProp`. It
  is the third PropList decoder that diverged from its two clamped siblings —
  exactly the "fix-not-generalized" pattern flagged by audit 2026-06-14b.
- **Fixed in-cycle** by adding `txnListCapHint(count, remaining) =
  min(count, remaining/5)`, mirroring `recovery.recoveryListCapHint` and
  `snapshot.listCapHint`. Reusable insight: GoGraph has **three parallel
  PropList decoders** that must stay lockstep on this clamp — audit all three
  together on any list/nested-value change.

### #1491 — Low — Bolt reader goroutine panic-recovery gap

- **CWE-248/CWE-755.** `handleConn` installs a `defer/recover` panic boundary,
  but the separate reader goroutine it spawns (looping on `cr.ReadMessage`) does
  not. Not presently reachable (the read/framing path is panic-free on
  adversarial bytes — verified), but an asymmetric defense gap: a future
  panic-on-input bug there would crash the whole process instead of the single
  connection, violating the never-crash mandate the handler goroutine already
  honours.
- **Fix.** Mirror the recover at the top of the reader closure
  (log + `metricConnPanics` + `cancelConn` + return, keeping `close(readerDone)`
  as the first deferred call).

### #1495 — Low (FIXED) — stale `.goreleaser.yaml` Dependabot reference

- **CWE-1104 (use of unmaintained third-party components / process-doc
  integrity).** A `.goreleaser.yaml` comment instructed maintainers to bump the
  `cyclonedx-gomod` SBOM-generator pin via a `.github/dependabot.yml` that had
  been deliberately removed (commit `28d3c20`); pins are in fact maintained
  manually. A maintainer trusting the stale comment could leave the pin stale.
- **Fixed in-cycle** by correcting the comment and adding a regression gate
  (`internal/scriptgate/supplychain_gate_test.go`).

### #1496 — Low — tzdata fixture script no SHA pin

- **CWE-494 (download of code without integrity check).**
  `scripts/gen_tck_tzdata.sh` downloads the IANA tzdata tarball with only a
  single canary-offset self-check, no SHA-256 pin. Dev-only fixture-regeneration
  script, not on the release or PR-CI path.
- **Fix.** Mirror the `scripts/install-antlr.sh` SHA-256-pin pattern.

## Surfaces verified solid

The bulk of the engagement confirmed that prior hardening holds. The following
attack classes were actively attempted and repelled (evidence in the battery):

- **Cypher.** Parser guard (1 MiB byte cap, 256 bracket-nesting, 256 CASE, 512
  binary-op) closes the stack-overflow DoS class; `=~` regex anchoring (#1479)
  intact and RE2 precludes ReDoS; integer arithmetic overflow-detected;
  `range()`, `toInteger`/`toFloat` (NaN/Inf/overflow), list slicing/subscript,
  and the per-evaluation list/string budgets (#1475/#1477/#1482) all bounded.
- **Bolt.** PackStream length-bombs rejected at the header before any `make()`
  (per-message wire-byte budget + 32-bit `MaxInt32` guard); recursion capped at
  128 (`ErrNestingTooDeep`); 128 MiB cumulative decoded-memory budget; auth is
  timing-safe (`subtle.ConstantTimeCompare`) and secure-by-default; the
  RESET-from-pre-auth bypass (#1345) holds at both the state-machine and session
  layers; Bolt 5.1+ deferred LOGON (#1470) correct; `"none"` scheme rejected by
  a credentialed handler; tx_timeout/timeout overflow (#1484) safe; connection
  caps, deadlines, and goleak coverage in place; error messages sanitized
  (no info leak); CVE-2025-11602 (Neo4j handshake leak) does not apply.
- **Store.** Every binary decoder (btree #1480, label #1481, snapshot
  properties/labels/mapper/edgehandles/constraints, PropList depth #1488, WAL
  frame, csrfile) bounds its allocation against remaining input and a sane cap;
  csrfile `Header.validate` makes every `unsafe.Slice` in-bounds by construction
  with 128-bit overflow guards; recovery promotion `parentDirFsync` (#1454) and
  snapshot publish ordering (#1331) preserve atomicity/durability; file
  permissions are `0o600`/`0o750` with `O_EXCL` WAL lock.
- **Import/export.** GraphML repels XXE (CWE-611) and billion-laughs (CWE-776)
  — Go `encoding/xml` resolves no external entities; 128 MiB CSV/GraphML caps
  (#1436), 16 MiB JSONL line cap (#1442), JSON 10 000-level nesting cap;
  well-formed nested-list recursion bounded by the byte caps; DOT id/label
  injection neutralized by `quote()` + reserved-keyword quoting (#1489). CSV
  formula injection (CWE-1236) remains an explicit, documented off-by-default
  trade-off (`Options.SanitizeFormulae`), unchanged.
- **Concurrency / ACID.** No provable data race, ACID isolation/atomicity hole,
  deadlock, or unbounded-resource path beyond documented caller-supplied budgets
  (`-race` green across `graph/…`, `store/…`, `cypher/…`, `search/…`,
  `ds/…`, `internal/metrics`); the `nowAwareRegistry`/`Graph.View`/
  `ApplyAtomically` barrier (3b22734) holds; `search/*` traversals are iterative
  and ctx-aware; metric names are compile-time constants (no attacker-driven
  registry growth).
- **Supply chain.** `govulncheck ./...` reports no vulnerabilities; toolchain
  `go1.26.4` still patched; all 10 direct dependencies free of reachable CVEs;
  every CI third-party action SHA-pinned with minimal `permissions:`, no
  `pull_request_target`, no untrusted interpolation into `run:` steps;
  `crypto/rand` used for Bolt session ids; no `math/rand` in any security path;
  no hardcoded secrets; `os/exec` only in test harnesses with no user input.

## Security test battery

The battery added by this audit (all bounded, short-layer, `-race`-clean):

- `cypher/security_audit2026c_test.go` — substring overflow, percentileCont
  NaN (portable aggregator lock-in), percentileDisc safe lock-in, replace
  amplification bound.
- `cypher/security_audit2026c_concurrency_test.go` — concurrent write/read
  no-race + no-partial-read (ACID atomicity) lock-in; single-writer
  serialisation lock-in.
- `bolt/server/security_audit_2026_06_14c_test.go` — 8 wire-level gates
  (allocation bombs, deep nesting, decoded-memory amplification,
  RUN-before-auth, RESET-pre-auth, `"none"`-scheme rejection, unknown-tag
  clean-fail, NOOP discard).
- `store/txn/security_listcap_test.go` — hostile `0xFFFFFFFF` count bounded,
  clamp arithmetic table, legitimate round-trip.
- `graph/io/jsonl/security_depth_test.go` — well-formed nested-list recursion
  bound (the gap the prior malformed-bracket test left).
- `internal/scriptgate/supplychain_gate_test.go` — SHA-pin discipline,
  cyclonedx exact-version, no stale Dependabot reference, Bolt `crypto/rand`
  lock-in.

For the open findings (#1492/#1493/#1494/#1491/#1496) the battery tests assert
the current containment without hanging or OOMing the runner, and carry
`// FLIP:` notes marking the assertion to switch to the conforming/secure
behaviour once each is remediated.

## Gate status (audit tree)

- `go build ./...` clean.
- New battery tests pass under `go test -race` (cypher, bolt/server, store/txn,
  graph/io/jsonl, internal/scriptgate).
- **openCypher TCK holds at 3897/3897** (`cypher/tck` green; `tckExecutionBaseline`
  untouched).
- `govulncheck ./...` clean; `gofmt`/`go vet` clean on touched packages.
- ACID crash-injection behaviour unchanged (the in-cycle store fix is
  decode-only; no write-path/fsync mutation).

## Remediation status

**All seven findings are fixed and merged to `main`.** Two (#1490, #1495) were
fixed in-cycle during the audit; the remaining five (#1492 High, #1493/#1494
Medium, #1491/#1496 Low) were remediated immediately after, with every battery
test flipped from a contained "current-behaviour" check to a strict
secure-behaviour regression assertion. The fixes:

- **#1492** — `fnSubstring` end bound computed overflow-safely; returns the
  conforming truncated tail, no panic, no error.
- **#1493** — `validPercentileParam` rejects non-finite `p` (typed
  `NumberOutOfRange`); `PercentileContAgg.Result` clamps the index like the
  discrete aggregator.
- **#1494** — `fnReplace` computes the worst-case output size overflow-safely
  and returns a typed `NumberOutOfRange` budget error before allocating.
- **#1491** — the Bolt reader goroutine now carries a `defer/recover` boundary
  mirroring `handleConn` (log + panic metric + connection cancel), with
  `close(readerDone)` preserved as the first deferred call.
- **#1496** — `gen_tck_tzdata.sh` SHA-256-pins the IANA tzdata tarball and
  aborts on mismatch or an unpinned version.

Gate on the integrated tree: `go build ./...` clean; openCypher **TCK 3897/3897**
(`tckExecutionBaseline` untouched); `go test -race` 0 races on all touched
packages; `golangci-lint`/`staticcheck`/`govulncheck` clean; ACID crash
behaviour preserved. Both compliance mandates hold.
