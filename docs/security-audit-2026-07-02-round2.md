# GoGraph Security Assessment & Remediation — 2026-07-02 (Round 2, 6th engagement)

This report documents a second, independent whole-module security assessment of
GoGraph conducted on 2026-07-02 against `main` (baseline `f506791`), and the full
remediation of every confirmed finding. It is the **sixth** security engagement
on the module, following the three rounds of 2026-06-14 (findings #1467–#1496)
and the first 2026-07-02 round (findings #1843–#1848, report
`docs/security-audit-2026-07-02.md`).

Unlike a re-run, this round was explicitly adversarial about the *prior* round's
fixes: each reviewer was tasked to (1) probe the #1843–#1848 remediations for
**bypass** and (2) hunt for **new** vulnerabilities the five prior engagements
missed. Every prior fix was re-verified holding; five new findings were filed
and all are fixed.

> **Remediation status.** All confirmed findings are **FIXED** on `main` (rmp
> roadmap `gograph`, sprint **260**, tasks **#1849–#1852**). The integrated tree
> is green: `go build ./...` clean, `go test ./...` passes across all packages,
> `go test -race ./...` reports no data race, the **openCypher TCK holds at
> 3897/3897** (`tckExecutionBaseline` untouched), `gofmt`/`go vet` clean, and
> `govulncheck` reports no known vulnerabilities. Each remediation was certified
> by the relevant specialist. Commits: `1eaaa47` (#1850), `be9f769` +`088d270`
> (#1849), `60043cc` (#1852), `5e7b41a` +`d7ba578` (#1851) — the two `+` commits
> are certification follow-ups (see below). Not pushed.

## Scope, threat model, and method

- **Target.** The entire GoGraph Go module: core graph types, search algorithms,
  the Cypher engine, the Bolt protocol server, the persistence/storage layer,
  and the import/export (serialization) layer.
- **Threat model.** Every byte on the Bolt wire is attacker-controlled (a
  malicious or unauthenticated client); every Cypher query and parameter is
  attacker-controlled; every on-disk artifact (WAL, snapshot, CSR, index dumps,
  manifest) is untrusted input, including a store directory adopted from an
  untrusted source (a tampered backup or a shared filesystem); every imported
  file (CSV/GraphML/JSONL) is untrusted.
- **Method.** `govulncheck` plus **five parallel adversarial reviewers**, one per
  attack surface (Bolt network + packstream, persistence/deserialization, I/O
  parsers, Cypher engine/procedures, cross-cutting crypto/secrets/concurrency).
  Every finding was traced to source and independently confirmed; the memory
  amplifications were reproduced empirically. Each remediation was then
  **certified by the relevant specialist**: the snapshot fix by the
  storage-engine-auditor, the packstream fix by the concurrency-architect, the
  Cypher fix by the cypher specialist, and the GraphML rewrite by the
  go-developer.

## Summary of findings

No Critical, no High. **Every finding is the same root class: a memory-exhaustion
denial of service (CWE-770 / CWE-789 / CWE-1284) driven by an allocation bounded
by an attacker-controlled count or cost that is not tied to the real input
size.** None involve remote code execution, memory corruption, data disclosure,
injection, or an ACID violation — the confidentiality and integrity posture is
sound; the residual risk this round closed is availability under hostile input.

| # | Severity | CWE | Surface | Title | Commit |
|---|----------|-----|---------|-------|--------|
| [#1849](#1849) | Medium | CWE-789/770 | Bolt/packstream | Decoded-memory cost model under-counts Go map allocation ~3.5× (pre-auth); aggregate ceiling omits raw payloads | `be9f769` |
| [#1850](#1850) | Medium | CWE-789/770 | Store/snapshot | CSR reader allocates from the untrusted manifest `Size` → ~128 GiB `make()` / OOM on recovery | `1eaaa47` |
| [#1851](#1851) | Medium | CWE-789/1284 | I/O GraphML | Full-DOM decode → struct-per-element amplification (2.2–3.4 GiB from a 128 MiB file) | `5e7b41a` |
| [#1852](#1852) | Medium | CWE-770/789 | Cypher | `range()` bypasses the per-evaluation budget; multi-column compounding → OOM | `60043cc` |

The Bolt aggregate-ceiling gap (raw String/Bytes payloads and the reassembly
buffer not counted against the engine-wide budget — originally a separate Low
observation) was folded into the #1849 fix.

## Findings and remediation

### <a name="1849"></a>#1849 — Medium — Bolt decoded-memory cost model under-counts maps (CWE-789/770)

- **Vulnerability.** `bolt/packstream/decoder.go` charged `mapEntryCost = 48` per
  map entry against both the per-message decoded-memory budget (128 MiB) and the
  engine-wide `InboundBudget` (#1845), on the premise — stated in a code comment
  as "conservative lower bounds … so the budget can never under-count" — that
  map entries pack densely. Go allocates a whole ~272-byte hash bucket on the
  first insert, so a **1-entry `map[string]Value` costs ~344–352 B, not the 80 B
  charged (a ~4.3× under-count)**. A crafted, structurally-valid **pre-auth**
  HELLO whose `Extra` map is a list of tiny 1-entry maps was reproduced forcing
  **~403 MiB of live heap from a ~3.43 MiB wire message** (~117× amplification)
  while the decoder believed it had charged ~110 MiB — breaching both the
  documented per-message contract and the #1845 aggregate ceiling by ~3.5×.
  Lists of boxed scalars under-counted by the same mechanism (16 B/elem charged
  vs ~24 B real). Separately, the #1845 ceiling covered only decoded
  *collections*, not raw String/Bytes payloads, so the aggregate it advertised
  did not, in fact, bound total pre-auth decode memory.
- **Fix.** The slot costs are now **upper bounds on Go's real allocation**,
  empirically validated against `runtime.MemStats`: a map charges
  `mapCollectionCost = 512` (hmap + first bucket) + `mapEntryCost = 112`
  (worst-case per-entry bucket growth incl. size-class rounding, measured ~90
  B/entry); a list slot charges `listElemCost = 32` (interface slot + boxed
  value). `chargeDecoded` takes a per-collection base cost. `ReadString`/
  `ReadBytes` now charge the raw payload length against the shared
  `InboundBudget`, so the aggregate pre-auth ceiling truly bounds total decode
  memory. All changes are no-ops when no shared budget is attached (no
  GOMEMLIMIT), so the default path is behaviour-preserving apart from the
  corrected, larger per-message charge. New `charge_upperbound_alloc_test.go`
  (gated `//go:build !race`, because the race detector inflates allocation)
  proves `charge ≥ real allocation` across sizes; the crafted list-of-tiny-maps
  vector is now rejected with `ErrDecodedMemoryExceeded`. **Certification
  follow-up (`088d270`):** the concurrency-architect found a residual of the same
  class — `listElemCost = 32` under-counted a `[]byte`-typed list/struct element
  by ~1.25× (boxing a `[]byte` into a `Value` allocates a 24-byte slice header,
  not the 16 B budgeted), so a pre-auth List of Bytes reached ~160 MiB against
  the 128 MiB per-message contract. `listElemCost` was raised to 48 (16 slot +
  24 slice header + 8 margin), a verified upper bound for every element type at
  every n, and the self-guard test extended to string- and `[]byte`-element
  lists. **Certified GO by the concurrency-architect** (upper-bound soundness
  incl. the Bytes shape; balanced aggregate accounting, no leak/double-release/
  deadlock; pre-auth + aggregate coverage).

### <a name="1850"></a>#1850 — Medium — Snapshot CSR reader trusts the manifest `Size` (CWE-789/770)

- **Vulnerability.** `store/snapshot`'s `readCSRLimited` is the only component
  reader that does a full up-front `make()` sized to the declared header count;
  its sole guard, `recordCap`, was derived from the manifest `FileEntry.Size` —
  an attacker-controlled JSON field never cross-checked against the real
  on-disk file size. A poisoned store directory whose manifest sets `"size":0`
  (or an inflated value) collapsed `recordCap` to the backstop
  `maxCSRCount = 1<<34`, so an 18-byte `csr.bin` declaring `nVertices = 1<<34`
  drove a **~128 GiB eager `make()` and an out-of-memory fatal crash on
  recovery**, before any CRC check. Reproduced end-to-end via `LoadSnapshotFull`.
- **Fix.** A new `safeCSRAllocBound(fsys, path, manifestSize)` obtains the real
  file size via `fsys.Stat` and passes `min(manifestSize, realSize)` (the real
  size when the manifest size is non-positive) as the allocation bound to
  `readCSRLimited`, at both the recovery path (`readVerifiedCSR`) and the legacy
  `Open` path (`openWith`). The real size is a fact of the bytes on disk and
  cannot be inflated by the manifest; content integrity remains CRC-guarded, and
  a legitimate snapshot (manifest size == real size) loads byte-identically. The
  static-poisoned-dir threat model carries no meaningful stat/open TOCTOU. New
  `csr_lying_manifest_size_test.go` proves both the `size:0` and inflated-size
  vectors are rejected with `ErrCSRCorrupted` and no giant allocation; the valid
  round-trip tests still load. **Certified GO by the storage-engine-auditor**
  (OOM closed on both paths; durability/atomicity/crash-safety untouched; on-disk
  format unchanged).

### <a name="1851"></a>#1851 — Medium — GraphML full-DOM element amplification (CWE-789/1284)

- **Vulnerability.** `graph/io/graphml` decoded the whole document into a
  struct-per-element DOM (`dec.Decode(&doc)`) before folding it into the graph.
  The 128 MiB byte cap bounds *input bytes* but not *per-element metadata*, so a
  legitimate ~128 MiB file of millions of tiny `<edge/>`/`<node/>`/`<key/>` tags
  was reproduced forcing **2.2–3.4 GiB of transient heap (10–27×)** — the exact
  class #1844 closed for CSV, left open for GraphML, and contradicting the
  package's own "well under 1 GiB" memory doc.
- **Fix (structural).** Both read entry families (`ReadInto*` and
  `ReadWithProps*`) now **stream** through a shared `dec.Token()`/
  `dec.DecodeElement` loop that folds each `<node>`/`<edge>` into the graph as it
  is decoded — peak now tracks the output graph, not a DOM — and **caps `<key>`
  declarations at 65536** (`ErrTooManyKeys`), since the schema produces no
  proportional graph output and is the one element that could otherwise grow
  unbounded. The fold logic (key index, `<data>` typed-property deserialisation,
  the #1793 node-label restore, weight handling, first-`<graph>`-only, directed
  default) is preserved verbatim; the root is pinned to `<graphml>`; the input
  byte cap is still enforced over the whole document (the remainder is consumed
  in O(1) memory after the first graph). Import stays all-or-nothing. The dead
  DOM structs are removed and the false peak-memory doc corrected. New
  `security_dom_amplification_test.go` pins the `<key>`-flood rejection and
  streaming correctness at 300k nodes+edges; every existing GraphML test
  (round-trip, typed props, hetero-key, labels, XXE/entity, byte cap) passes
  unchanged. **Certified GO by the go-developer** (fidelity preserved — logical
  import byte-identical — robust, idiomatic); a certification follow-up
  (`d7ba578`) corrected a comment that mischaracterised the prior reader's
  handling of a spec-violating post-`<graph>` `<key>`.

### <a name="1852"></a>#1852 — Medium — Cypher `range()` bypasses the per-evaluation budget (CWE-770/789)

- **Vulnerability.** `range()` was bounded only by its own `maxRangeElements = 1e8`
  cap — 10× the shared per-evaluation list-element budget
  (`DefaultMaxListElements = 1e7`) — and, being dispatched through the generic
  function registry, was never charged against that shared budget. A single
  `RETURN range(1, 1e8)` was reproduced allocating ~2.4 GB, and a multi-column
  query (`RETURN range(1,1e7), range(1,1e7), …`) compounded to tens of GB in one
  output row, because each column installs a fresh budget and the result-memory
  ceiling charges only *after* a row is fully materialised.
- **Fix (three layers, all TCK-neutral).** (1) `evalFunction` now charges a
  function's returned list against the per-evaluation budget, bounding one column
  expression; (2) `maxRangeElements` is lowered `1e8 → 1e7` to match
  `DefaultMaxListElements`, bounding a single call; (3) `exec.Project` enforces an
  **incremental per-row byte budget** — reusing the engine's `MaxResultBytes`
  ceiling and the allocation-free `estimateValueSize` deep-counter — that rejects
  a row *before* it is fully materialised, so columns that each fit but whose sum
  does not are caught (`ErrProjectionRowTooLarge`), bounding the transient peak to
  the ceiling plus one column regardless of column count. The largest range/
  function list in the entire openCypher TCK is 1,000,001 elements (five orders of
  magnitude below 1e7), and every conforming result already fits the default 1 GiB
  per-result budget, so no conforming query trips either bound; **TCK holds at
  3897 before and after**. New `security_cypher_range_budget_test.go` pins the
  single-oversized and multi-column rejections and the largest-TCK-range positive
  control. **Certified TCK-neutral by the cypher specialist** (who also verified
  that sharing the per-column budget would have given zero peak advantage — the
  incremental per-row guard is the correct fix).

## Verified sound (re-confirmed under adversarial review)

- **Prior fixes hold.** No #1843–#1848 remediation regressed or was bypassable:
  the WAL/snapshot `O_NOFOLLOW` hardening is comprehensive on every production
  fixed-name open; the #1845 `InboundBudget` pool arithmetic is balanced (no
  leak/double-release/lockout); the #1846 error sanitisation leaks no internals
  on any FAILURE/IGNORED path; the #1848 csrfile empty-image fix holds.
- **`unsafe` and `math/rand`.** Every non-test `unsafe.` site was triaged safe:
  the csrfile mmap reinterpret is gated by an overflow-safe exact-canonical
  header validation; `nodeset`'s tagged-union `n≤8` invariant is structural and
  never crosses the disk boundary; the adjlist COW publication is atomic and
  `-race`-clean. No security-relevant randomness uses `math/rand` (session IDs
  and `randomUUID` use `crypto/rand`); a supply-chain gate test rejects any
  regression to `math/rand` in the Bolt session path.
- **Crypto / auth / TLS.** `crypto/subtle` constant-time auth comparison;
  secure-by-default (nil auth fails closed); loud plaintext/no-auth startup
  warnings; TLS 1.2-floor AEAD-only default; no `InsecureSkipVerify`; no
  `net/http/pprof`; no credential ever logged; internal errors sanitised at the
  wire.
- **Cypher injection & procedures.** The parameter/injection boundary is
  airtight (a parameter value never re-enters the parser; labels/types/keys/
  procedure names come only from the AST); the six built-in procedures are
  read-only introspection with no filesystem/network/exec and no query-reachable
  dynamic dispatch; the parser is guarded against stack-overflow and
  complexity attacks; variable-length paths, comprehensions, `reduce`, and string
  concatenation are all bounded and context-cancellable.
- **Deserialization & import.** Parse→validate→CRC→bound before allocating across
  csrfile/WAL/snapshot/manifest; GraphML is XXE / billion-laughs / external-DTD
  safe at every entry point; JSONL is depth/line/aggregate bounded; the #1844 CSV
  field guard is bypass-proof; no compression/zip-bomb path; importers take
  `io.Reader`, so path traversal is not reachable; DOT/GraphML/CSV export
  injection is well defended.
- **Dependencies** clean (`govulncheck`, no known vulnerabilities).

## Verdict

GoGraph is **production-ready from a security standpoint** on `main` after this
remediation. The five prior engagements plus this sixth, adversarial round have
now closed every hostile-input memory-exhaustion vector across all five attack
surfaces; the confidentiality and integrity posture (no RCE, corruption,
disclosure, injection, or ACID violation) was re-confirmed sound. All compliance
gates — TCK 3897, ACID (storage-auditor certification), `-race`, `govulncheck` —
are green, and each remediation was certified by the relevant specialist.

**Operational note.** Two by-design memory bounds are opt-in: the Bolt engine-wide
inbound-decode ceiling (#1845/#1849) and the Cypher result-memory ceiling
(#1842) both activate only when a Go soft-memory limit is set. Operators exposing
the Bolt server to untrusted clients should set `GOMEMLIMIT` (or an explicit
positive `Options.MaxInboundDecodeBytes` / `EngineOptions.MaxResultBytes`) so the
aggregate ceilings engage. The always-on per-message and per-call bounds hold
regardless.
