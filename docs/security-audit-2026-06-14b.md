# GoGraph Security Audit — 2026-06-14 (second pass, "-14b")

This report documents a second, independent, exhaustive security audit of the
GoGraph module conducted on 2026-06-14 against `main` (HEAD `1c63bf8`, release
line v0.3.x). It is a deliberate **follow-on** to the morning audit of the same
day ([`security-audit-2026-06-14.md`](security-audit-2026-06-14.md), findings
#1467–#1479, all fixed): rather than re-tread that ground, this pass was
designed to be **additive** by stressing three things the first pass could not
have covered:

1. the **completeness and correctness of the morning fixes** — fresh code is
   fresh attack surface;
2. **sibling code paths** that share a fixed defect's root cause but were never
   touched by the localized fix; and
3. **vectors the first pass did not enumerate**.

It follows the same phased red-team methodology (mapping & planning →
reconnaissance → exploitation → post-exploitation/blast-radius → reporting) and
is grounded only in verified evidence: every finding cites a source location
and ships a **bounded, runnable reproduction**; every "verified-solid" claim
names the attack that was attempted and repelled.

All findings are tracked individually in the `gograph` roadmap (`rmp`), tasks
**#1480–#1489** (#1487 was a cross-auditor duplicate of #1482 and was removed),
grouped into themed sprints **184** (untrusted-artifact unbounded
allocation/recursion), **185** (untrusted query/transaction resource
exhaustion), and **186** (protocol & export correctness). A reproducible
security test battery (12 new test files) accompanies this report; see
[Security test battery](#security-test-battery).

> **Status — REMEDIATED (2026-06-14).** All nine findings were **fixed** on
> branch `security/sec-2026-06-14b-remediation` (9 commits, one per task), and
> every battery demo was flipped from the *document-and-pass* convention into a
> strict regression assertion that passes on the fixed code and fails on
> regression. Final integrated gate on the branch: `go test ./...` → **94
> packages pass (exit 0)**; the full `TestSec_`/`FuzzSec_` battery passes;
> `go test -race` reports **0 data races** across the touched and
> concurrency-sensitive packages; **openCypher TCK holds at 3897/3897**
> (0 failed / 0 undefined); `govulncheck`, `staticcheck`, and `golangci-lint`
> are clean; `gofmt`/`goimports` clean. Both non-negotiable mandates (100% TCK,
> 100% ACID) were preserved by every fix — valid snapshots/indexes still load
> and only the *failure mode* of a hostile artifact changed (OOM/crash → clean
> typed error). See [Remediation](#remediation-completed-2026-06-14).

## Scope and threat model

Unchanged from the morning audit. GoGraph is an **embedded Go library** (plus an
embeddable Bolt server); there is no standalone daemon. The relevant attacker
supplies one of:

- untrusted **Bolt** bytes/messages to an application embedding the server;
- untrusted **Cypher** query text and/or parameters;
- a malicious **import file** (GraphML / CSV / JSON-Lines / DOT);
- an attacker-influenced **on-disk artifact** (snapshot / WAL / CSR / **index /
  constraint** file) that arrives via a non-privileged channel (backup, restore,
  download, replication).

Per [`SECURITY.md`](../SECURITY.md), an attacker who already holds filesystem
write access to the **live** database directory, and third-party-consumer
defects not caused by a GoGraph API-contract violation, are **out of declared
scope** and are marked accordingly.

## Methodology

Six specialist auditors ran in parallel over disjoint surfaces, each told the
13 morning findings were already fixed and instructed to report only *new*
defects (incomplete fix, fix-introduced bug, sibling path, or un-enumerated
vector):

| Domain | Surface |
|---|---|
| α | Fix-regression & new-code attack surface (the 13 morning fixes #1467–#1479) |
| β | Bolt protocol depth, concurrency, resource bounds |
| γ | Cypher engine — resource exhaustion & semantic/ACID integrity beyond the morning pass |
| δ | Persistence: WAL replay, recovery, snapshot, CSR/mmap, **indexes**, constraints |
| ε | Untrusted file-format ingestion/export |
| ζ | Cross-cutting: `unsafe`, integer overflow, panic-as-DoS, info disclosure, concurrency, supply chain |

Each finding was classified against CWE and cross-checked against public
vulnerability knowledge bases (CVE, NVD, ENISA EUVD, Exploit-DB, OSV, CISA KEV)
for known patterns. Reproductions were bounded so they detect a defect without
exhausting this host: small inputs, `context` deadlines, and `GOMEMLIMIT`
subprocesses for over-allocation demonstrations (the expensive count/recursion
proofs are gated behind `testlayers.RequireSoak` so the short layer never
triggers the multi-GiB/TiB allocation the fix must eliminate).

## Executive summary

The module's externally-attacked boundaries that the morning audit hardened
(Bolt auth state machine including the new ≥5.1 LOGON path, PackStream decode
budgets, slowloris deadlines, TLS posture, the CSR/mmap `unsafe` surface, WAL
framing, GraphML/CSV/JSONL/DOT ingestion, the `unsafe`/overflow/concurrency
cross-section, and the supply chain) were re-attacked and **held** — the
supply-chain pins from #1472/#1473 were independently verified to have landed,
and `govulncheck`/`go mod verify` are clean.

**Nine new findings were confirmed.** The dominant — and most important —
theme is that **the morning remediation, while correct where it was applied,
was not *generalized***. The "bound the eager `make()` before the integrity
gate" fix was localized to the snapshot record and string-table decoders; the
*same root cause* survives in:

- the **btree index** decoder (#1480) and **label index** decoder (#1481),
  reachable on snapshot restore via `store/recovery.applySnapshotIndexes`;
- the snapshot record decoders themselves, where the clamp bounds only the
  *initial* `make` but the `append` loop still grows to the untrusted count
  because `FileEntry.Size` was never threaded through (#1486); and
- a **depth** variant the count-clamp cannot catch — unbounded `PropList`
  recursion (#1488).

The Cypher analogue is identical in shape: the morning per-evaluation
list-*element* budget (#1475) is **byte-blind**, so string concatenation growth
is uncharged (#1482). Two genuinely new vectors round out the set: a Bolt
`tx_timeout` **integer overflow** that bypasses the transaction-deadline reaper
and pins the global writer lock (#1484), and a disconnected Cartesian-product
query whose intermediate work is unbounded under a default-zero statement
timeout (#1483). Two low-severity correctness defects (Bolt NOOP keep-alive
#1485, DOT reserved-keyword emission #1489) complete the list. **None is a
memory-corruption or RCE defect**; Go's memory safety and the project's
`unsafe` discipline continue to hold.

| Severity (rmp 0–9) | Count | IDs |
|---|---|---|
| High (7) | 5 | #1480, #1481, #1482, #1484, #1486 |
| Medium (5–6) | 3 | #1483, #1488, #1489 |
| Low (3–4) | 1 | #1485 |

## Findings

| rmp | Sev | CWE | Location | Summary | Sprint |
|---|---|---|---|---|---|
| [#1480](#1480) | 7 | 789/20 | `graph/index/btree/index.go:525` | btree index `Deserialize` reserves `make([]entry, 0, entryCount)` from an untrusted 8-byte count (rejected only when `> 1<<40`) before reading the body → 20-byte CRC-valid payload drives **16 TiB** `TotalAlloc`. Reached on snapshot restore. | 184 |
| [#1481](#1481) | 7 | 789/400/20 | `graph/index/label/index.go:375` | label index `Deserialize` passes an untrusted `uint32 count` verbatim as a `make(map, int(count))` size hint → 16-byte payload (`count=2³²−1`) eagerly materialises ~**144 GiB** of map buckets + ~26 s CPU. | 184 |
| [#1486](#1486) | 7 | 770/789 | `store/snapshot/full.go` `readVerified{Labels,Properties,Mapper,EdgeHandles}` + `mapper.go` | The morning capHint clamp bounds only the *initial* `make`; the `append` loop still grows to the untrusted header count because `FileEntry.Size` is not threaded in (unlike `readCSRLimited`), and the CRC is checked only *after* the full parse. A ~13 MB `labels.bin` parsed while its manifest `FileEntry.Size` declared 64 bytes. **Incomplete #1467.** | 184 |
| [#1488](#1488) | 6 | 674/248 | `store/snapshot/properties.go:803` `decodeListPropertyValue` (+ `edgehandles.go:500`) | The property-value decoder has no recursion-depth bound on nested `PropList` values. The *encoder* forbids any nesting, but the *decoder* doesn't enforce it; a crafted file nests to `filesize/5` depth → goroutine-stack-exhaustion `fatal error` with **no `recover` on the library path**. Distinct from #1467 (width, not depth). | 184 |
| [#1482](#1482) | 7 | 789/400/770 | `cypher/expr/eval.go:969` `evalArith` string `+` branch | The per-evaluation budget (`DefaultMaxListElements=1e7`, #1475) charges list *elements* but is byte-blind; string `+` returns `StringValue(ls+rs)` uncharged. `RETURN size(reduce(s='x', i IN range(1,24) | s+s))` → 16 MiB; exponent 33 → 8 GiB from ~100 bytes of query text. The exponential form runs only ~N iterations so the #1477 ctx-stride check never fires. **Incomplete #1475.** | 185 |
| [#1483](#1483) | 5 | 400/770 | `cypher/plan/join_enum.go`, `cypher/api.go`, `bolt/server/serve.go:144` | A disconnected `MATCH (a),(b),(c),(d) RETURN count(*)` streams `Nᵏ` intermediate tuples; the result-row/byte caps bound only the 1-row output. The operator path *is* cancellable, but the Bolt server's `MaxStatementTimeout` **defaults to 0** (no cap) and `handleRun` leaves `runCtx` deadline-free, so a default server lets an authenticated client pin a CPU core indefinitely. | 185 |
| [#1484](#1484) | 7 | 190→400/667 | `bolt/server/session.go` `handleBegin` (≈L905), `handleRun` (≈L644) | `tx_timeout` (ms) → `time.Duration(ms) * time.Millisecond` overflows int64 nanoseconds (e.g. `1<<62` → product 0; `MaxInt64` → negative), guarded only by `ms > 0`. On a **default** server (`MaxStatementTimeout=0`) the clamp is skipped, `txDeadline` stays zero (no wall-clock reaper) and the engine tx gets no deadline → a client holds the single global writer lock indefinitely (refreshing the idle `ConnTimeout` with a trivial RUN), bypassing the #1302/#1346 finite writer-lock bound. | 185 |
| [#1485](#1485) | 4 | 20/400 | `bolt/proto/chunking.go` `ReadMessage` + `bolt/server/serve.go` loop | A Bolt ≥4.1 NOOP keep-alive (bare `00 00` chunk that does not terminate an in-progress message) must be silently discarded; GoGraph returns an empty message, the serve loop decodes it, fails, and replies `FAILURE Neo.ClientError.Request.Invalid`, which evicts the very idle connection the keep-alive should preserve. | 186 |
| [#1489](#1489) | 5 | 116/838 | `graph/io/dot/writer.go:153` `isSimpleID` (emit at `:98`/`:119`) | The DOT exporter emits Graphviz reserved keywords (`node`, `edge`, `graph`, `digraph`, `subgraph`, `strict`; case-independent) as **unquoted** ids. A vertex named `node` emits `node -> safe;`, which Graphviz reinterprets as a default-attribute statement rather than an edge — silent export-integrity corruption. | 186 |

### Detailed findings

<a id="1480"></a>
#### #1480 — btree index `Deserialize` eager cap-make (High)

`graph/index/btree/index.go:525` does `out := make([]entry[V], 0, entryCount)`
where `entryCount` is an 8-byte field read from the (untrusted) serialized
index, rejected only when strictly `> 1<<40`. This decoder is reached on
snapshot restore: `store/recovery.applySnapshotIndexes` (recovery.go:272) →
`index.Serializer.Deserialize`. A 20-byte CRC-valid `indexes/<name>.bin` with
`entryCount = 2⁴⁰` drove **16 TiB** of `TotalAlloc`
(`17592186099104` bytes under `GOMEMLIMIT=512MiB`) before the truncated body
yielded `EOF`. This is the exact class of the morning snapshot finding #1467,
in a decoder the fix never reached. Fix: clamp the reservation to
`min(entryCount, capHintMax)` (≈`1<<20`, the ceiling already used by
`store/snapshot/tombstones.go`/`constraints.go`) and grow on demand — the
RocksDB/LevelDB table-reader discipline of never pre-sizing on an untrusted
count.

<a id="1481"></a>
#### #1481 — label index `Deserialize` eager map size-hint (High)

`graph/index/label/index.go:375` does
`bits := make(map[uint32]*roaring64.Bitmap, int(count))` with the untrusted
`uint32 count` passed verbatim as the map size hint, unclamped. A 16-byte
CRC-valid payload with `count = 2³²−1` drove ~**144 GiB** `TotalAlloc`
(`154954422656` bytes) **plus ~26 s of CPU** under `GOMEMLIMIT=1GiB` — worse
than the slice case because map buckets are eagerly materialised, not lazy zero
pages. Same fix as #1480.

<a id="1486"></a>
#### #1486 — Snapshot decoders: `FileEntry.Size` not threaded → append still unbounded (High, incomplete #1467)

The morning fix bounded the *initial* `make` in `readVerified{Labels,Properties,Mapper,EdgeHandles}`
with a `capHint`, but the subsequent `append` loop still grows to the true
on-disk header count: there is no `io.LimitReader` and no cross-check against
the manifest `FileEntry.Size`, and the CRC is verified only *after* the full
parse. (The sibling `readCSR` path does this correctly — it threads
`FileEntry.Size` into `readCSRLimited` as a precise byte budget.) A
`labels.bin` whose manifest declared `FileEntry.Size = 64` was parsed to ~13 MB.
Driven to the `1<<40` ceiling and bounded only by disk, this re-opens the
original #1467 OOM-on-recovery hole through the back door. Fix: wrap each
component reader in `io.LimitReader(f, entry.Size)` (mirror the CSR discipline).

<a id="1488"></a>
#### #1488 — Snapshot `PropList` decoder lacks a recursion-depth bound (Medium)

`decodeListPropertyValue` reads each element's kind tag from the untrusted wire
and recurses via `decodePropertyValue` when the tag is `PropList`, with no depth
counter. The *encoder* explicitly rejects nested lists, so a legitimate snapshot
never nests at all — but the decoder doesn't enforce that invariant. A crafted
`properties.bin` / `edgehandles.bin` nests a single-element list to a depth
bounded only by `filesize/5` (≈111k levels per MiB), driving unbounded Go-stack
growth to the `fatal error: goroutine stack exceeds 1000000000-byte limit`
ceiling. Reached from the **public** `LoadSnapshotFull` (the edgehandles path
crashes at parse time, the properties path at apply time) with **no `recover`**
on the library path → crashes the embedding app (CWE-674/248). Confirmed: depth
200 000 decodes in ~8.7 s. This is *distinct* from #1467/#1486 (which cap
allocation **width**); a per-level count of 1 evades the count-clamp entirely.
Fix: reject a `PropList` element kind in the decoder, mirroring the encoder's
invariant (a one-line guard on both the properties and edgehandles paths).

<a id="1482"></a>
#### #1482 — String `+` byte-growth uncharged by the list-element budget (High, incomplete #1475)

The morning fix added `DefaultMaxListElements = 10_000_000` and ctx-cancellation
to `reduce`/comprehension/quantifier iteration, but the budget counts list
*elements*, not bytes. `evalArith`'s string `+` branch
(`cypher/expr/eval.go:969`) returns `StringValue(string(ls)+string(rs))` with no
budget charge, unlike the adjacent list-concat branches.
`RETURN size(reduce(s='x', i IN range(1,24) | s+s))` produces a 16 MiB string in
~11 ms; the same doubling reaches ~8 GiB at exponent 33 — a single small Cypher
query (reachable via the Bolt RUN path or `Engine.Run`) exhausts host memory.
Because the exponential form runs only ~N iterations, the #1477 every-4096-iter
ctx check never fires before the OOM. The `WITH s+s AS s` chain is an equivalent
non-`reduce` vector. Fix: a per-evaluation **byte** budget shared across the
expression helpers, set above the TCK's 10 000-char string ceiling (verified
with the cypher-expert: no TCK `reduce` scenarios exist and the max asserted
string is 10 000 chars in `Literals6`).

<a id="1483"></a>
#### #1483 — Disconnected Cartesian-product CPU exhaustion + default-zero Bolt timeout (Medium)

A disconnected `MATCH (a),(b),(c),(d) RETURN count(*)` streams `Nᵏ` intermediate
tuples; the result-rows/bytes caps bound only the (1-row) output, not the
intermediate work (confirmed: `40⁴` = 2.56M tuples through `count(*)` in 155 ms,
1 row out). The operator path **is** cancellable — a 300 ms deadline aborts a
`60⁵ ≈ 7.78e8`-tuple product — but the Bolt server's `MaxStatementTimeout`
defaults to **0** (no cap) and `handleRun` leaves `runCtx` deadline-free when
unset, so a default server lets an authenticated client pin a CPU core
indefinitely. This finding carries a **Decision-autonomy** point (which
mitigation to adopt); the recommendation is a finite default Bolt
`MaxStatementTimeout` **plus** a Cartesian-product planner notification/guard.

<a id="1484"></a>
#### #1484 — `tx_timeout` integer overflow bypasses the writer-lock reaper (High)

`handleBegin` computes `effective = time.Duration(ms) * time.Millisecond` from
the client BEGIN `tx_timeout`, guarded only by `ms > 0`. `time.Duration` is
int64 nanoseconds, so a hostile `ms` overflows: `1<<62` → product exactly 0;
`MaxInt64` → negative. On a **default-configured** server (`NewServer` sets
`DefaultTxTimeout` but leaves `MaxStatementTimeout = 0`) the clamp
`if s.maxStmtTimeout > 0 && effective <= 0 …` is skipped, so `effective` stays
≤ 0: `handleBegin` leaves `s.txDeadline` zero (the serve loop arms no wall-clock
reaper) and `newTx` receives `timeout ≤ 0` (engine tx rooted at the bare
connection context, no engine deadline). A client BEGINs with the overflow
timeout, refreshes the idle `ConnTimeout` with a trivial RUN/PULL every <30 s,
and holds the engine's single global writer serialisation indefinitely —
blocking every other writer (liveness DoS), bypassing the #1302/#1346 finite
writer-lock bound. Verified white-box: all three overflow cases yield a
`SUCCESS` BEGIN with `txDeadline.IsZero() == true`; a control with
`MaxStatementTimeout` set mitigates it. Fix: detect the multiplication overflow
(or compute in a wider type / clamp `ms` to a sane maximum) and treat ≤ 0 as
"use the server default", independent of whether `MaxStatementTimeout` is set.

<a id="1485"></a>
#### #1485 — NOOP keep-alive answered with FAILURE (Low)

Bolt 4.1+ defines a standalone `00 00` chunk (one that does not terminate an
in-progress message) as a NOOP keep-alive that a conformant server **must
silently discard**. GoGraph negotiates Bolt 4.4 and 5.0–5.6 (all ≥4.1).
`ReadMessage` returns an empty `[]byte{}` for a bare `00 00`; the serve loop
decodes the empty buffer, fails, and replies
`FAILURE Neo.ClientError.Request.Invalid "malformed Bolt message"`. For a real
driver this unsolicited, non-retryable `ClientError` moves the connection to
FAILED and evicts it from the pool — the keep-alive destroys the very idle
connections it is meant to preserve. Verified end-to-end, including a spurious
FAILURE injected mid-stream between a RUN success and its PULL. Fix: treat an
empty decoded message (bare `00 00`) as a no-op and continue the read loop.

<a id="1489"></a>
#### #1489 — DOT exporter emits reserved keywords as unquoted ids (Medium)

Per the Graphviz DOT grammar, `node`, `edge`, `graph`, `digraph`, `subgraph`,
and `strict` are case-independent keywords that may not be used as bare
identifiers. `isSimpleID` (`graph/io/dot/writer.go:153`) treats them as ordinary
alphabetic ids, so a vertex literally named `node` (plausible in a Node.js
dependency graph or a metamodel) emits `node -> safe;`, which Graphviz
reinterprets as a default-attribute statement rather than an edge — silent,
irreversible export-integrity corruption (CWE-116/838). Not a remote DoS (write
path; no panic/OOM/hang). Fix: have `isSimpleID` reject any id whose lowercase
form is one of the six keywords so the existing `quote()` path wraps it.

## Verified-solid (defences confirmed to hold)

Each was attacked in this pass and held; the battery pins each as a regression
guard.

**Morning fixes re-attacked and confirmed robust**

- **#1470 Bolt ≥5.1 LOGON auth** — no pre-LOGON bypass for
  RUN/BEGIN/PULL/DISCARD/COMMIT/ROLLBACK/ROUTE; LOGON→LOGOFF→RUN rejected; a
  failed first LOGON is terminal/DEFUNCT (no retry-with-other-credentials);
  `crypto/subtle.ConstantTimeCompare` still used; `authenticated` set only
  post-verify (no TOCTOU). One Info-only Bolt-5.1 conformance nit (LOGOFF →
  `StateReady`) with no security impact.
- **#1479 `=~` operator + anchoring** — the source-peek disambiguates `=~` from
  `=` correctly (`= ~` with a space safely stays equality); the `\A(?:…)\z`
  anchoring is correct (`'xadminx' =~ 'admin'` → false, closing the authz
  hazard; alternation binds correctly); Go RE2 is linear (no ReDoS); the cache
  is a bounded FIFO-1024 keyed on the anchored pattern (no poisoning).
- **#1475 list-element budget (list path)** — a nested comprehension at `1e8` is
  correctly rejected with a typed `EvalError`; only the *string* path leaks
  (#1482).
- **#1476 `UnionFindSlice` 64-bit** — `parent []int`, `rank []uint8`; no residual
  `int32` truncation; callers in `search/kruskal.go`/`wcc.go` pass `int`.
- **#1478 VLE per-query budget** — `totalEdgesVisited` is reset only in `Init`
  (per query), accumulates across rows, and is checked at every edge;
  `defaultMaxUnboundedHops = 65536` is applied for `[*]`, `[*..]`, `[*1..]`.
- **#1471 CSV formula sanitizer (when ON)** — solid for the documented triggers
  (`= + - @ \t \r`); leading-whitespace-before-trigger is not exploitable on
  mainstream spreadsheets (Info-only hardening note, not filed).
- **#1467/#1468/#1469 (the part the fix *does* cover)** — `capHint`/`listCapHint`
  integer math is sound (no overflow/wrap/negative); truncated bodies fail fast
  with bounded allocation. The residual is purely the missing-`FileEntry.Size`
  genuine-body path (#1486) and the un-generalized index decoders (#1480/#1481).

**Newly attacked surfaces that held**

- **Bolt**: struct-as-parameter injection rejected (`ErrUnsupportedParamType` —
  Node/Relationship/Path/temporal structs cannot be injected as engine values);
  no input temporal-decode path (output encoder round-trip tested);
  PULL/DISCARD negative/zero/huge `n` safe; bogus `qid` ignored; ROUTE pre-auth
  rejected; multi-session concurrent read+write under `-race` is race-free;
  abrupt mid-statement disconnect (even mid-BEGIN holding the writer lock) leaks
  no goroutine or lock (goleak clean); `MaxConnections` semaphore bounds accepts;
  huge HELLO/BEGIN maps bounded by the 128 MiB decode budget + 16 MiB chunk cap.
- **Cypher**: parser guard covers deep nesting (5000 parens/lists rejected),
  CASE depth ≤256, binary-op chains ≤512; `range()` step 0 → typed `ArityError`,
  overflow-safe count, 100M cap; UNWIND streams element-by-element and is
  cancellable; all eager operators capped (Sort/Eager/Distinct 10M rows,
  EagerAggregation 1M groups, collect/percentile 10M/group); every operator
  honours `ctx.Err()`; DELETE/DETACH DELETE route through
  `mutator.RemoveNode`/`RemoveEdge` (the 2026-06-12 ghost-node bug is fixed, not
  reproduced).
- **Persistence**: the **hash index** decoder streams entries (no count-keyed
  `make`); `constraints.bin`/`tombstones.bin` counts are clamped; the btree
  on-disk format is a flat ascending list (no child-pointer cycles → no
  infinite-loop-on-lookup); `csrfile` mmap validates against the *mapped* length
  with no re-stat TOCTOU (SIGBUS-on-shrink is a documented embedder
  precondition, as in LMDB/BoltDB); write-path FS posture (predictable temp
  names, no `O_NOFOLLOW`/`O_EXCL` on data files) is uniform across WAL/snapshot/
  csrfile and sits within the embedded-DB trust model (SQLite/BoltDB/RocksDB make
  the same assumption — a symlink-on-temp attack already requires the DB's own
  directory to be attacker-writable). WAL replay is log-trusting (the
  RocksDB/Neo4j norm; `AddEdge` auto-vivifies endpoints in *both* replay and the
  live path, so there is no replay-specific divergence) — see the Informational
  note below.
- **File formats**: GraphML type-confusion / undefined-key-ref / duplicate-id /
  property-key injection all produce clean errors or opaque storage; CSV
  NUL/BOM/lone-CR/CRLF/unterminated-quote/int-overflow all clean; JSONL huge
  numbers / NaN / Inf / duplicate keys / line-length cap (`ErrLineTooLong`) all
  clean; DOT metacharacter escaping solid (only the reserved-keyword gap #1489);
  every reader/writer takes `io.Reader`/`io.Writer` (no path-traversal surface).
- **Cross-cutting**: every `unsafe.` site is confined to
  `graph/mapper.go`/`graph/adjlist`/`store/csrfile` and is sound (the adjlist
  atomic-pointer publication/grow path was re-audited); the remaining narrowing
  conversions (`pagerank` `uint32(deg)`, `bibfs` `int32`) require >2³¹ elements
  in memory and are physically infeasible (unlike #1476, which truncated a
  *count* used in index math); `go test -race` across `graph/lpg`, all of
  `cypher/`, and all of `store/` reports zero data races; panics reachable only
  from trusted internal callers are acceptable per the fail-stop rule; non-Bolt
  error messages echo only the caller's own supplied path.
- **Supply chain (verified, not trusted)**: `go mod verify` → all modules
  verified; `govulncheck ./...` → no vulnerabilities; toolchain `go1.26.4`
  current. The #1472/#1473 fix **landed**: every GitHub Action across all
  workflows is pinned to a 40-hex SHA, `cyclonedx-gomod` is pinned to `@v1.10.0`,
  and CI top-level `permissions: contents: read` is least-privilege — so the
  tj-actions CVE-2025-30066 class does not apply.

### Informational (not filed as defects)

- **WAL-replay consistency trust.** Replay applies ops trustingly and recovered
  data is not re-validated against UNIQUE constraints
  (`cypher/api.go SeedUniqueValuesIgnoringDuplicates`, explicitly documented:
  "recovery must always succeed; duplicates are historical artefacts the live
  path rejects on next write"). A crafted CRC-valid WAL with two same-UNIQUE
  nodes recovers into a serviceable-but-constraint-violating state observable
  until touched. This is log-trusting replay (the RocksDB/Neo4j norm); treating
  it as a vulnerability is a threat-model/product decision, not a code fix.
- **`ci.yml` installs `govulncheck@latest`/`benchstat@latest`** on read-only,
  secret-less jobs (the *release* path is correctly pinned). Low risk; pinning
  them would complete reproducibility. Below the configuration-defect bar given
  the least-privilege context.
- **`time.LoadLocation` reachable** with an attacker-controlled tz string via
  Cypher `datetime({timezone:$tz})` over Bolt RUN. Go's `LoadLocation` rejects
  path traversal and is well-trodden; flagged for awareness.

## Security test battery

Twelve additive test files were authored (test-only; no production code was
changed; the openCypher TCK result-pass count is unchanged at 3897; nothing was
committed). They follow the morning audit's *document-and-pass* convention so
`go test ./...` stays green, each carrying a `SECURITY-GAP #NNNN` marker and a
documented one-line flip to a strict regression assertion once the fix lands.

| Package | File | Proves |
|---|---|---|
| `graph/index/btree` | `security_store_btree_capmake_test.go` | #1480 (soak-gated 16 TiB probe + always-on truncated-reject guard) |
| `graph/index/label` | `security_store_label_maphint_test.go` | #1481 (soak-gated 144 GiB probe + truncated-reject guard) |
| `store/snapshot` | `security_fixreg_decoder_test.go`, `security_fixreg_overflow_test.go` | #1486 (size-not-threaded; append amplification; capHint integer math) |
| `store/snapshot` | `security_xcut_nested_proplist_test.go` | #1488 (`GOGRAPH_SNAPSHOT_DEPTH_FIX=1` strict gate; verified to fail when armed) |
| `cypher` | `security_cypher_string_byte_budget_test.go`, `security_fixreg_stringconcat_test.go` | #1482 (byte-budget bypass; TCK 10 000-char floor control) |
| `cypher` | `security_cypher_cartesian_test.go` | #1483 (intermediate-work blowup; positive cancellable lock-in) |
| `bolt/server` | `security_bolt_txtimeout_overflow_test.go` | #1484 (all three overflow cases → `txDeadline.IsZero()`; `MaxStatementTimeout` control) |
| `bolt/server` | `security_bolt_noop_test.go` | #1485 (NOOP → spurious FAILURE, incl. mid-stream) |
| `bolt/server` | `security_fixreg_boltlogon_test.go` | #1470 LOGON-auth defences (verified-solid lock-in) |
| `graph/io/dot` | `security_io_reserved_keyword_test.go` | #1489 (`GOGRAPH_DOT_KEYWORD_FIX=1` strict gate) |

Run the battery:

```bash
go test ./...                                          # short layer (default) — stays green
go test -run 'TestSec_|FuzzSec_' ./...                 # the battery only
go test -tags=soak -run 'TestSec_' ./graph/index/...   # + the soak-gated allocation probes
```

### Validation gate (post-battery)

`go build ./...` → exit 0. `go vet` on every touched package (parallel-authored
files compile together with no symbol collision). `go test -run 'TestSec_'` on
all touched packages → green on the short layer; the two env-gated strict
assertions (#1488, #1489) were verified to **fail when armed**, confirming the
defects are live and the gates are real. No production or TCK file was modified,
so **TCK holds at 3897/3897** and ACID behaviour is unchanged by construction.

<a id="remediation-completed-2026-06-14"></a>
## Remediation (completed 2026-06-14)

All nine findings were fixed on branch `security/sec-2026-06-14b-remediation`,
one commit per task, sprints 184 → 185 → 186:

| Sprint | rmp | Commit | Fix |
|---|---|---|---|
| 184 | #1480 | `46673b4` | btree index `Deserialize` clamps the eager `make` cap to `min(entryCount, 1<<20)` and grows via append. |
| 184 | #1481 | `6c97505` | label index `Deserialize` clamps the map size hint to `min(int(count), 1<<20)`. |
| 184 | #1486 | `f594641` | snapshot `readVerified*` thread `FileEntry.Size` and wrap each component body in `io.LimitReader` (mirrors `readCSRLimited`), so a declared count cannot grow past the real on-disk size. |
| 184 | #1488 | `120fc6b` | `decodeListPropertyValue` rejects a nested `PropList` element with `ErrPropertiesCorrupted`, enforcing the encoder's no-nesting invariant (closes the unbounded-recursion path). |
| 185 | #1484 | `22b365d` | client `tx_timeout`/`timeout` converted overflow-safely; a non-positive/overflowing value falls back to the server default `DefaultTxTimeout` **unconditionally**, so the wall-clock reaper is always armed. |
| 185 | #1482 | `82c705b` | a per-evaluation **byte** budget (`DefaultMaxStringEvalBytes = 1<<30`) is charged on string `+` growth before allocation; breach → typed `EvalError`. The 10 000-char TCK floor is ~5 orders of magnitude below the ceiling. |
| 185 | #1483 | `bc7173b` | a plan-time Cartesian-product **notification** (faithful to Neo4j's `Neo.ClientNotification.Statement.CartesianProductWarning`, surfaced via the new `Result.Notifications()` API and the Bolt PULL `SUCCESS` metadata) plus a locked-in deadline-cancellation guarantee. No breaking default-timeout change. |
| 186 | #1485 | `86a07ae` | `ChunkedReader.ReadMessage` recognises a bare `00 00` NOOP keep-alive (no in-progress body) and skips it instead of surfacing an empty message that the serve loop rejected with `FAILURE`. |
| 186 | #1489 | `d85a403` | `isSimpleID` returns false for the six DOT reserved keywords (case-independent), routing them through the existing `quote()` path. |

Each fix flipped its battery file into a strict regression gate (the
`GOGRAPH_SNAPSHOT_DEPTH_FIX`/`GOGRAPH_DOT_KEYWORD_FIX` env gates were removed in
favour of unconditional assertions). Integrated gate: 94 packages pass, TCK
3897/3897, `-race` 0 races, `govulncheck`/`staticcheck`/`golangci-lint` clean.

### Original remediation plan (for reference)

Sprints **184–186** held the nine findings. Recommended sequencing, highest
leverage first:

1. **Sprint 184** — generalize the morning decoder-bound discipline:
   `io.LimitReader(f, entry.Size)` + `min(count, capHintMax)` clamp across the
   btree (#1480) and label (#1481) index decoders and the snapshot record
   decoders (#1486), and a decoder-side no-nesting guard for `PropList` (#1488).
   One coherent fix family.
2. **Sprint 185** — a per-evaluation **byte** budget in the expression evaluator
   (#1482), a finite default Bolt `MaxStatementTimeout` + Cartesian planner
   notification (#1483), and overflow-safe `tx_timeout` handling (#1484).
3. **Sprint 186** — discard the NOOP keep-alive (#1485) and quote DOT reserved
   keywords (#1489).

Every fix must preserve both non-negotiable mandates (100% openCypher TCK, 100%
ACID); each battery file documents the exact assertion to flip once its fix
lands.
