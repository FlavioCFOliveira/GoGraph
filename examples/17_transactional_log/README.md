# Example 17 — Durable ledger: transactional log, checkpointing, and crash recovery

## What it demonstrates

The full durability path of GoGraph's WAL-backed store, driven by a financial
ledger: committing weighted transfers to a write-ahead log one transaction at a
time, folding the log into a self-sufficient on-disk snapshot with a
**background checkpointer**, and recovering the exact committed ledger — every
transfer with its **bit-exact amount** — after a simulated crash. The transfer
amount is the durable **edge weight**, so the example exercises the WAL
weighted-edge op (`txn.OpAddEdgeH`) and the `txn.WeightCodec` recovery path end
to end, and the recovered amounts are verified individually with
`lpg.Graph.EdgeWeight`.

The example ships **two** crash demonstrations. The default is an **in-process**
crash (fast and deterministic, pinned by the regression test): it abandons every
in-memory reference and reopens the store from disk. The `-real-crash` flag adds
a **real cross-process `kill -9`** demonstration: it re-execs this binary as a
child, commits a fixed number of fsynced transfers, then hard-kills the child
with `SIGKILL` **with a torn WAL frame in flight**, and the parent proves
recovery reconstructs exactly the durably-committed prefix — exercising OS-level
torn-frame handling the in-process crash never touches.

## Domain / scenario

A financial **ledger** modelled as a directed property graph: accounts are
nodes, transfers are directed edges, and each transfer's amount (in integer
cents) is the edge weight.

```
(:ACCOUNT {id})                       // id is a 24-char hex string
(:ACCOUNT)-[transfer]->(:ACCOUNT)     // weight = amount in cents
```

A seeded generator produces `accounts` accounts and `transfers` distinct
directed transfers. The ledger is a **simple directed graph**: at most one
transfer per ordered `(src, dst)` pair and no self-loops. That keeps the
per-amount verification unambiguous — `EdgeWeight` returns the weight of the
first edge for a pair, so one edge per pair means `EdgeWeight(src, dst)` is
exactly that transfer's amount — and it makes the **conservation identity**
exact: every transfer contributes its amount once to the source's debit total
and once to the destination's credit total, so the global debit and credit
totals are equal by construction and both equal the sum of all committed
amounts. No redundant `amount` property is stored; the edge weight is the
single source of truth.

Each transfer is committed as its own WAL transaction. While the transactions
commit, a background checkpointer fires on a timer, snapshotting the live graph
and truncating the WAL. After the last commit the example abandons every
in-memory reference — modelling a process crash — and reopens the store from
disk alone, verifying that every transfer survived with its exact amount.

The WAL and snapshot live under a directory created with `os.MkdirTemp`, so the
absolute path differs on every run and is never asserted on.

### The ACID coordination

The checkpointer takes its snapshot by reading the live in-memory graph. If it
read concurrently with a transaction's in-memory apply it could persist a
partially-applied transaction, violating **Atomicity**. Because this example
drives the `txn.Store` directly, it hands the checkpointer the store's own
commit serialiser via `checkpoint.WithCommitSerialiser`
(`txn.Store.RunUnderCommitLock`): the checkpointer runs its snapshot-capture
and WAL-truncate critical section under the store's private single-writer lock,
so no transaction can be between `Begin` and `Commit` while a snapshot is taken
or the WAL is truncated. Because the snapshot is written with a mapper codec
(`checkpoint.WithMapperCodec`) it is self-sufficient, so the WAL can be
truncated after each checkpoint without losing committed state.

### The two crash modes

- **In-process (default).** `run` commits the whole ledger, abandons every
  in-memory reference, and reopens the store from disk with `recovery.OpenCtx`.
  It is fast and byte-deterministic, so the regression test pins it — but
  because nothing was interrupted mid-write, it never exercises the WAL reader's
  torn-frame path.
- **Real cross-process `kill -9` (`-real-crash`).** `runRealCrashDemo` re-execs
  this binary (`os.Args[0]`) as a **crash-child** in a separate OS process. The
  child opens the store, commits `-crash-committed` transfers — each its own
  fsynced transaction, so each is durable on return — then appends a **torn
  partial WAL frame** (a header declaring a payload it never writes) to model an
  interrupted next commit, and hard-kills itself with `SIGKILL` while the store
  is still open. The parent waits for the child to die, confirms it was killed
  by `SIGKILL`, then reopens the data directory with `recovery.OpenCtx` and
  asserts the ledger balances to **exactly the durably-committed prefix**: every
  committed transfer present with its bit-exact amount, the interrupted transfer
  absent, no spurious edge conjured from the torn frame, and the benign torn
  tail detected (`IsClean`) rather than rejected as corruption. Any lost
  committed transfer, resurrected uncommitted transfer, or accepted torn frame
  is surfaced as a `DURABILITY:` error carrying a deterministic repro recipe —
  it is treated as a module durability defect, never hidden. The path needs a
  self-directed `SIGKILL` and is a clean no-op on platforms that lack it.

