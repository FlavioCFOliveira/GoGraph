# Example 06 — CSV import and export

## What it demonstrates

The graph serialisation round-trip: read an edge-list CSV into the
mutable `adjlist` builder with `csv.ReadInto`, then write the same
graph back out in two interchange formats — CSV via `csv.Write` and
newline-delimited JSON (JSON Lines / NDJSON) via `jsonl.Write`.

## Domain / scenario

A three-edge directed cycle between three people. Each line of the input
CSV is a `src,dst,weight` edge; the leading `#` line is a comment the
reader skips:

```
# 3 example edges
alice,bob,1
bob,carol,2
carol,alice,3
```

The same graph is then emitted twice: once as CSV (one edge per row)
and once as JSON Lines (one node record per node, then one edge record
per edge). Both serialisations iterate nodes by ascending `NodeID` —
which the mapper assigns in insertion order — so the output is
byte-stable across runs and machines.

## How to run

```sh
go run ./examples/06_csv_import
```

## Expected output

```
Ingested 3 rows

CSV out:
alice,bob,1
bob,carol,2
carol,alice,3

JSON Lines out:
{"type":"node","id":"alice"}
{"type":"node","id":"bob"}
{"type":"node","id":"carol"}
{"type":"edge","src":"alice","dst":"bob","weight":1}
{"type":"edge","src":"bob","dst":"carol","weight":2}
{"type":"edge","src":"carol","dst":"alice","weight":3}
```

## Key APIs

- `graph/io/csv.ReadInto` — parse an edge-list CSV (skipping `#` comments) into an `adjlist.AdjList[string, int64]`, returning the row count.
- `graph/io/csv.DefaultOptions` — the default CSV layout (`,` delimiter, no header, `#` comment character).
- `graph/io/csv.Write` — serialise the adjacency list back to CSV, one edge per row.
- `graph/io/jsonl.Write` — serialise the same graph as JSON Lines: a node record per node followed by an edge record per edge.

## Further reading

- [`graph/io/csv`](../../graph/io/csv) — CSV edge-list reader and writer
- [`graph/io/jsonl`](../../graph/io/jsonl) — JSON Lines reader and writer
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder both formats target
- [Example 01 — basic shortest paths](../01_basic) — the minimal build-and-query flow
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
