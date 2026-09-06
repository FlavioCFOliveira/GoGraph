# Raw data — `v0.14.0`

Backing evidence for [`../v0.14.0.md`](../v0.14.0.md). Everything here was produced in a
single session on one host, with both arms pre-compiled before any timing.

## Arms

| Arm | Tree | Commit | Role |
|---|---|---|---|
| `A` | detached worktree of tag `v0.13.0` | `b439283e` | base |
| `B` | detached worktree of `release/0.14.0` | `f8a3ac0b` | head |
| `B2` | a **second, independent build of the same `v0.14.0` tree** | `f8a3ac0b` | **noise floor** |

`go build` is deterministic here under `-trimpath`, so `B2` comes out **byte-identical to
`B`** — verified by sha256 for all 11 packages in `binary-sha256.txt`. `B` vs `B2` is
therefore the same binary against itself: a pure host-noise floor, measured **in the same
rounds, in the same interleaving, under the same load** as the signal it is the yardstick
for.

Four packages — `graph`, `graph/mvcc`, `internal/metrics/prometheus`, `store/wal` —
compile to binaries that are byte-identical **between the two releases** as well. On those,
`A` vs `B` is a second floor, measured **across** the arms.

**Build mode is stated because allocation counts are not build-invariant:** plain build,
**no `-race`**, `-trimpath`.

## Files

| File | What it is |
|---|---|
| `binary-sha256.txt` | sha256 of all 33 test binaries; proves `B` == `B2`, and which packages are unchanged across the releases |
| `build-exits.log` | 33 builds, the exit code of each |
| `blocks.sh` | the block table: label, binary, benchmark selector, whether the goroutine ladder applies |
| `campaign.sh` | the interleaved runner |
| `size.sh`, `sizing.log` | the sizing run that set the round count from a **measured** per-arm cost |
| `classify-benchmarks.py` | classifies every benchmark as genuinely concurrent or serial |
| `concurrency-classification.txt` | its output: which benchmarks make `-test.cpu` a real goroutine ladder |
| `analyse.py` | integrity check + median/spread/Mann-Whitney over the raw files |
| `load1.py` | locale-proof 1-minute load reader (see the note below) |
| `exits.log` | every invocation's exit code, read from **here**, never from a pipeline's status |
| `loadavg.log` | `uptime` bracketed **before and after** every single invocation |
| `progress.log` | per-round gate value, start, completion and mirror timestamps |
| `<block>_<arm>.txt` | the benchmark result lines |
| `<block>_<arm>.stderr` | that invocation's stderr, kept **separate** — never `2>&1` |
| `integrity.txt` | per-block line-integrity verdict and per-arm sample counts |
| `benchstat-*.txt` | the canonical statistical comparisons |

## Two harness defects were found and fixed before any figure was taken

Both were caught by the sizing run, which is why one exists.

1. **The field delimiter collided with the regexes.** The block table used `|` to separate
   its fields, and the benchmark selectors are regexes whose alternation is also `|`, so
   `read` split the selector itself. The `cy` block exited 1 with 0 result lines
   (`testing: invalid regexp ... missing closing )`). Fixed by delimiting on `#`.

2. **The load gate could not fire.** This host's locale is `pt_PT`, so `uptime` prints
   `10,01`. The obvious shell gate — `tr ',' '.'` then `awk '$1 > 2.5'` — compares
   `"10.01" > "2.5"` as **strings**, because a `.`-bearing field is not numeric under a
   comma-radix locale, and silently returns false. A gate built that way never fires, and
   the report would have claimed a quiet host it never actually checked. Replaced with
   `load1.py`, which reads `os.getloadavg()` and has no text step at all.

## Spotlight

The previous comparison
([`../v0130-vs-v0140-2026-09-06.md`](../v0130-vs-v0140-2026-09-06.md)) could report no
timing at all, because Spotlight was indexing the 1.9 GiB worktrees the experiment itself
had created and held the host at load 2.86–19.73. Here the worktrees were created under a
directory whose name ends in **`.noindex`**, which Spotlight skips. `mds_stores` did not
appear in the CPU top through the campaign.

## Added after the first draft of this README

| File | What it is |
|---|---|
| `integrity.txt` | per-block line-integrity verdict; 0 malformed, n=6 exactly, arms matched |
| `noise-floor.txt` | Floor 1 and Floor 2, pooled **and** stratified by magnitude band |
| `adjudication.txt` | the bar per band, derived from same-code data only, and every significant row judged against it |
| `changes-only.txt` | the first-pass change list, kept so the tightening in `adjudication.txt` is auditable |
| `effects-all.txt` | every benchmark in every block, effect beside its own same-binary floor |
| `ladder.txt` | the 1/8/64/256/1024 tables, including the three ladder-shaped floors |
| `footprint.txt`, `footprint.sh`, `footprint_{A,B,B2}.txt` | `graph/index/count` `BenchmarkStoreFootprint`, at the default GOMAXPROCS |
| `benchstat-effect-<block>.txt` | canonical A→B comparison per block |
| `benchstat-floor-<block>.txt` | canonical B vs B2 (same binary) comparison per block |
| `release-accuracy` | `make release-accuracy VERSION=v0.14.0` passed with `MAKE_EXIT=0` |

## A third harness defect, found during cleanup

The two defects above were found by the sizing run, before any figure was taken. A third
was found **after** the report was drafted, while spot-checking the published evidence
against the published claims — which is the reason to do that.

**A later, aborted run overwrote the campaign's mirrored logs.** A short supplementary
footprint run was first attempted with `for arm in $ARMS` under **zsh**, which — unlike
bash — does **not** word-split an unquoted parameter. `$ARMS` was passed as the single
word `"A B B2"`, every binary path was wrong, and all four invocations exited **127**. That
attempt then ran `cp .../exits.log .../loadavg.log ... $MIRROR/`, which **clobbered the
campaign's `exits.log` (252 lines → 4), `loadavg.log` (504 → 8) and `progress.log`**.

For a while this directory therefore contained an `exits.log` showing four `EXIT=127`
lines, while `../v0.14.0.md` claimed 252 invocations all exiting 0. **The published
evidence contradicted the published claim, and would have read as a failed campaign.**

All three logs were restored from the authoritative run directory and re-verified:
252 `EXIT=` lines, **252 of them `EXIT=0`, zero non-zero**; 252 `BEFORE` + 252 `AFTER`
brackets; 6 `GATE_OPEN`, 6 `ROUND … COMPLETE`, `CAMPAIGN_ALLDONE` present. All 84
data/`stderr` files were then compared to the authoritative copy by sha256 — **all 84
match** — and `integrity.txt` was regenerated **from this directory** rather than from the
run directory, so the committed verdict is about the committed data.

**The main campaign was never affected.** It ran under `bash` (`nohup bash campaign.sh`),
and its log shows the arms split correctly: exactly **84 invocations per arm** across
`arm=A`, `arm=B`, `arm=B2`. Only the mirrored copies of its logs were damaged, and only
after it had finished.

**One further caution for anyone verifying these files.** `grep` on this host is `ugrep`,
which can exit quietly with no output where GNU grep would match. The exit-code and
bracket counts above were all recounted with `python3`, not with `grep`; an empty `grep`
result is not evidence of absence here.
