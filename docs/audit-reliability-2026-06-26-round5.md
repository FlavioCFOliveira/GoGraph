# Reliability Audit — 2026-06-26 (round 5)

A fifth empirical reliability pass at HEAD `df14357` (after round 4 closed
sprint 250). Five specialist clusters ran concurrently, each read-only and each
required to **reproduce a finding before reporting it**. The round was seeded by
a lead from round 4: the fix for `normalizeVarlenBounds` (#1788) showed the
parser's text pre-pass family shares a fragility class — a text rewrite intended
for one syntactic context misfiring in another. Round 5 confirmed sibling bugs.

## Method (empirical)

| Gate | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt` | clean |
| openCypher TCK | 3897, held before & after every fix |
| `go test -race ./...` (whole module) | green |
| packstream fuzz (`FuzzDecodeValue`, `FuzzDecodeFidelity`) | 2.18M execs, no crashers |
| normalize byte-scanner fuzz + adversarial tables | no panics, UTF-8 safe |
| graphml XXE / billion-laughs / external-DTD | inert (Go `encoding/xml`) |

Clusters: parser text pre-pass family; transaction-isolation + constraints;
wire/packstream fuzzing; analytics correctness; load + round-4-code concurrency.

## Findings → rmp sprint 251 (5 tasks; 1 backlog)

The new findings are one bug cluster in the parser's compact-subtraction
pre-pass — all three are the same root cause (mis-discriminating a binary `-`):

| # | Sev | Finding | Fix |
|---|---|---|---|
| 1796 | High | Compact subtraction on an identifier/key ending in `e`/`E` (`age-1`, `scope-1`, `m.age-1`) fails to parse — the float-exponent guard misfires on identifier letters | `5a03f3c` |
| 1797 | Medium | Compact subtraction after a closing `)` `]` `}` (`(5)-1`, `[5][0]-1`, `(5)-0x1`) fails — never space-separated; `normalizeNegHexOct` rewrote `(5)-0x1` to `(5)(-1)` | `5a03f3c` |
| 1798 | Medium | Compact subtraction after a hex/oct literal ending in `e`/`E` (`0x1E-1`) fails — same exponent guard (radix digit mistaken for mantissa) | `5a03f3c` |
| 1799 | Low | `FuzzDecodeValue` checked no-panic only, not value fidelity — coverage gap | `7a0a386` |
| 1800 | — | Feature gap: centrality package lacks closeness / eigenvector / Katz / harmonic — **backlog, needs scheduling** (net-new feature, not a defect) |

Fix: `endsWithDecimalFloatExponentMarker` (true only for a genuine decimal
mantissa ending in `e`/`E`, false for an identifier letter or a `0x`/`0o`/`0b`
radix digit); the value-closing set in `normalizeArithmeticMinus` extended to
`)` `]` `}`; `normalizeNegHexOct` treats `)` `]` `}` as binary-subtraction
context. Genuine exponents (`1.5e-3`, `1e-3`, `2E-01`) and unary radix minus
(`-0x1F`, `5 + -0x1`) preserved. Each task carries an empirical repro and a
regression gate (`cypher/arith_minus_regression_test.go` + `normalizeArithmetic
Minus` unit cases; `bolt/packstream/fidelity_test.go`).

## Certified sound (negative results — strong confirmation)

Four of the five clusters found **zero reproducible defects** after deep probing:

- **Transaction isolation & constraints — CERTIFIED.** No dirty read (reader
  blocks on `visMu` until commit); no partial-tx visibility (WAL-backed,
  multi-statement); read-your-writes + clean rollback; no lost update between
  concurrent explicit txns (single-writer serialisation); no uniqueness TOCTOU
  under concurrency (exactly 1 survivor); index seek == full scan across churn,
  no ghost rows; bulk loader faithful; csrfile round-trips structural edge cases
  and fail-stops on truncation. `BeginReadTx` is per-statement read-committed by
  documented design (not a defect).
- **Wire/packstream & importers — CERTIFIED.** packstream codec is fidelity-
  and DoS-safe (byte budget, 128 MiB decoded-memory budget, `maxValueDepth=128`,
  32-bit wrap guard); all 10 normalize byte-scanners are panic- and UTF-8-safe
  under fuzzing (no `#1788` siblings of the *panic* class); graphml XXE/billion-
  laughs inert; csv/jsonl bounded + typed-error; all 26 examples build and run.
- **Analytics correctness — CERTIFIED.** Flow (Dinic/EK/PR agree vs brute force
  over 5000 random instances incl. parallel edges/self-loops/zero-cap), MCMF vs
  SPFA reference (2200 instances), capacity-overflow guard, Leiden (edge cases +
  50-run determinism + cross-call pool `-race` + modularity reference),
  LabelPropagation, PageRank (reference match + parallel bit-identity + damping
  validation), and all 15 generators (byte-deterministic across rebuilds). The
  round-4 `BuildFromAdjListLive` is byte-identical on the nil path and matches a
  manual ghost-edge filter over 2000 trials (weights+handles arc-aligned).
- **Round-4 code under concurrency — CERTIFIED.** `LiveNodeFilter`/`BuildFrom
  AdjListLive` are only reached inside `Engine.Run`'s `View` (`visMu.RLock`),
  mutually exclusive with writers' `visMu.Lock` — no torn tombstone/adjacency
  read (verified under `-race` with concurrent DELETE + path-projection MATCH);
  `expr.Compare`'s cross-numeric branch is pure/stateless; Bolt connection flood
  is bounded (non-blocking semaphore, zero leaked goroutines under goleak);
  cancel/drop error paths leak no goroutines.

## Status

Build, vet, gofmt, the openCypher TCK (3897), and `go test -race ./...` are all
green after remediation. Sprint 251 is closed (4 tasks). #1800 (centrality
variants) is a tracked feature gap awaiting scheduling, not a reliability defect.
