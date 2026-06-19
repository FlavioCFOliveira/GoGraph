# Example 06 — CSV import and export

## What it demonstrates

The graph interchange round-trip as a throughput benchmark: generate a
seeded edge-list CSV in memory, parse it back into the mutable `adjlist`
builder with `csv.ReadIntoCtx`, then re-serialise the resulting graph in
two formats — CSV via `csv.WriteCtx` and newline-delimited JSON (JSON
Lines / NDJSON) via `jsonl.WriteCtx` — measuring each leg and confirming
the data survives the round trip.

## Domain / scenario

A directed **follower graph**: handles follow other handles. Each node is
a 24-char hex id; each edge is one CSV row of `src,dst,weight`, where the
weight (in `[1, weight-max]`) stands in for interaction strength. The
seeded generator gives every node a random out-degree in
`[follows-min, follows-max]` to **distinct** other nodes — no self-loops,
no duplicate `(src,dst)` pairs — so the graph is *simple* and the row
count is exactly the edge count. That matters for the round-trip
invariant: a simple directed graph re-serialises to exactly as many CSV
rows as were ingested, with none collapsed by parallel-edge
deduplication. Fixing `-seed` fixes the data shape exactly.

The generated CSV carries a leading `# ` comment row documenting the file;
`csv.ReadInto` skips it, which is why the written CSV is a few bytes
smaller than the input (the comment is metadata, not an edge).

## How to run

```sh
go run ./examples/06_csv_import                          # small deterministic default
go run ./examples/06_csv_import -nodes 1000000 -follows-max 12 -seed 7  # observable-scale run
```

At the default scale the example does **not** dump the CSV or JSON Lines
to stdout — that would be large and would read as non-deterministic. The
round-trip output is written to in-memory buffers; only the deterministic
facts, the volatile `# ` telemetry, and a few sample lines of each format
are printed.

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-nodes` | number of follower nodes | `1000` | `1000000` |
| `-follows-min` | minimum out-degree per node | `3` | `3` |
| `-follows-max` | maximum out-degree per node | `6` | `12` |
| `-weight-max` | edge weight drawn from `[1, weight-max]` | `9` | `9` |
| `-sample` | sample lines of each format to print | `3` | `0` |
| `-seed` | RNG seed (fixes the data shape) | `1` | any |

The default builds a 1000-node graph (~4.5k edges) and round-trips it well
under the short-test 60 s budget; the large run is where the serialisers'
throughput becomes interesting.

## Expected output

The bare lines are deterministic **facts** (pinned by the regression
test). The `# `-prefixed lines are volatile **telemetry** — durations,
throughput, bytes and heap — that varies per run and per machine.

```
config.nodes=1000
config.follows=[3,6]
config.weight_max=9
config.seed=1
generated.rows=4475
ingested.rows=4475
graph.nodes=1000
graph.edges=4475
csv.rows_out=4475
jsonl.records_out=5475
jsonl.expected_records=5475
roundtrip.csv_reread_rows=4475
roundtrip.edges=4475
# parse.row_rate=1650074 rows/s       # telemetry — varies, never pinned
# csv.serialise.throughput=254.76 MiB/s
# jsonl.serialise.throughput=128.94 MiB/s
# bytes.in_csv=227.30 KiB
# bytes.out_csv=227.25 KiB
# bytes.out_jsonl=453.30 KiB
# mem.heap_alloc=981.80 KiB
# sample.csv (first 3 rows):
# 5b18db94b4d338a5143e6340,f1a6fbcd8704e119196fcc28,6
# sample.jsonl (first 3 records):
# {"type":"node","id":"5b18db94b4d338a5143e6340"}
```

Note `jsonl.records_out` (5475) = `graph.nodes` (1000) + `graph.edges`
(4475): JSON Lines emits one record per node, then one per edge.

## Evidence it collects

For an **interchange** subject the example reports (as `# ` telemetry):

- **Parse throughput** — `parse.row_rate` (rows/s) and `parse.throughput`
  (MiB/s) for `csv.ReadIntoCtx`.
- **Serialise throughput** — `csv.serialise.*` and `jsonl.serialise.*`
  (rows or records/s, and MiB/s) for `csv.WriteCtx` / `jsonl.WriteCtx`.
- **Bytes in/out per format** — `bytes.in_csv`, `bytes.out_csv`,
  `bytes.out_jsonl`, plus `bytes.per_edge_csv` and
  `bytes.per_record_jsonl` so the two encodings' density is comparable.
- **Live heap** — `mem.heap_alloc` after a forced GC, with the growth,
  total-alloc and GC count of the whole pipeline.

The deterministic results (as bare facts) are the ingested row count, the
written row/record counts, and the round-trip edge count. Scale the run up
with `-nodes` and `-follows-max` and watch the per-format throughput and
the JSON-Lines-vs-CSV byte ratio: JSON Lines is roughly twice the size
because every field name is repeated on every record.

## Key APIs

- `graph/io/csv.ReadIntoCtx` — parse an edge-list CSV (skipping `#` comments) into an `adjlist.AdjList[string, int64]`, honouring context cancellation; returns the row count.
- `graph/io/csv.WriteCtx` — serialise the adjacency list back to CSV, one edge per row; returns the row count.
- `graph/io/csv.DefaultOptions` — the default CSV layout (`,` delimiter, no header, `#` comment character, directed simple graph).
- `graph/io/jsonl.WriteCtx` — serialise the same graph as JSON Lines: a node record per node followed by an edge record per edge; returns the record count.
- `graph/adjlist.AdjList.Order` / `.Size` — the node and edge counts of the parsed graph.

## Further reading

- [`graph/io/csv`](../../graph/io/csv) — CSV edge-list reader and writer
- [`graph/io/jsonl`](../../graph/io/jsonl) — JSON Lines reader and writer
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder both formats target
- [Example 26 — social-scale benchmark](../26_social_scale_bench) — the reference end-state example this one mirrors
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
