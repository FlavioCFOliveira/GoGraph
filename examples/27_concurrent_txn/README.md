# Example 27 — Concurrent Transaction Isolation

## What it demonstrates

Transactional **isolation**, **atomicity**, and **consistency** of the
WAL-backed Cypher engine, certified under concurrency and the race detector.
Many writer goroutines move money between accounts while many reader goroutines
continuously observe an invariant that can only hold if the engine isolates
in-flight transactions from readers and never loses a concurrent update. It is
the only example whose runtime exercises `cypher.Engine.BeginTx` (multi-statement
explicit transactions), `cypher.Engine.RunInTx` (single-statement autocommit
writes), and `cypher.Engine.BeginReadTx` (read-only transactions) together under
contention.

## Domain / scenario

A bank clearing-ledger. Each account is a `(:ACCOUNT {id, balance})` node whose
integer `balance` is held in cents and keyed by a string account number backed by
a range index. A **transfer** debits one account and credits another by the same
amount, so the **sum of all balances is invariant** — money is neither created
nor destroyed. That conserved total is the observable the readers pin.

A seeded generator fixes the whole workload for a given `-seed`: the accounts and
their initial balances, and every transfer (source, destination, amount) assigned
to each writer. The ledger is **fully capitalised** — initial balances are
validated to exceed the largest possible aggregate debit on any single account —
so no account can ever go negative and every planned transfer commits. That keeps
the committed set, and therefore the final per-account state, deterministic:
because a transfer is a commutative delta on two accounts, replaying the committed
transfers in any order yields the same final balances, which the run computes up
front and asserts against after the concurrent phase.

## How to run

```sh
go run ./examples/27_concurrent_txn                 # small deterministic default
go run ./examples/27_concurrent_txn \
    -accounts 5000 -writers 16 -readers 32 \
    -ops-per-writer 5000 -max-amount 1000 -seed 7   # observable-scale run
```

Run it under the race detector to use it as a data-race + isolation
certification:

```sh
go test -race ./examples/27_concurrent_txn/...
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-accounts` | number of `:ACCOUNT` nodes | `32` | `5000` |
| `-writers` | concurrent writer goroutines | `4` | `16` |
| `-readers` | concurrent reader goroutines | `4` | `32` |
| `-ops-per-writer` | transfers each writer commits | `150` | `5000` |
| `-min-initial` | minimum initial balance (cents) | `1000000000` | — |
| `-max-initial` | maximum initial balance (cents) | `2000000000` | — |
| `-max-amount` | maximum transfer amount (cents; min is 1) | `1000000` | `1000` |
| `-sweep-ops` | transfers per writer-scaling-sweep level (0 disables) | `120` | — |
| `-seed` | RNG seed (fixes the data shape) | `1` | `7` |

`min-initial` must be at least `writers × ops-per-writer × max-amount` so the
no-overdraft invariant holds; `validate` rejects a configuration that violates it.
At larger scales keep `-max-amount` small (as in the example above) so the
guarantee is easy to satisfy.

## Expected output

Bare lines are deterministic **facts** pinned by the regression test; lines
prefixed with `# ` are volatile **telemetry** that varies per run and machine.

```
config.accounts=32
config.writers=4
config.readers=4
config.ops_per_writer=150
config.seed=1
accounts=32
transfers.planned=600
transfers.multi_statement=300
transfers.single_statement=300
initial_total=46625986168
# plan.debit_index_seek=true
transfers.committed=600
final_total=46625986168
conservation.holds=1
lost_updates=0
no_negative_balances=1
total_balance_invariant_holds=1
# run.elapsed=2.28s
# writer.transfers_per_s=262
# writer.mean_acquire_wait=11.45ms
# reader.observations=2414
# reader.observations_per_s=1056
# mem.heap_alloc=1.69 MiB
# scale.writers_1.transfers_per_s=264
# scale.writers_2.transfers_per_s=267
# scale.writers_4.transfers_per_s=265
```

The headline facts are the ACID certification:

- `total_balance_invariant_holds=1` — every reader observation equalled the
  seeded total; no reader ever saw a debit without its matching credit
  (**isolation**).
- `conservation.holds=1` and `final_total == initial_total` — money was neither
  created nor destroyed; every transfer applied atomically (**atomicity**).
- `lost_updates=0` — the final per-account state matched the deterministic
  replay; no concurrent read-modify-write interleaving lost a write
  (**consistency / serialisability of writers**).
- `no_negative_balances=1` — the fully-capitalised ledger stayed non-negative.

A single torn observation, lost update, or conservation failure makes `run`
return an error naming the violated property rather than reporting success — the
example surfaces a module isolation bug, it never hides one.

## Evidence it collects

For a **concurrency** subject (see [`docs/examples-standard.md`](../../docs/examples-standard.md)):

- **Writer throughput** (`# writer.transfers_per_s`) and **reader throughput**
  (`# reader.observations_per_s`) of the mixed workload.
- **Contention** (`# writer.mean_acquire_wait`) — the mean time a writer blocked
  acquiring a write transaction on the store's single-writer mutex, which rises
  with the writer count.
- **Scaling across worker counts** (`# scale.writers_N.transfers_per_s`) — the
  identical workload at 1, 2, 4 … writers on a fresh store. Throughput that stays
  flat as the writer count climbs is the observable signature of the single-writer
  serialisation that underpins isolation; scaling it up makes the effect starker.
- **Index seek** (`# plan.debit_index_seek`) — evidence the keyed lookup plans as
  a `NodeByIndexSeek` rather than a full label scan.
- **Live heap** (`# mem.heap_alloc`).

When scaling up, watch how writer throughput fails to climb with `-writers`
(serialised writers) while reader throughput and observation count grow with
`-readers` (readers only block for the brief window a write transaction holds the
visibility barrier).

## Key APIs

- `cypher.NewEngineWithStore` — a WAL-backed engine over a `txn.Store`.
- `cypher.Engine.BeginTx` / `cypher.ExplicitTx.Exec` / `Commit` / `Rollback` —
  multi-statement explicit write transactions (the debit-then-credit transfer).
- `cypher.Engine.RunInTx` (via `RunAny`) — single-statement autocommit writes.
- `cypher.Engine.BeginReadTx` — read-only transactions for the invariant reads.
- `cypher.Engine.Run` — the concurrent read path used by the other readers.

## Further reading

- [`cypher`](../../cypher) — package documentation (`cypher/exectx.go` holds the
  explicit-transaction and isolation contract).
- [docs/isolation-design.md](../../docs/isolation-design.md) — the isolation model.
- [docs/acid-audit.md](../../docs/acid-audit.md) — the ACID durability/atomicity design.
- [Example 17 — Transactional Log](../17_transactional_log) — WAL, checkpointing,
  and crash recovery of the durable store.
- [Example 20 — Concurrent Reads](../20_concurrent_reads) — the lock-free read
  contract of an immutable snapshot.
- [Example 25 — Software House API](../25_software_house_api) — a full app that
  serialises engine read/write access behind an HTTP surface.
