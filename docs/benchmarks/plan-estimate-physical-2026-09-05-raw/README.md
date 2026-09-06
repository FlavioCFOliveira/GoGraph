# rmp #2765 — cost of carrying the planner's estimate onto the physical plan

> **Note on the distribution archive.** The raw data files this document points at
> live in the GoGraph repository and are deliberately **not** shipped in the release
> tarball, which carries Markdown only (`.goreleaser.yaml`, rmp #2758). Read them at
> the tag in git; this README travels with the archive so the method is recorded even
> where the data is not.


Raw data for the "Cost on the ordinary query path" section of the rmp #2765 addendum
in `docs/explain-profile-honesty-audit-2026-09-03.md`.

## Environment

* Apple M4, 10 cores, darwin/arm64, macOS 26.5.2 (build 25F84)
* go1.27.1 darwin/arm64, PLAIN build (no `-race`, no `-cover`)
* Load average 2.3–4.4 throughout. **The host was NOT idle**, which is why the
  allocation counts are the claim and no timing claim is made.

## Method

Two (later six) test binaries were compiled up front with `go test -c` — one from
`git archive HEAD` (commit 44804e59) extracted to a scratch tree, one from the
working tree — and then run **interleaved**, one round of each arm per iteration,
10 rounds, `-benchtime=2s -count=1` per invocation. Interleaving rather than
running all of A then all of B is what keeps thermal drift and frequency scaling
out of the comparison.

Benchmark: the project's own `BenchmarkPlanReusePhases` on cypher-read-label-small
(`MATCH (n:N) RETURN count(n) AS c`), which decomposes the read path into
cumulative prefixes. `3build` is the phase this change touches; `4full` is the whole
read. Round 6 covers all four phases at `-benchtime=1s`.

## Arms

| file | arm |
|---|---|
| `r*_A_base*.txt` | HEAD (44804e59) |
| `r1_A2_base_repeat.txt` | HEAD again, same round — the NOISE FLOOR |
| `r1_B_head.txt`, `r2_B_head.txt` | this change, two build-option fields, split guard |
| `r2_C_nobranch.txt` | that tree with the per-operator recording branch DELETED |
| `r3_D_onepointer.txt` | collector folded into ONE build-option pointer |
| `r4_E_layoutprobe.txt` | HEAD plus 160 lines of NEVER-EXECUTED code inserted into `api.go` immediately before `buildOperatorRec` |
| `r5_F_final.txt`, `r6_F_final_allphases.txt` | final: one pointer plus `buildOperator`'s single short-circuit early-out |

## Results

Read the `benchstat-*.txt` files. In summary:

* **allocs/op and B/op are IDENTICAL in every comparison**, all samples equal,
  p=1.000. Nothing was added to the query path's allocation profile.
* Noise floor (`benchstat-noisefloor.txt`): `~ p=0.739` and `~ p=0.670`, geomean
  −0.02%. The methodology discriminates well below 1% even on this host.
* `benchstat-nobranch-vs-head.txt`: adding the per-operator recording branch is
  **not significant** (`~ p=0.093`, `~ p=0.481`, geomean +0.13%).
* `benchstat-layout-sensitivity.txt`: 160 lines of never-executed code moved
  **nothing** (`~ p=0.896`, `~ p=0.315`, geomean −0.16%), which REFUTES code layout
  as the explanation of the residual at this magnitude.
* The residual against HEAD fell across the two changes made in response:
  two fields + split guard +1.13% → one pointer +0.92% → one pointer + single
  guard +0.68% (geomean, `3build`+`4full`).
* `benchstat-base-vs-final-allphases.txt` shows the residual is not consistently
  localised: at `-benchtime=1s` `1parse`, `2snapshot` and `3build` are all `~`
  (p=0.165, 0.138, 0.197) while `4full` reads +1.85% — and `4full` adds only the
  result drain, which this change does not touch.
