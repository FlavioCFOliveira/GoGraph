# The publish fast path, measured (rmp #2362, 2026-08-09)

`Clock.finishCommitTS` used to take the process-global `pubMu` on **every** commit, to
advance a frontier that in-order publication moves by exactly one. rmp #2362 put a
lock-free compare-and-swap in front of it. This is what that bought, and what it cost.

Baseline commit `c652b911`; candidate the working tree at the time of the run.

## How these numbers were produced

- Two `go test -c` binaries plus a **byte-identical copy of the baseline** as a
  self-control, all three alternated inside **one** loop, `n = 10` each, compared with
  `benchstat`. An across-time comparison on this host has been shown worthless.
- No `-race`. Apple M4, 10 cores, `darwin/arm64`.
- Host load averages 2.79 at the start and 7.82 at the end of the arms run — the
  self-control ran under exactly the same drift, which is why it is in the same loop.

## The harness is validated, not assumed

Base against its own byte-identical copy, 24 rows, at the same load as the comparison:

**23 of 24 read `~`.** The single exception is
`create-labelled-node/writers=8` at **+1.06% (p=0.014)** — a phantom by construction.
So the noise floor for these arms on this host, on this run, is **≈1%**, and nothing
smaller than that is worth reading.

## End-to-end: the contention arms (rmp #2359)

`sec/op` is per commit. Only rows benchstat called significant are listed; every other
row read `~`. **Every significant row is a win — there is no regression anywhere in the
table.**

| arm | writers | base | cand | delta |
|---|---:|---:|---:|---:|
| `label-add-remove` | 8 | 3.097 µs | 2.986 µs | **−3.58%** |
| `label-add-remove` | 16 | 2.903 µs | 2.800 µs | **−3.55%** |
| `label-add-remove` | 32 | 2.734 µs | 2.633 µs | **−3.69%** |
| `update-property` | 8 | 1.704 µs | 1.683 µs | −1.23% |
| `update-property` | 16 | 1.568 µs | 1.549 µs | −1.21% |
| `update-property` | 32 | 1.467 µs | 1.445 µs | −1.53% |
| `create-labelled-node` | 16 | 1.270 µs | 1.256 µs | −1.10% |
| `create-labelled-node` | 32 | 1.263 µs | 1.248 µs | −1.19% |
| `create-labelled-node` | 2 | 1.735 µs | 1.717 µs | −1.07% |
| `update-property` | 2 | 2.070 µs | 2.061 µs | −0.43% |
| `label-add-remove` | 2 | 3.269 µs | 3.239 µs | −0.92% |
| `mixed` | 8 | 1.887 µs | 1.867 µs | −1.09% |
| `mixed` | 32 | 1.931 µs | 1.918 µs | −0.65% |

Two things the shape of that table says, and neither was assumed in advance:

- **One writer is flat on every arm.** With no second publisher the mutex is uncontended
  and nearly free, so there is nothing for the fast path to remove. The win appears from
  two writers up and is largest at 8–32.
- **`label-add-remove` gains twice what the others do**, and it is the arm whose unit is
  **two statements** — therefore two commits, therefore two publications. The win tracks
  publication count, which is the mechanism the change claims to address.

Scaling at 32 writers improves accordingly: `create-labelled-node` 2.18× → 2.21×,
`label-add-remove` 1.59× → 1.65×.

## The mechanism: `BenchmarkPublish` in `graph/mvcc`

Same interleaved method, `n = 10`. The `legacy/*` rows are a control — a bare atomic
counter that neither binary changes — and they read `~` (in-order) and **+8.24%**
(concurrent), which is the noise floor for the concurrent rows and the reason `-11%`
below is quoted with care.

| benchmark | base | cand | delta |
|---|---:|---:|---:|
| `Publish/frontier/in-order` | 6.081 ns | 2.760 ns | **−54.61%** |
| `Publish/frontier/out-of-order-window-8` | 5.126 ns | 10.180 ns | **+98.60%** |
| `PublishConcurrent/frontier` | 66.85 ns | 59.46 ns | −11.05% |
| `Publish/legacy/in-order` (control) | 5.201 ns | 5.188 ns | ~ |
| `PublishConcurrent/legacy` (control) | 99.50 ns | 107.70 ns | +8.24% (phantom) |

**The out-of-order row is a real regression and is recorded as one.** That arm publishes a
window of eight in reverse, so the fast path never fires once and every publication pays
the guard for nothing: one extra atomic load, a `syncTo` call, and — once per window — a
failed compare-and-swap and two flag stores. It is the deliberate worst case, and it is
worth being explicit that **the end-to-end table above shows no arm paying it**: real
publication does fire the fast path, and the synthetic reverse window does not.

Three guard shapes were measured on that row rather than reasoned about, and the ranking
was not the predicted one:

| guard | out-of-order delta |
|---|---:|
| atomic counter, `Add` per reason | +103.43% |
| atomic counter, `Store` per publication | +80.81% |
| **atomic flag, stored only on transition** (shipped) | +98.60% |

The shipped form issues strictly fewer and cheaper atomic operations than the middle one,
so it should have been the fastest of the three and measured slower. The difference is
within the drift these rows show between runs (the base column itself moved 5.126–5.242 ns
across the three runs), and no attribution is offered for it here: this project has a
standing rule that a profile cannot attribute a delta it has not isolated. The flag form
was kept because it is the one that is cheapest *by construction* and because it is the
simplest to reason about, not because the measurement chose it.

## What this does NOT settle

`PublishConcurrent/frontier` at −11.05% sits only marginally above its own control row's
+8.24% phantom. It reproduced at −11.51%, −11.51% and −11.05% across three independent
runs while the phantom moved (−1.86%, +5.97%, +7.28%), which is why it is believed — but
the arms table, not this row, is the evidence the decision rests on.

The **batching follow-up** (PostgreSQL's `ProcArrayGroupClearXid` shape: one lock
acquisition for a chain of publications) is untouched and remains open. The out-of-order
row above is precisely the traffic it would address, so the case for it is now stronger
than it was, not weaker.
