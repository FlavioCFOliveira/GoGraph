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
| `seed     -d <dir> [-users N] [-friends K] [-seed S] [-evidence]` | Inserts the deterministic fixture (5 users, 8 FOLLOWS, 3 Posts, 5 Comments, 7 LIKED) and, with `-users N`, an opt-in seeded synthetic population of `N` extra users with `K` FOLLOWS each. | `{"seeded":<bool>,"status":"ok"}` (+ `# ` telemetry with `-evidence`) |
| `query    -d <dir> [cypher]` | Runs a Cypher query (read or single-node write) and emits each record as one JSONL line. The query is taken from the positional argument or, if absent, from the entire stdin stream. | one JSON object per row |
| `snapshot -d <dir>` | Builds a CSR view of the current in-memory graph and writes a full snapshot (manifest + csr.bin + labels.bin + properties.bin + mapper.bin) alongside the WAL. The v3 manifest is self-sufficient: recovery can rebuild the graph from the snapshot alone, even when the WAL is empty or truncated. | `{"snapshot_dir":"<abs>","status":"ok"}` |
| `stats    -d <dir> [-evidence]` | Runs the eight `MATCH count(*)` queries and returns one alphabetically-keyed JSON object. | `{"authored":N,"comments":N,…,"users":N}` (+ `# ` telemetry with `-evidence`) |
| `plandiff -d <dir> [-scale N]` | Seeds a skewed synthetic content layer once, then runs two read scenarios on a reordering-ENABLED vs -DISABLED engine and prints the EXPLAIN plan-diff, an exact count-store work contrast, and the wall-clock for each. | `# ` telemetry + EXPLAIN plan-diff report |

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

## Scale and flags

The fixture above is a hand-written demonstration shape. To exercise the
module at a size where its behaviour is actually observable, the `seed`
subcommand takes three opt-in scale knobs that layer a **seeded,
reproducible synthetic population** on top of the fixture. All three are
off by default, so the deterministic output is unchanged unless you ask
for more.

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-users N` | Number of extra seeded `:User` nodes to append (0 = canonical fixture only). | `0` | `1000000` |
| `-friends K` | `:FOLLOWS` out-degree per synthetic user (must be `< users`). | `8` | `50` |
| `-seed S` | RNG seed; fixes the synthetic data shape exactly. | `1` | any `int64` |
| `-evidence` | Print `# ` telemetry after the JSON reply (on both `seed` and `stats`). | off | — |

The synthetic users carry the same `username` / `display_name` /
`created_at` shape as the fixture and use a namespaced key (`u_<hex>`), so
they collide with neither the fixture keys (`alice`..`erin`) nor the
Cypher `CREATE` synthetic keys. They are counted by `stats` and walked by
`FOLLOWS` traversals exactly like the fixture, so a scaled run drives the
WAL, recovery, CSR snapshot, and Cypher engine at size. The whole seed —
fixture plus synthetic population — commits in a single durable
transaction.

```bash
go run ./examples/24_social_network_cli init -d "$DATA_DIR"            # small deterministic default
go run ./examples/24_social_network_cli seed -d "$DATA_DIR" \
    -users 1000000 -friends 50 -seed 7 -evidence                       # observable-scale run
```

A given `-seed` yields byte-identical deterministic facts on any machine;
only the `# ` telemetry varies per run.

## Evidence it collects

With `-evidence`, the two heaviest subcommands report the
persistence-and-Cypher evidence dimensions from
[`docs/examples-standard.md`](../../docs/examples-standard.md). Facts are
bare lines (pinned by the tests); telemetry is `# `-prefixed and ignored
by the tests.

`seed -evidence` reports the synthetic build:

```
{"seeded":true,"status":"ok"}
# scale.users=1000000          # fact-shaped but scale-dependent, so telemetry
# scale.follows=50000000
# seed.elapsed=...             # varies per run / machine
# seed.node_rate=... nodes/s
# seed.edge_rate=... edges/s
# mem.heap_alloc=... GiB
# mem.heap_growth=... GiB
```

`stats -evidence` reports graph size, live heap, and per-query latency:

```
{"authored":8,"comments":5,"follows":50000008,"likes":7,"on":5,"posts":3,"replies":2,"users":1000005}
# graph.order=1000013
# graph.size=50000030
# mem.heap_alloc=... GiB
# q.users.latency=...          # one line per count query
# q.follows.latency=...
# ...
```

When you scale up, watch `mem.heap_alloc` and `# bytes`-shaped figures for
the resident footprint, and the `# q.*.latency` lines for which count
queries (label scans vs relationship scans) dominate at size.

## Query planning: the `plandiff` subcommand

**Scenario.** Real social graphs are skewed: most posts get no comments,
and the user population dwarfs any single content slice. Under that skew a
naively-written read can start from the wrong side and do far more work
than necessary. GoGraph's planner corrects two such cases automatically
with count-store-gated peepholes, and `plandiff` makes their effect
observable end to end.

