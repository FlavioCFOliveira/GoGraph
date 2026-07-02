# GoGraph Security Assessment & Remediation — 2026-07-02

This report documents a whole-module security assessment of GoGraph conducted
on 2026-07-02 against `main` (baseline `7d3eb4c`, release line v0.6.x) and the
full remediation of every confirmed finding. It is the fifth security
engagement on the module, following the three rounds of 2026-06-14
(findings #1467–#1496, all fixed) and complementing the reliability and
production-readiness audits since.

> **Remediation status.** All confirmed findings are **FIXED** on `main`
> (rmp roadmap `gograph`, sprint **259**, tasks **#1843–#1848**). The
> integrated tree is green: `go build ./...` clean, `go test ./...` and
> `go test -race ./...` pass across all 97 packages with no data race, the
> **openCypher TCK holds at 3897/3897** (`tckExecutionBaseline` untouched),
> `gofmt`/`go vet` clean, and `govulncheck` reports no known vulnerabilities.
> The WAL change was certified by the storage-engine-auditor and the Bolt
> inbound-decode ceiling by the concurrency-architect. Commits: `33454e4`,
> `e63f9ce` (+`efff3be`), `2174d21`, `17d8056`, `11c611f`. Not pushed.

## Scope, threat model, and method

- **Target.** The entire GoGraph Go module: core graph types, search
  algorithms, the Cypher engine, the Bolt protocol server, the
  persistence/storage layer, and the import/export (serialization) layer.
- **Threat model.** Every byte on the Bolt wire is attacker-controlled
  (malicious or unauthenticated client); every Cypher query and parameter is
  attacker-controlled; every on-disk artifact (WAL, snapshot, CSR, index dumps)
  is untrusted input, including a store directory adopted from an untrusted
  source (a tampered backup or a shared filesystem); every imported file
  (CSV/GraphML/JSONL) is untrusted.
- **Method.** `govulncheck` plus five parallel adversarial reviewers, one per
  attack surface (Bolt network, persistence/deserialization, I/O parsers,
  Cypher engine/procedures, cross-cutting crypto/secrets/config). Every finding
  was traced to source and independently confirmed; the CSV memory
  amplification was reproduced empirically.

## Summary of findings

No Critical finding. No prior audit fix had regressed. Six findings were filed
and all are fixed.

| # | Severity | CWE | Surface | Title | Commit |
|---|----------|-----|---------|-------|--------|
| [#1843](#1843) | **High** | CWE-59 | Store/WAL | WAL family opened without `O_NOFOLLOW` → symlink-escape arbitrary-file append/truncate | `33454e4` |
| [#1844](#1844) | Medium | CWE-789/1284 | I/O CSV | Unbounded fields-per-record → ~50× memory amplification / OOM | `e63f9ce` |
| [#1845](#1845) | Medium | CWE-770 | Bolt | No engine-wide inbound-decode memory ceiling (per-conn cap × MaxConnections, pre-auth) | `11c611f` |
| [#1846](#1846) | Low | CWE-209 | Bolt | Residual handler-error response branch sent an unsanitised message | `17d8056` |
| [#1847](#1847) | Low | CWE-59 | Snapshot | Legacy `snapshot.Open` read `csr.bin` without `O_NOFOLLOW` | `2174d21` |
| [#1848](#1848) | Info | — | csrfile/Bolt | `allocAligned8(0)` panic; no plaintext-transport startup warning | `17d8056` |

The two Info observations from the assessment that are **by-design and not
defects** were not "fixed" but hardened where safe: plaintext-transport-by-
default is deliberate for an embeddable engine (now surfaced with a loud
startup warning, #1848); the mapper's FNV shard hash is documented as not
resisting hash flooding and is not query-reachable (no change).

## Findings and remediation

### <a name="1843"></a>#1843 — High — WAL family lacks `O_NOFOLLOW` (CWE-59)

- **Vulnerability.** The WAL file family — the data file (`store/wal/writer.go`
  `Open`), the suffix temp (`writeSuffixTmp`, opened `O_TRUNC`), the
  post-rename reopen, the reader (`OpenReader`), and the `.lock` sentinel
  (`lock_unix.go`) — was opened without `O_NOFOLLOW`, while the snapshot layer
  already hardens the identical threat (`store/snapshot/safe_open_unix.go`
  `openSnapshotComponent`). Adopting or restoring a store directory from an
  untrusted source whose `wal` or `wal.tmp` entry is a symlink to a
  process-writable victim file let a checkpoint **truncate then overwrite**, or
  append the mutation stream to, an arbitrary file.
- **Fix.** A build-tagged `walNoFollow` constant (`syscall.O_NOFOLLOW` on
  linux/darwin/BSD, `0` elsewhere) is OR-ed into every production open of a
  fixed-name final component. The injected sim/testfs backends are deliberately
  left unmodified (not symlink-exposed). Behaviour-preserving for the regular
  files the writer creates; only a symlinked final component is rejected
  (ELOOP). New `store/wal/symlink_escape_test.go` proves rejection of a
  symlinked WAL/reader/`O_TRUNC`-temp with the victim file untouched, and that
  a normal WAL is unchanged. **Certified GO** by the storage-engine-auditor
  (durability, crash-safety, and ACID-D preserved; the flag is purely additive).

### <a name="1844"></a>#1844 — Medium — CSV unbounded fields-per-record (CWE-789/1284)

- **Vulnerability.** `graph/io/csv/reader.go` set `FieldsPerRecord = -1`. A
  single record of nothing but delimiters is accepted and makes `encoding/csv`
  allocate ~40 bytes of per-field metadata (the reader consumes only three
  columns), amplifying one accepted file into multi-GiB transient heap. Measured
  ~52× at 8 MiB of commas; at the 128 MiB `DefaultMaxBytes` cap ≈ 6.6 GiB — an
  untrusted-input OOM that also refuted the file's own "4–5× / well under 1 GiB"
  documentation.
- **Fix.** A quote-aware `fieldGuardReader` caps a record at 65536 fields and
  rejects the flood with `ErrTooManyFields` **before** `encoding/csv` allocates
  it. Delimiters and newlines inside a quoted field are treated as literal
  content, so a single large quoted field (bounded by the byte cap) and
  legitimate multi-column CSVs still parse; the guard engages for a single-byte
  delimiter (the norm). The misleading peak-memory doc comment was corrected.
  New guard tests cover quote-awareness, per-record reset, and flood rejection;
  the pre-existing wide-row test was updated from the old, weaker "accepted,
  bounded by MaxBytes" contract to assert rejection.

### <a name="1845"></a>#1845 — Medium — No engine-wide inbound-decode ceiling (CWE-770)

- **Vulnerability.** The packstream decoded-collection budget (128 MiB) and wire
  budget (16 MiB) were strictly per-connection, with no aggregate accounting.
  Because HELLO decodes before authentication, that per-connection cap times
  `MaxConnections` (~131 GiB at the default 1024) was reachable pre-auth with no
  bound — a memory-exhaustion denial of service.
- **Fix.** A per-Server `packstream.InboundBudget` (an atomic pool) is shared by
  every connection's pooled decoder. The decoder charges its decoded-collection
  cost against the pool in 1 MiB reservation steps inside `chargeDecoded` and
  returns the bytes when the decode completes (a `defer` release in the serve
  loop plus a `Reset` self-heal). When the pool is drawn down, further decodes
  are rejected with a transient `Neo.TransientError.General.OutOfMemoryError` and
  the connection survives — backpressure, never a crash. `Options.MaxInboundDecodeBytes`
  mirrors the #1842 result-memory ceiling: it defaults to one eighth of
  GOMEMLIMIT when a soft memory limit is set, else unlimited (`-1` opts out; a
  positive value is verbatim). With no GOMEMLIMIT the budget is disabled — zero
  atomic operations, behaviour-preserving by default. **Certified GO** by the
  concurrency-architect (balanced accounting, no leak/double-release, aggregate
  bound, pre-auth coverage, no deadlock, hot-path contention bounded).

### <a name="1846"></a>#1846 — Low — Residual unsanitised handler-error branch (CWE-209)

- **Vulnerability.** One FAILURE branch in `bolt/server/serve.go` sent the raw
  `handlerErr.Error()`, bypassing `sess.sanitiseErr`. The branch is currently
  unreachable (only `errRecordWrite` reaches it, and that is intercepted just
  above), but a future handler returning an unwrapped internal error would
  disclose Go internals/paths to the client.
- **Fix.** The branch now routes through `sess.sanitiseErr`, making the
  no-internal-leak invariant structural rather than dependent on the current
  reachability accident. The sanitiser is covered by the existing
  info-disclosure sweep test.

### <a name="1847"></a>#1847 — Low — Legacy `snapshot.Open` without `O_NOFOLLOW` (CWE-59)

- **Vulnerability.** `store/snapshot/reader.go` read `csr.bin` via a plain
  `fsys.Open`. The path is unreachable from production recovery (which uses
  `LoadSnapshotFull` → `OpenComponent`), but the exported `Open` would follow a
  symlinked `csr.bin` in an untrusted directory.
- **Fix.** Switched to `fsys.OpenComponent` (O_NOFOLLOW on unix), consistent
  with the hardened full-load path. New tests: a symlinked `csr.bin` is
  rejected; a regular snapshot still loads unchanged.

### <a name="1848"></a>#1848 — Info — Defense-in-depth hardening

- **`allocAligned8(0)` panic.** The byte-backed csrfile reader panicked on
  `&backing[0]` for an empty image (sim-only path) before the length check.
  `allocAligned8` now returns `nil` for `n <= 0`, so `openBytes` yields a clean
  `ErrHeaderTooShort` — a fail-stop error, not a crash.
- **Plaintext-transport startup warning.** A default-constructed Bolt server
  with a nil `Options.TLSConfig` serves plain TCP, so credentials travel
  unencrypted. This is a deliberate default for an embeddable engine, but
  `NewServer` now emits a loud WARN (mirroring the `NoAuthHandler` warning) so a
  plaintext exposure is a conscious choice rather than a silent default.

## Verified sound (unchanged, re-confirmed)

- **Dependencies** clean (`govulncheck`). **Cypher** parameter/injection
  boundary airtight (parameters never re-enter the parser); the six built-in
  procedures are read-only introspection with no filesystem/network/exec;
  procedure registration is a Go-API-only trust boundary; internal errors are
  sanitised at the wire.
- **Deserialization** parse→validate→CRC→bound before allocating across
  csrfile/WAL/snapshot/manifest; the csrfile mmap `unsafe` path is provably
  in-bounds (`Header.validate` is exact-canonical and overflow-safe); no OOB, no
  overflow-driven allocation, no manifest path traversal.
- **Import** GraphML is XXE / billion-laughs / external-DTD safe (stdlib
  `xml.NewDecoder` with no `CharsetReader`/`Entity`, regression-tested); JSONL
  depth/line/aggregate bounded; the I/O packages take `io.Reader`/`io.Writer`,
  so path traversal is not reachable.
- **Crypto** `crypto/rand` for session IDs and `randomUUID`; constant-time auth
  comparison; secure-by-default auth (nil fails closed); no `net/http/pprof`; no
  `InsecureSkipVerify` in production; TLS `DefaultTLSConfig` is TLS 1.2-floor /
  AEAD-only.

## Verdict

GoGraph is **production-ready** from a security standpoint on `main` after this
remediation. The two conditions that previously gated the module's own
hostile-input threat model — the WAL symlink-escape (High) and the CSV
memory-amplification OOM (Medium) — are closed, along with the Bolt aggregate
inbound-decode ceiling (Medium) and the low/info hardening. All compliance
gates (TCK 3897, ACID via the storage-auditor certification, `-race`,
`govulncheck`) are green.
