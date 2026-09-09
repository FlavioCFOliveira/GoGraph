# Raw data — `v0.14.1`

Backing evidence for [`../v0.14.1.md`](../v0.14.1.md). Everything here was produced in a
single session on one host, with all three arms pre-compiled before any timing.

## Arms

| Arm | Tree | Commit | Role |
|---|---|---|---|
| `A` | `git archive` of tag `v0.14.0` | `7c59a02c` | base |
| `B` | `git archive` of `release/0.14.1` at `HEAD` | `efd32fb9` | head |
| `B2` | a **second, independent compilation of the same `v0.14.1` tree** | `efd32fb9` | **noise floor** |

The arms are extracted with `git archive` rather than checked out as worktrees, so the
measurement writes nothing into the repository's git administrative area and there is no
worktree to forget to remove. Each tree is extracted under a `.noindex` directory so
Spotlight does not index it and contaminate the host — it did exactly that during an
earlier campaign.

`go build` is deterministic here under `-trimpath`, so `B2` comes out **byte-identical to
`B`** — verified by sha256 for **all 18** packages in `binary-sha256.txt`. `B` vs `B2` is
therefore the same binary against itself: a pure host-noise floor, measured **in the same
rounds, in the same interleaving, under the same load** as the signal it is the yardstick
for.

Four packages — `graph`, `graph/index/count`, `graph/mvcc` and
`internal/metrics/prometheus` — compile to binaries that are byte-identical **between the
two releases** as well. On those, `A` vs `B` is a second floor, measured **across** the
arms, which catches what a within-arm floor structurally cannot.

Note the difference from the `v0.14.0` campaign: `store/wal`'s test binary is **no longer**
byte-identical between the releases, because `store/wal/writer.go` changed in this window.
The set of cross-arm floors is measured per release and is not carried forward.

**Build mode is stated because allocation counts are not build-invariant:** plain build,
**no `-race`**, `-trimpath`.

## Files

| File | What it is |
|---|---|
| `binary-sha256.txt` | sha256 of all 54 test binaries; proves `B` == `B2` for every package, and which packages are unchanged across the releases |
| `build-exits.log` | 54 builds, the exit code of each, read from **here** |
| `pkgs.sh` | the package list and the binary stem each one is compiled to |
| `build.sh` | the pre-compilation pass — every binary built before any timing, so no compiler CPU lands inside a measurement |
| `blocks.sh` | the block table: label, binary, benchmark selector, whether the goroutine ladder applies, which arms the block can run on |
| `campaign.sh` | the interleaved runner |
| `size.sh`, `sizing.log` | the sizing run that set the round count and the block set from a **measured** per-arm cost |
| `analyse.py` | integrity check + median/spread/Mann-Whitney over the raw files |
| `report.py` | integrity, then the two floors, then the magnitude bands, then adjudication, then effects — in that order |
| `apidiff.py`, `fielddiff.py`, `godocdiff.sh` | the exported-surface diff between the arms: top-level declarations, struct fields, and a `go doc` cross-check |
| `load1.py` | locale-proof 1-minute load reader (see the note below) |
| `exits.log` | every invocation's exit code, read from **here**, never from a pipeline's status |
| `loadavg.log` | `uptime` bracketed **before and after** every single invocation |
| `progress.log` | per-round gate value, start, completion and mirror timestamps |
| `<block>_<arm>.txt` | the benchmark result lines |
| `<block>_<arm>.stderr` | that invocation's stderr, kept **separate** — never `2>&1` |
| `integrity.txt` | per-block line-integrity verdict and per-arm sample counts |
| `noise-floor.txt` | both floors, pooled and stratified by magnitude band |
| `adjudication.txt` | every `p<0.05` row with its band and the bar it had to clear |
| `effects-*.txt` | the per-block effect tables |
| `benchstat-*.txt` | the canonical `benchstat` comparisons |

## The discipline, and the defect each rule was paid for

Every rule below exists because its absence has already produced a wrong published figure
on this project.

1. **stderr goes to its own file, never `2>&1`.** A log line landing mid-result silently
   removes samples and `benchstat` does not warn. A previous campaign lost 60 of 60 result
   lines in one block to exactly this.
2. **Exit codes are appended to `exits.log` and read from there.** A `… | tail` pipeline
   reports `tail`'s status, and a real `Error 1` has been masked that way here.
3. **`uptime` is bracketed before and after every single invocation.** A number without its
   load conditions cannot set a threshold.
4. **The load gate reads `os.getloadavg()` and has no text step.** This host's locale is
   `pt_PT`, so `uptime` prints `10,01`; the obvious shell gate compares `"10.01" > "2.5"`
   as **strings**, because a `.`-bearing field is not numeric under a comma-radix locale,
   and silently returns false. A gate built that way never fires, and the report would
   claim a quiet host it had never checked. See `load1.py`.
5. **The block table is delimited on `#`, not `|`.** The selectors are regexes whose
   alternation is also `|`, so a `|` delimiter splits the selector itself and the block
   exits 1 with 0 result lines.
6. **The arm order rotates every round**, so no arm systematically inherits another's cache
   and thermal aftermath.
7. **The noise floor is measured first**, before any delta is attributed, and it is
   measured in the same rounds under the same load as the signal.
8. **Every kept line is validated** against `^Benchmark\S*\s+\d+\s+`, and the arms'
   benchmark sets and sample counts are asserted equal before any comparison is read.
