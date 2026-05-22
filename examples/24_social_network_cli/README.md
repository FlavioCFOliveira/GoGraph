# Example 24 — Social Network CLI

A one-shot command-line tool that walks every layer of GoGraph end to
end on a small social-network domain:

- a labelled property graph (`graph/lpg`) for users, posts, comments,
  follows, likes and reply threads;
- WAL-backed transactional writes (`store/wal` + `store/txn`) and
  recovery from a snapshot plus the WAL tail (`store/recovery`);
- manual checkpoints via `store/snapshot.WriteSnapshotFull`;
- Cypher reads via `cypher.NewEngineWithStore` + `Engine.RunInTx`,
  streamed back as JSON Lines.

```
go run ./examples/24_social_network_cli <subcommand> -d <data-dir> [args]
```

## Schema

```
                +-----------------+
                |     User        |  username, display_name, created_at
                +-----------------+
                  ^   |    |   |
        FOLLOWS   |   |    |   | AUTHORED
                  |   |    |   v
                +-+   |    |   +-----------------+
                | ... |    +-> |     Post        |
                +-----+        +-----------------+
                                  ^         ^
                              ON  |         | LIKED
                                  |         |
                                +-----------------+
                                |    Comment      |
                                +-----------------+
                                   |    ^      ^
                          REPLY_OF |    |      | LIKED
                                   v    |      |
                                +-----------------+
                                |    Comment      |
                                +-----------------+
```

Labels and relationship types are declared as constants in
`schema.go`; the seed fixture and every helper share those names so a
rename surfaces compilation errors in one place.

## Subcommands

| Subcommand | What it does | Reply |
|---|---|---|
| `init     -d <dir>` | Creates the data directory if missing and writes an empty initial snapshot. Idempotent. | `{"data_dir":"<abs>","status":"ok"}` |
| `seed     -d <dir>` | Inserts the deterministic fixture (5 users, 8 FOLLOWS, 3 Posts, 5 Comments, 7 LIKED). | `{"seeded":<bool>,"status":"ok"}` |
| `query    -d <dir> [cypher]` | Runs a Cypher query (read or single-node write) and emits each record as one JSONL line. The query is taken from the positional argument or, if absent, from the entire stdin stream. | one JSON object per row |
| `snapshot -d <dir>` | Builds a CSR view of the current in-memory graph and writes a full snapshot (manifest + csr.bin + labels.bin + properties.bin) alongside the WAL. | `{"snapshot_dir":"<abs>","status":"ok"}` |
| `stats    -d <dir>` | Runs the eight `MATCH count(*)` queries and returns one alphabetically-keyed JSON object. | `{"authored":N,"comments":N,…,"users":N}` |

Exit codes:

- `0` on success;
- `1` on runtime failure (Cypher error, I/O error, validation);
- `2` on usage error (unknown subcommand, missing/malformed flags).

## End-to-end session

```bash
DATA_DIR=/tmp/social
go run ./examples/24_social_network_cli init  -d "$DATA_DIR"
go run ./examples/24_social_network_cli seed  -d "$DATA_DIR"
go run ./examples/24_social_network_cli stats -d "$DATA_DIR"
go run ./examples/24_social_network_cli query -d "$DATA_DIR" \
    'MATCH (u:User) RETURN u.username AS username ORDER BY username'
go run ./examples/24_social_network_cli snapshot -d "$DATA_DIR"
```

A representative `stats` reply on a freshly-seeded directory:

```json
{"authored":8,"comments":5,"follows":8,"likes":7,"on":5,"posts":3,"replies":2,"users":5}
```

A representative `query` (all users alphabetically) emits one JSONL
record per row:

```json
{"display_name":"Alice","username":"alice"}
{"display_name":"Bob","username":"bob"}
{"display_name":"Carol","username":"carol"}
{"display_name":"Dave","username":"dave"}
{"display_name":"Erin","username":"erin"}
```

The `query` subcommand also reads from stdin, so it pipes naturally
into `jq`:

```bash
echo 'MATCH (u:User)-[:FOLLOWS]->(v:User) RETURN u.username AS from, v.username AS to' \
  | go run ./examples/24_social_network_cli query -d "$DATA_DIR" \
  | jq -c '{from, to}'
```

## Architecture

```
        ┌──────────────┐
        │  os.Args     │
        └──────┬───────┘
               │
               v
        ┌──────────────┐        ┌─────────────────────┐
        │  dispatch    │  ───►  │  cmdInit / cmdSeed  │
        │  main.go     │        │  cmdQuery /          │
        │              │        │  cmdSnapshot / cmdStats │
        └──────┬───────┘        └─────────┬───────────┘
               │                          │
               │     openedStore.Close    │ openStore(ctx, dir)
               │       fsyncs the WAL     │
               v                          v
        ┌──────────────────────────────────────────────┐
        │  recovery.Open[string, float64](dir, opts)   │  read snapshot + WAL
        │  wal.Open(<dir>/wal)                         │  append-only WAL writer
        │  txn.NewStoreWithOptions(graph, wal, opts)   │  WAL-backed store
        │  cypher.NewEngineWithStore(store)            │  Cypher engine
        └──────────────────────────────────────────────┘
                                │
                                │  RunInTx / WriteSnapshotFull
                                v
                       ┌────────────────┐
                       │   data dir     │
                       │ ─ snapshot/    │
                       │ ─ wal          │
                       └────────────────┘
```

`store_helpers.go` centralises the wiring: `openStore` is the single
entry point every read/write subcommand uses, and `initEmpty` is the
single bootstrap. The shared `[string, float64]` codec pair
(`txn.NewStringCodec`, `txn.NewFloat64WeightCodec`) is pinned in
`dataDirOptions` so every layer agrees on encoding.

## Tests

```bash
go test -race ./examples/24_social_network_cli/...
```

The package's `cli_test.go` walks the full `init → seed → query →
snapshot → stats` cycle in one process, captures each subcommand's
stdout via `os.Pipe`, and compares the byte stream against the goldens
under `testdata/`. `TestMain` plugs in `go.uber.org/goleak` so every
test in the package doubles as a goroutine-leak check.

## History

The example originally documented three engine constraints — CREATE
with RETURN, multi-edge CREATE / MATCH+CREATE-relationship, and
cross-process snapshot label drift. All three were fixed in Sprint 56
of the gograph roadmap (tasks #498, #499, #500). The seed subcommand
still uses the direct `txn.Tx` API rather than Cypher CREATE so it
mirrors `examples/04_persistence` and stays independent of the Cypher
write planner.