## How to run

```sh
go run ./examples/17_transactional_log                                   # small deterministic default (in-process crash)
go run ./examples/17_transactional_log -accounts 50000 -transfers 2000000 -seed 7  # observable-scale run
go run ./examples/17_transactional_log -real-crash                       # real cross-process kill -9 crash + recovery
go run ./examples/17_transactional_log -real-crash -crash-committed 2000 -accounts 60 -seed 7  # larger crash workload
```

## Scale and flags

| Flag | Meaning | Default | Large value |
|---|---|---|---|
| `-accounts` | number of `ACCOUNT` nodes | `100` | `50000` |
| `-transfers` | number of distinct directed transfer edges | `600` | `2000000` |
| `-min-amount` | minimum transfer amount in cents (inclusive) | `100` | `100` |
| `-max-amount` | maximum transfer amount in cents (inclusive) | `1000000` | `1000000` |
| `-seed` | RNG seed (fixes the deterministic data shape) | `1` | any |
| `-checkpoint-every` | background checkpointer age threshold (how often the WAL is folded into the snapshot) | `5ms` | `50ms` |
| `-real-crash` | run the real cross-process `kill -9` crash + recovery demo instead of the in-process one | `false` | `true` |
| `-crash-committed` | real-crash demo: durably-committed transfers before the kill | `50` | `2000` |

`-crash-committed` applies only with `-real-crash`; the demo sizes its plan to
`crash-committed + 1` transfers (the extra one is the deterministic in-flight
transfer the crash interrupts) and requires that count not to exceed
`accounts*(accounts-1)`. The internal `-crash-child-dir` flag is set only by the
parent when it re-execs the child; it is not meant to be passed by hand.

`transfers` must not exceed `accounts*(accounts-1)`: a simple directed graph
with no self-loops has at most that many distinct ordered pairs. The default
commits one fsynced transaction per transfer, so it stays comfortably under the
60 s per-package short-test budget while still exercising the WAL, the
background checkpointer (dozens of checkpoints), and recovery for real.

## Expected output

The deterministic **facts** (bare lines) are reproducible for a fixed `-seed`.
The **telemetry** lines (prefixed `# `) — commit throughput, the checkpoint
count and folded bytes, the snapshot footprint, and the recovery wall-clock —
vary per run and per machine and are never asserted. A representative run at
the default config:

```
config.accounts=100
config.transfers=600
config.amount=[100,1000000]
config.seed=1
nodes.accounts=100
edges.transfers=600
ledger.amount_sum=288773518
recovered.accounts=100
recovered.transfers=600
recovered.amount_sum=288773518
ledger.debit_sum=288773518
ledger.credit_sum=288773518
ledger.conserved=true
# commit.elapsed=3.5s             # telemetry — varies, never pinned
# commit.tx_rate=169 tx/s         # telemetry — varies, never pinned
# checkpoint.count=33             # telemetry — varies, never pinned
# checkpoint.wal_bytes_folded=209.50 KiB   # telemetry — varies, never pinned
# checkpoint.snapshot_bytes=27.18 KiB      # telemetry — varies, never pinned
# recovery.elapsed=359µs          # telemetry — varies, never pinned
# recovery.snapshot_hit=true      # telemetry — varies, never pinned
# recovery.wal_ops=45             # telemetry — varies, never pinned
```

The **deterministic invariants** the regression test pins are the recovered
counts (`recovered.accounts`, `recovered.transfers`), the bit-exact recovered
amount sum (`recovered.amount_sum == ledger.amount_sum`), and the conservation
identity (`ledger.debit_sum == ledger.credit_sum == ledger.amount_sum`, surfaced
as `ledger.conserved=true`). `run` additionally verifies every individual
transfer with `EdgeWeight(src, dst)` before reporting these, so a single
corrupted amount fails the run. The checkpoint stats, the recovered WAL-op
count, and the temp path are volatile and are not asserted.

### Real cross-process crash (`-real-crash`)

The `-real-crash` demonstration reports a different set of facts — the durability
outcome across a real `SIGKILL`. A representative run
(`-real-crash -crash-committed 50`):

