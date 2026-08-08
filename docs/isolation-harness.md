# The isolation harness — scripted, exhaustive, deterministic

`internal/isolationtest` runs a scripted concurrent scenario over **every**
interleaving of its steps, deterministically, and diffs the resulting transcript
against a golden file.

It is GoGraph's analogue of PostgreSQL's `src/test/isolation` ("isolationtester"),
adopted at commit `0ec3f048bfc15c8eb9933e8228b847593389da1b` (read 2026-08-07).
Task: rmp #2340, sprint 335.

---

## Why it exists

GoGraph already has a great deal of isolation testing: roughly 199 test
functions across `graph/lpg/mvcc_*_test.go` and `store/txn/`, the randomised DST
battery in `internal/sim`, and the crash-injection battery in
`internal/crashinject`. None of them enumerates the **interleavings** of a
scripted scenario — every concurrent isolation test either fixes one
interleaving by hand or samples the space at random.

That gap has a cost on the record:

- **rmp #2333** was a torn total that could not be reproduced.
- **rmp #2336** exists because a torn-total sighting from 2026-08-06 is still
  unexplained and has to be chased with a *standing randomised search*.

A randomised search can **find** an anomaly. It cannot **certify** that a
scenario is free of one, and it reproduces badly. This harness answers the
complementary question — *"over every interleaving of these steps, is the
observable outcome the expected one?"* — and answers it the same way every run.

It **complements** the existing tests; it replaces none of them.

---

## The model

A **spec** declares:

| Part | Meaning |
|---|---|
| `Setup` / `Teardown` | run once per permutation on a control session |
| `Session` | a scripted actor: its own goroutine, its own `cypher.Session` |
| `Session.Setup` / `Teardown` | run once per permutation on that session |
| `Step` | one named unit of work; **names are unique across the spec** |
| `Permutations` | optional explicit interleavings; when empty, **all** are run |

A step body is exactly one of:

- **`Query`** — Cypher, run in the session's open transaction, or autocommit when
  there is none.
- **`Ctl`** — `BEGIN`, `BEGIN READ`, `COMMIT`, `ROLLBACK`. PostgreSQL expresses
  these as ordinary SQL because they *are* SQL there; in GoGraph they are API
  calls, so the harness names them.
- **`Hook`** — arbitrary Go. The escape hatch for what a query language cannot
  express: a rendezvous between sessions, a deliberate wait, a fault injected at
  a precise point. PostgreSQL needs no equivalent — SQL has `pg_sleep` and
  advisory locks; Cypher has neither.

### Enumeration

PostgreSQL's "piles" recursion (`isolationtester.c`,
`run_all_permutations_recurse`), re-implemented in Go: each session's remaining
steps are a pile, and at each position the next step may be drawn from any
non-empty pile. Every order-preserving interleaving is produced exactly once.

The count is the multinomial **(Σnᵢ)! / Πnᵢ!**, and it grows fast:

| sessions × steps | permutations |
|---|---:|
| 2 × 3 | 20 |
| 2 × 4 | 70 |
| 3 / 4 / 3 | 4 200 |
| 3 × 5 | 756 756 |

`isolationtest.CountPermutations` computes it **without** building them, so a
spec's test-layer assignment is derived rather than guessed.

---

## What was deliberately NOT copied from PostgreSQL

**The lex/yacc spec language.** PostgreSQL needs a text format because its steps
are SQL strings shipped over libpq to separate backends. GoGraph's steps run
through an in-process `cypher.Engine`, so a Go literal is exactly as declarative,
is type-checked, and costs no scanner or grammar to maintain. The spec is *data*
either way, which is the property that matters.

**Lock-view polling for blocking detection.** `isolationtester` recognises a
blocked command by finding it in `pg_locks`, and therefore detects only
heavyweight-lock waits. GoGraph has no such view — and, more to the point, under
MVCC-only concurrency control an ordinary read or write acquires nothing and a
write-write collision is *refused* rather than queued. Blocking is detected here
by bounded timeout, which is what an out-of-process observer can actually see.

**The stabilisation markers** (`(*)`, `(otherstep)`, `notices <n>`). PostgreSQL
needs them because it launches the next step as soon as the previous one is
"done or deemed blocked", so two steps can be in flight and complete in either
order. This harness **awaits** each step before launching the next unless that
step is blocked, so within one permutation the order of completions is fixed by
construction and there is nothing to stabilise.

---

## Determinism, and why it holds

Within one permutation the runner launches a step and then awaits it. Only a step
still running after `BlockTimeout` is left in flight, and it is reported as
`<waiting ...>` at that point; its completion is reported when it is observed. So
the transcript's line order is fixed by the permutation, not by the scheduler.
The only way two runs can differ is if a step's **result** differs — which is
exactly what the golden file exists to catch.

Every permutation gets a **freshly built graph and engine**, so no permutation
can observe another's state. That is what makes `Runner.Only` — replaying a
single named permutation — reproduce the full run's transcript byte for byte, a
property asserted by `TestPermutationIsReRunnableByName`.

---

## Two ways a spec asserts

