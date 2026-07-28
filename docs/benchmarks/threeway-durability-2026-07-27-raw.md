
## Durability posture at the configuration measured

| Target | Durability |
|---|---|
| gograph-embedded | **none** — in-memory lpg.Graph, no WAL, no fsync |
| gograph-embedded-durable | **per commit** — WAL append + fsync before the write is visible (store.DB) |
| gograph-embedded[id] | **none** — in-memory lpg.Graph, no WAL, no fsync |
| gograph-bolt | **none** — the Bolt server here is wired to an in-memory engine; only transport differs from gograph-embedded |
| neo4j-bolt | **per commit** — the transaction log is forced on every commit (default) |
| memgraph-bolt | **every 100 000 tx** — default storage_wal_file_flush_every_n_tx=100000 (memgraph/memgraph, tests/e2e/configuration/default_config.py); per-commit durability is opt-in |

## Load time (20000 nodes, 199941 edges, UNWIND batches of 5000)

| Target | Load | Durability |
|---|---|---|
| gograph-embedded | 2.056s | **none** — in-memory lpg.Graph, no WAL, no fsync |
| gograph-embedded-durable | 2.564s | **per commit** — WAL append + fsync before the write is visible (store.DB) |
| gograph-embedded[id] | 2.064s | **none** — in-memory lpg.Graph, no WAL, no fsync |
| gograph-bolt | 2.344s | **none** — the Bolt server here is wired to an in-memory engine; only transport differs from gograph-embedded |
| neo4j-bolt | 4.054s | **per commit** — the transaction log is forced on every commit (default) |
| memgraph-bolt | 987ms | **every 100 000 tx** — default storage_wal_file_flush_every_n_tx=100000 (memgraph/memgraph, tests/e2e/configuration/default_config.py); per-commit durability is opt-in |

## Query latency (median of 9, after 3 warm-ups)

| Query | Description | gograph-embedded | gograph-embedded-durable | gograph-embedded[id] | gograph-bolt | neo4j-bolt | memgraph-bolt |
|---|---|---|---|---|---|---|---|
| `q01_point_lookup` | Indexed point lookup | 8µs | 9µs | 8µs | 86µs | 1.813ms | 218µs |
| `q01b_point_lookup_str` | Point lookup on a STRING key (contrast with q01) | 5µs | 5µs | 5µs | 49µs | 1.969ms | 280µs |
| `q02_range_scan` | Indexed range scan + count | 5.002ms | 5.034ms | 4.992ms | 5.869ms | 3.657ms | 494µs |
| `q03_starts_with` | STARTS WITH prefix (round-2 finding 1) | 4.836ms | 4.722ms | 4.522ms | 4.779ms | 1.643ms | 3.589ms |
| `q04_one_hop` | 1-hop expand from a bound node | 2.853ms | 2.303ms | 2.643ms | 3.302ms | 1.919ms | 252µs |
| `q05_two_hop` | 2-hop friends-of-friends, DISTINCT | 7.404ms | 6.942ms | 5.863ms | 6.755ms | 1.436ms | 235µs |
| `q06_varlen_3` | Variable-length 1..3 DISTINCT | 9.913ms | 10.402ms | 7.973ms | 10.627ms | 2.509ms | 422µs |
| `q07_global_count` | Global label count (count-store shape) | 567µs | 561µs | 563µs | 652µs | 1.718ms | 1.147ms |
| `q08_group_by` | Group-by + order + limit | 2.821ms | 2.831ms | 2.609ms | 3.238ms | 4.549ms | 2.974ms |
| `q09_top_k` | Top-k by unindexed property | 30.516ms | 32.699ms | 32.382ms | 33.308ms | 3.384ms | 5.771ms |
| `q10_triangle` | Cyclic 3-clique count (WCOJ shape) | 1.213524s | 1.21496s | 1.198328s | 1.252889s | 393.994ms | 218.039ms |
| `q11_expand_into` | Both endpoints bound (ExpandInto shape) | 2.168ms | 2.181ms | 2.735ms | 3.428ms | 1.135ms | 220µs |
| `q12_multi_label` | Multi-label conjunction (round-2 finding 2) | 9µs | 8µs | 8µs | 58µs | 1.303ms | 899µs |
| `q13_shortest_path` | Unweighted shortest path <=6 | 13.115ms | 12.748ms | 13.346ms | 13.183ms | 1.556ms | 348µs |
| `q14_property_filter` | Unindexed property equality scan | 5.075ms | 4.63ms | 5.026ms | 4.822ms | 8.634ms | 2.49ms |
| `q15_create` | Single-node write | 5µs | 3.994ms | 5µs | 54µs | 2.039ms | 232µs |

## Row counts returned (semantic cross-check)

| Query | gograph-embedded | gograph-embedded-durable | gograph-embedded[id] | gograph-bolt | neo4j-bolt | memgraph-bolt |
|---|---|---|---|---|---|---|
| `q01_point_lookup` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q01b_point_lookup_str` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q02_range_scan` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q03_starts_with` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q04_one_hop` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q05_two_hop` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q06_varlen_3` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q07_global_count` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q08_group_by` | 10 | 10 | 10 | 10 | 10 | 10 |
| `q09_top_k` | 10 | 10 | 10 | 10 | 10 | 10 |
| `q10_triangle` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q11_expand_into` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q12_multi_label` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q13_shortest_path` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q14_property_filter` | 1 | 1 | 1 | 1 | 1 | 1 |
| `q15_create` | 1 | 1 | 1 | 1 | 1 | 1 |

## Errors