```
config.accounts=100
config.seed=1
crash.committed_before_kill=50
recovery.transfers_replayed=50
recovery.torn_tail_detected=1
recovery.balance_conserved=1
recovery.is_clean=1
# crash.child_termination=signal:killed   # telemetry — how the child died
# recovery.elapsed=602µs                   # telemetry — varies, never pinned
# recovery.wal_bytes_replayed=17.72 KiB    # telemetry — the durable WAL prefix
# recovery.wal_ops=250                      # telemetry — ops replayed from the WAL
# recovery.snapshot_hit=false              # telemetry — WAL-only, no checkpoint
```

The **deterministic invariants** here are `recovery.transfers_replayed` equalling
`crash.committed_before_kill` (every fsynced transfer recovered, no more),
`recovery.torn_tail_detected=1` (the interrupted frame was seen), and both
`recovery.balance_conserved=1` and `recovery.is_clean=1`. The child's
termination, the recovery wall-clock, the replayed WAL byte count, and the WAL-op
count are telemetry.

## Evidence it collects

This is a persistence/recovery subject, so it reports the evidence from that row
of the taxonomy:

- **Commit throughput** — `# commit.tx_rate` (transactions per second; one
  fsynced transaction per transfer, so this measures the durable write path).
- **Checkpoint fold stats** — `# checkpoint.count` (how many times the WAL was
  folded), `# checkpoint.wal_bytes_folded` (WAL bytes truncated into snapshots),
  and `# checkpoint.snapshot_bytes` (the on-disk snapshot footprint).
- **Recovery wall-clock** — `# recovery.elapsed`, plus `# recovery.snapshot_hit`
  and `# recovery.wal_ops` to show how much state came from the snapshot vs the
  WAL tail.

When you scale it up (`-accounts 50000 -transfers 2000000`), watch how the
checkpoint count and folded-bytes grow with the commit stream, how the snapshot
footprint tracks the live graph rather than the full WAL, and how recovery time
is dominated by the WAL tail since the previous checkpoint rather than by the
whole history.

The `-real-crash` mode adds **durability-under-crash** evidence: it recovers the
WAL after a genuine `SIGKILL` and reports the recovery wall-clock
(`# recovery.elapsed`) and the durable WAL byte count replayed
(`# recovery.wal_bytes_replayed`), alongside the deterministic all-or-nothing
facts. Scale `-crash-committed` up to see recovery time and replayed bytes grow
linearly with the durable prefix.

## Key APIs

- `store/wal.Open` — open the write-ahead log file that makes commits durable.
- `store/txn.NewStoreWithOptions` (`txn.Options` with `NewStringCodec` + `NewInt64WeightCodec`) / `Store.Begin` / `Tx.AddEdge` / `Tx.Commit` — the transactional write path; `AddEdge(src, dst, amount)` records the amount as the durable edge weight via an `OpAddEdgeH` frame, and each commit fsyncs its WAL frames then applies atomically to the in-memory graph.
- `store/checkpoint.New` / `WithCommitSerialiser` / `WithMapperCodec` / `Checkpointer.Start` / `Stop` / `Stats` — the background checkpointer; `WithCommitSerialiser(store.RunUnderCommitLock)` serialises snapshots against commits under the store's own commit lock.
- `store/recovery.OpenCtx` — rebuild the graph after a crash from the snapshot plus any WAL tail; reports `SnapshotHit`, `WALOps`, `WALTailOffset` (the durable prefix boundary), `TailErr`, and `IsClean` (true for a nil or benign torn tail; false — with a matching function error — for genuine corruption).
- `graph/lpg.Graph.EdgeWeight` / `AdjList.HasEdge` / `AdjList.Size` / `LiveOrder` — verify the recovered transfers, their exact amounts, the recovered edge count, and the account count.
- `store/wal.Magic` / `wal.CurrentVersion` — the frame-header constants the `-real-crash` child uses to append a deliberately torn partial frame (see `appendTornFrame`).
- `os/exec.CommandContext` + `os.Args[0]` (`-real-crash`) — re-exec this binary as a crash-child that commits, tears, and `SIGKILL`s itself; `syscall.Kill(os.Getpid(), SIGKILL)` delivers the kill (see `selfkill_unix.go`).

## Further reading

- [`store/wal`](../../store/wal) — write-ahead log package documentation
- [`store/txn`](../../store/txn) — transactional store, node codecs, and weight codecs
- [`store/checkpoint`](../../store/checkpoint) — background checkpointer and the commit-serialiser contract
- [`store/recovery`](../../store/recovery) — snapshot + WAL recovery
- [Example 04 — persistence](../04_persistence) — the simpler save/reopen flow without a background checkpointer
- [Example 21 — typed recovery](../21_typed_recovery) — recovery with typed (non-string) node keys
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
