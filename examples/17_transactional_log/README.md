# Example 17 — Transactional log, checkpointing, and crash recovery

## What it demonstrates

The full durability path of GoGraph's WAL-backed store: committing
transactions to a write-ahead log, folding the log tail into a
self-sufficient on-disk snapshot with a **background checkpointer**, and
recovering the exact committed graph after a simulated crash. It also shows
the **shared-lock contract** that keeps the checkpointer ACID-safe — the
checkpointer and the writer hold the *same* mutex, so a snapshot can never
capture a half-applied transaction.

## Domain / scenario

A small directed social graph is built one transaction at a time. Each
transaction adds one edge and labels it:

| Transaction | Edge | Label |
|---|---|---|
| 1 | `alice → bob` | `KNOWS` |
| 2 | `bob → carol` | `KNOWS` |
| 3 | `carol → dave` | `KNOWS` |
| 4 | `alice → carol` | `FOLLOWS` |

While the transactions commit, a background checkpointer fires on a timer,
snapshotting the live graph and truncating the WAL. After the last commit
the example abandons every in-memory reference — modelling a process crash
— and reopens the store from disk alone, verifying that all four edges and
their labels survived.

The WAL and snapshot live under a directory created with `os.MkdirTemp`, so
the absolute path differs on every run and is never asserted on.

### The shared-lock ACID contract

The checkpointer takes its snapshot by reading the live in-memory graph. If
it read concurrently with a transaction's in-memory apply it could persist a
partially-applied transaction, violating **Atomicity**. To prevent that, the
example follows the contract documented on `checkpoint.New`: the writer
holds a single `sync.Mutex` across each transaction's `Begin → Commit`
window, and the *same* mutex is handed to `checkpoint.New`. The checkpointer
acquires that mutex before building the CSR snapshot, so a snapshot and a
commit can never overlap — the checkpointer observes either all of a
transaction's writes or none of them. Because the snapshot is written with a
mapper codec (`checkpoint.WithMapperCodec`) it is self-sufficient, so the
WAL can be truncated after each checkpoint without losing committed state.

## How to run

```sh
go run ./examples/17_transactional_log
```

## Expected output

The output is **non-deterministic**: the checkpoint cadence depends on
timing, so the `Checkpoint stats` line (checkpoint count, truncated bytes,
last duration) and the recovered WAL-op count vary per run. A representative
run:

```
Committed 4 transactions.
Checkpoint stats: {Checkpoints:1 WALTruncBytes:226 LastDurationNS:36872000 LastError:}

Recovered 4 ops from WAL (snapshot used: true).
  recovered alice -> bob with label "KNOWS"
  recovered bob -> carol with label "KNOWS"
  recovered carol -> dave with label "KNOWS"
  recovered alice -> carol with label "FOLLOWS"
```

The **deterministic invariant** — the one the regression test pins — is the
four `recovered …` lines: every committed edge and its label is present
after recovery, in commit order. The `Checkpoint stats` values and the
`Recovered N ops` count are volatile and are not asserted.

## Key APIs

- `store/wal.Open` — open the write-ahead log file that makes commits durable.
- `store/txn.NewStoreWithCodec` / `Store.Begin` / `Tx.AddEdge` / `Tx.SetEdgeLabel` / `Tx.Commit` — the transactional write path; each commit fsyncs its WAL frames then applies atomically to the in-memory graph.
- `store/checkpoint.New` / `WithMapperCodec` / `Checkpointer.Start` / `Stop` / `Stats` — the background checkpointer; `New`'s `storeMu` parameter is the shared lock that serialises snapshots against commits.
- `store/recovery.Open` — rebuild the graph after a crash from the snapshot plus any WAL tail; reports `SnapshotHit` and `WALOps`.
- `graph/lpg.Graph.HasEdgeLabel` / `AdjList.HasEdge` — verify the recovered edges and labels.

## Further reading

- [`store/wal`](../../store/wal) — write-ahead log package documentation
- [`store/txn`](../../store/txn) — transactional store and codecs
- [`store/checkpoint`](../../store/checkpoint) — background checkpointer and the shared-mutex contract
- [`store/recovery`](../../store/recovery) — snapshot + WAL recovery
- [Example 04 — persistence](../04_persistence) — the simpler save/reopen flow without a background checkpointer
- [Example 21 — typed recovery](../21_typed_recovery) — recovery with typed (non-string) node keys
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