| Mechanism | Answers | Fails when |
|---|---|---|
| **Golden transcript** | did behaviour *change*? | any step's output differs from `testdata/<spec>.golden` |
| **`Runner.Observe`** | is behaviour *correct*? | an invariant rejects a step's structured rows |

Both are needed, and they fail for different reasons. Violations are
**accumulated**, not raised, so one run reports every interleaving that breaks
the property rather than only the first — "s2 tears whenever it reads between the
debit and the credit" is actionable; "some permutation failed" is not.

**Validate an observer before trusting its silence.** A clean exhaustive run
means "no violation" only if the observer *can* produce one. The first version of
the read-only-anomaly observer keyed on cell text; the `id` column renders
quoted, so it rejected all 4 200 interleavings. The mirror-image failure — an
observer that can never fire — is silent and far worse, which is why
`TestReadOnlyAnomalyExhaustive` probes its own observer with a known-illegal
observation before running.

---

## Shipped specs

| Spec | Layer | Perms | What it pins |
|---|---|---:|---|
| `lost-update` | short | 20 | P4 must be **refused**: two transactions writing the same object collide, and one is rejected |
| `write-skew` | short | 20 | A5B is **allowed** — the documented price of snapshot isolation (Berenson et al., SIGMOD 1995 §4.4.4) |
| `bank-transfer` | short | 20 | conservation: **every** interleaving reads a total of 100, never a torn 90 or 110 |
| `read-only-anomaly-named` | short | 1 | the Fekete/O'Neil anomaly, on PostgreSQL's own permutation |
| `read-only-anomaly` | soak | 4 200 | exhaustively, no reader sees a balance no commit state produces |

### A real divergence from the reference

PostgreSQL **defers** a `REPEATABLE READ` snapshot to the transaction's first
statement, so its `read-only-anomaly.spec` can open the observer session in
session setup and still have the snapshot taken wherever the read lands. GoGraph
takes the snapshot **eagerly** at `BeginReadTx`. Measured: with the observer's
`BEGIN` in session setup, its read returned `X=0, Y=0` for the reference's own
permutation — the anomaly was unreachable. Interleaving the `BEGIN` as a step
restores the degree of freedom PostgreSQL gets for free, and then the anomaly
reproduces (`X=0, Y=20`).

---

## Blocking, and the fact that GoGraph does not

Under MVCC-only concurrency control there is almost nothing for a GoGraph step to
block on. A read acquires nothing; a write acquires nothing; a write-write
collision is refused rather than queued; and the one genuinely exclusive hold —
the schema gate a DDL takes — is released when that DDL's own statement returns.
Because the harness awaits each step before launching the next, whatever a step
might have waited for has already been released.

That is a good property, and it is **why the exhaustive enumeration is tractable
here** where PostgreSQL's README has to warn that a spec using blocking "must
manually specify valid permutations". It is asserted, not assumed:
`TestNoGoGraphStepBlocks` fails if any shipped spec ever produces a
`<waiting ...>` line.

It also means no Cypher spec can exercise the blocking machinery, so
`blocking_test.go` manufactures a block with a `Hook` rendezvous and asserts:

1. the blocked step is reported as `<waiting ...>` within the timeout, not hung;
2. the run continues and the release is reported as `<... completed>`;
3. `waiting` precedes the releasing step, which precedes the completion;
4. handing a still-blocked session another step is reported as
   `INVALID PERMUTATION`, naming the stuck step — where PostgreSQL cancels after
   a timeout, this reports immediately and deterministically.

---

## The negative control

**A validation instrument that has never been shown to fail is not evidence.**

`fault_test.go` injects a known isolation fault — the bank transfer split across
**two** transactions, so the graph passes through a state in which 10 exists
nowhere — and asserts that the *same* invariant, reader and enumeration:

- report **nothing** against the correct (single-transaction) scenario, and
- report a torn read against the fault-injected one.

Measured: **24 torn reads** across the enumeration, each naming a permutation
that replays alone and reproduces (`TestViolationNamesAReplayablePermutation`).
The test also fails if *every* read is reported, which is what an
always-failing observer looks like rather than a detected fault.

The fault is injected in the **scenario**, not in the engine, deliberately:
patching production code to break isolation would test a build nobody ships,
whereas splitting the transfer injects the defect the invariant is actually about
into a build that is otherwise bit-identical to the one under test.

---

## Adding a spec

1. Write the spec as a Go literal beside the others.
2. Compute its size: `isolationtest.CountPermutations(spec)`.
3. Assign the layer from that number, per [docs/test-layers.md](test-layers.md).
   Short-layer specs must keep the package under its 60 s budget; anything
   larger goes to soak behind `testlayers.RequireSoak`, or names explicit
   `Permutations` (PostgreSQL's own escape hatch).
4. Generate the transcript with `GOGRAPH_UPDATE_GOLDENS=1 go test
   ./internal/isolationtest/`, then **read it** before committing.
5. If the spec has an invariant, add an `Observe` function — and probe it with a
   known-illegal observation so its silence means something.

Regenerating goldens without reading the diff is the one action that defeats the
entire harness.