**Objective.** Exercise both peepholes on a graph carrying the skew, and
surface — as explicit, comparable evidence — the difference between the
plan the engine *would* run naively and the plan it *does* run:

- **Anchor swap (#2090)** — `MATCH (p:Post)<-[:ON]-(c:Comment)` ("list
  every commented post with its comment"). Written anchored on `:Post`, the
  engine scans **every** post and walks its incoming `ON` edges. Because
  `ON` points `Comment → Post` and comments are far fewer than posts, the
  peephole re-roots the pattern onto `:Comment` — a forward `DirOut` expand
  — scanning `|Comment|` starting rows instead of `|Post|`.
- **Disjoint reorder (#2091)** — `MATCH (u:User), (c:Comment) RETURN
  count(*)` (sizing a user × comment moderation candidate space). The
  Cartesian re-initialises its inner plan once per outer row; the peephole
  drives the smaller `:Comment` side, re-initialising the inner plan
  `|Comment|` times instead of `|User|`.

**Purpose.** Provide auditable evidence that the optimisation fired and was
worthwhile. For each scenario `plandiff` prints the **EXPLAIN plan-diff**
(the chosen operator order / expand direction differs between the
reordering-ENABLED and -DISABLED engines), an **exact work contrast** read
from the count-store (scanned start rows for the anchor swap; inner
re-initialisations for the disjoint reorder — the db-hits-style figure the
cost model itself compares), and the **median wall-clock** ENABLED vs
DISABLED. On first run it seeds a deterministic synthetic content layer
(2000 `:User`, 1500 `:Post`, 100 `:Comment` each `:ON` a distinct post;
`-scale N` multiplies these) so the graph carries the skew; re-running
skips re-seeding.

```bash
go run ./examples/24_social_network_cli init -d "$DATA_DIR"
go run ./examples/24_social_network_cli seed -d "$DATA_DIR"
go run ./examples/24_social_network_cli plandiff -d "$DATA_DIR"
```

A representative anchor-swap plan-diff (the scan label and expand direction
both change; the `# ` lines report the exact and volatile evidence):

```
## scenario: anchor-swap
query: MATCH (p:Post)<-[:ON]-(c:Comment) RETURN c.id AS comment, p.id AS post
--- EXPLAIN (reordering DISABLED) ---
ProduceResults
└─ Projection
   └─ Selection
      └─ Expand (p)<-[:ON]-(c)
         └─ NodeByLabelScan [p:Post]
--- EXPLAIN (reordering ENABLED) ---
ProduceResults
└─ Projection
   └─ Selection
      └─ Expand (c)-[:ON]->(p)
         └─ NodeByLabelScan [c:Comment]
# anchor-swap.reordered=true
# anchor-swap.scanned_start_rows.disabled=1503
# anchor-swap.scanned_start_rows.enabled=105
# anchor-swap.scanned_start_rows.ratio=14.3x
# anchor-swap.elapsed.disabled=...     # volatile
# anchor-swap.elapsed.enabled=...
# anchor-swap.speedup=...x
```

The disjoint-reorder scenario prints the analogous diff — the
`CartesianProduct` children swap order (`[c:Comment]` drives before
`[u:User]`) — with `inner_reinitialisations` as its exact work figure.

## Architecture

```
        ┌──────────────┐
        │  os.Args     │
        └──────┬───────┘
               │
               v
        ┌──────────────┐        ┌─────────────────────────┐
        │  dispatch    │  ───►  │  cmdInit / cmdSeed /     │
        │  main.go     │        │  cmdQuery / cmdSnapshot /│
        │              │        │  cmdStats / cmdPlandiff  │
        └──────┬───────┘        └─────────┬───────────────┘
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
test in the package doubles as a goroutine-leak check, and the
cross-process tests build the binary and drive the lifecycle as separate
processes to prove durability and determinism survive a `kill -9`-style
restart.

`scale_test.go` covers the opt-in scale and evidence paths: it asserts
that the default (no flags) output is byte-for-byte unchanged, that a
scaled seed's deterministic counts match `5 + N` users and
`8 + N·K` follows, that a fixed `-seed` reproduces the same facts, and
that the JSON fact line is identical whether `-evidence` is on or off —
the only difference being the `# ` telemetry block, which the tests never
assert on.

## History

The example originally documented three engine constraints — CREATE
with RETURN, multi-edge CREATE / MATCH+CREATE-relationship, and
cross-process snapshot label drift. All three were fixed in Sprint 56
of the gograph roadmap (tasks #498, #499, #500). The seed subcommand
still uses the direct `txn.Tx` API rather than Cypher CREATE so it
mirrors `examples/04_persistence` and stays independent of the Cypher
write planner.
