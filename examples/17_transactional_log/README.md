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

## How to run

```sh
go run ./examples/17_transactional_log                                   # small deterministic default
go run ./examples/17_transactional_log -accounts 50000 -transfers 2000000 -seed 7  # observable-scale run
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

## Key APIs

- `store/wal.Open` — open the write-ahead log file that makes commits durable.
- `store/txn.NewStoreWithOptions` (`txn.Options` with `NewStringCodec` + `NewInt64WeightCodec`) / `Store.Begin` / `Tx.AddEdge` / `Tx.Commit` — the transactional write path; `AddEdge(src, dst, amount)` records the amount as the durable edge weight via an `OpAddEdgeH` frame, and each commit fsyncs its WAL frames then applies atomically to the in-memory graph.
- `store/checkpoint.New` / `WithCommitSerialiser` / `WithMapperCodec` / `Checkpointer.Start` / `Stop` / `Stats` — the background checkpointer; `WithCommitSerialiser(store.RunUnderCommitLock)` serialises snapshots against commits under the store's own commit lock.
- `store/recovery.OpenCtx` — rebuild the graph after a crash from the snapshot plus any WAL tail; reports `SnapshotHit`, `WALOps`, and `IsClean` (fail-stop on a corrupt WAL).
- `graph/lpg.Graph.EdgeWeight` / `AdjList.HasEdge` / `LiveOrder` — verify the recovered transfers, their exact amounts, and the account count.

## Further reading

- [`store/wal`](../../store/wal) — write-ahead log package documentation
- [`store/txn`](../../store/txn) — transactional store, node codecs, and weight codecs
- [`store/checkpoint`](../../store/checkpoint) — background checkpointer and the commit-serialiser contract
- [`store/recovery`](../../store/recovery) — snapshot + WAL recovery
- [Example 04 — persistence](../04_persistence) — the simpler save/reopen flow without a background checkpointer
- [Example 21 — typed recovery](../21_typed_recovery) — recovery with typed (non-string) node keys
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
