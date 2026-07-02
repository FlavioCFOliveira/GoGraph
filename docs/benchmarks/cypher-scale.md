# Cypher Engine Benchmarks — 120k-node scale

Companion to [`cypher.md`](cypher.md) (the 1 000-node IC1–IC14 suite). That
suite is too small to expose the Cypher scan/project/expand path's
per-scanned-entity allocation cost, so no benchmark in the guard set operated
above roughly 1 000 nodes — the 2026-07-02 production-readiness audit
(finding P3) flagged this as a coverage gap, since the audit's own
100 000-node ad hoc profiling harness (findings P1 and P2) had no permanent
regression gate. `bench/cypher_scale` closes that gap: three benchmarks,
each running against a shared 120 000-node graph.

## Environment

| Key         | Value                                                              |
|-------------|---------------------------------------------------------------------|
| Platform    | darwin/arm64                                                        |
| CPU         | Apple M4                                                            |
| Go version  | go1.26.4                                                            |
| Commit      | (sprint 262, task #1860)                                            |
| Run command | `go test -bench=. -benchmem -count=5 ./bench/cypher_scale/...`      |

## Seed Graph

120 000 `:Person` nodes, each with a `firstName` (string) and `age` (integer,
`18 + i%65`, i.e. uniformly spread across `[18, 82]`) property. Each node has
8 outgoing `:KNOWS` edges to a deterministic pseudo-random spread of other
nodes (`(i + k*104729) % 120000` for `k` in `1..8`), for 960 000 directed
edges total. The graph is built once in `TestMain` and shared read-only
across every benchmark; a short-layer smoke test
(`TestCypherScale_QueriesRun`) verifies all three query shapes against it on
every `go test` run, independent of `-bench`.

## Query Shapes

| Benchmark             | Query                                                              | Shape                                    |
|------------------------|---------------------------------------------------------------------|-------------------------------------------|
| `BenchmarkCountAllPersons` | `MATCH (p:Person) RETURN count(p) AS c`                          | Pure label scan + aggregate               |
| `BenchmarkFilterProject`   | `MATCH (p:Person) WHERE p.age > 47 RETURN p.firstName, p.age`    | Scan + property filter + 2-column project |
| `BenchmarkExpand1Hop`      | `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.firstName, b.age` | Scan + type-filtered 1-hop expand + project (960 000 result rows) |

## Raw Benchmark Output

```
goos: darwin
goarch: arm64
pkg: github.com/FlavioCFOliveira/GoGraph/bench/cypher_scale
cpu: Apple M4
BenchmarkCountAllPersons-10    	     361	   3300989 ns/op	  970540 B/op	  119807 allocs/op
BenchmarkCountAllPersons-10    	     366	   3228975 ns/op	  970539 B/op	  119807 allocs/op
BenchmarkCountAllPersons-10    	     367	   3246019 ns/op	  970539 B/op	  119807 allocs/op
BenchmarkCountAllPersons-10    	     364	   3297152 ns/op	  970539 B/op	  119807 allocs/op
BenchmarkCountAllPersons-10    	     360	   3241416 ns/op	  970540 B/op	  119807 allocs/op
BenchmarkFilterProject-10      	      20	  54755031 ns/op	13290550 B/op	  184480 allocs/op
BenchmarkFilterProject-10      	      20	  54931546 ns/op	13290552 B/op	  184480 allocs/op
BenchmarkFilterProject-10      	      21	  55646655 ns/op	13290465 B/op	  184479 allocs/op
BenchmarkFilterProject-10      	      20	  56179312 ns/op	13290499 B/op	  184479 allocs/op
BenchmarkFilterProject-10      	      21	  54711167 ns/op	13290500 B/op	  184479 allocs/op
BenchmarkExpand1Hop-10         	       1	1455610250 ns/op	396400448 B/op	 6006184 allocs/op
BenchmarkExpand1Hop-10         	       1	1443443833 ns/op	397972560 B/op	 6003936 allocs/op
BenchmarkExpand1Hop-10         	       1	1440745166 ns/op	397099504 B/op	 6003876 allocs/op
BenchmarkExpand1Hop-10         	       1	1427519833 ns/op	397536080 B/op	 6003906 allocs/op
BenchmarkExpand1Hop-10         	       1	1448351041 ns/op	397972800 B/op	 6003937 allocs/op
PASS
ok  	github.com/FlavioCFOliveira/GoGraph/bench/cypher_scale	22.192s
```

### benchstat summary

```
                     │       sec/op       │        B/op        │     allocs/op     │
CountAllPersons-10          3.246m ± ∞ ¹          947.8Ki ± ∞ ¹          119.8k ± ∞ ¹
FilterProject-10            54.93m ± ∞ ¹          12.67Mi ± ∞ ¹          184.5k ± ∞ ¹
Expand1Hop-10                1.443 ± ∞ ¹          379.1Mi ± ∞ ¹          6.004M ± ∞ ¹
¹ need >= 6 samples for confidence interval at level 0.95 (5 samples matches the
  project's canonical `-count=5` benchmark convention; this is a baseline
  measurement, not a before/after comparison)
```

## Reading these numbers

- **`CountAllPersons`**: 119 807 allocs for a query whose result is a single
  integer — one allocation per scanned node (the scan leaf boxes the node ID
  into an `expr.IntegerValue` upstream of the `count` aggregate, which never
  needs the boxed value). This is finding P1's exact mechanism, now with a
  permanent regression gate: `allocs/op ≈ seedSize - 256` (the `staticuint64s`
  cutoff for small boxed integers) plus a small constant result-materialisation
  overhead (119 807 measured vs. 119 744 from the raw `seedSize - 256`
  formula, a 63-allocation gap), matching the "vacuous zero-alloc gate"
  pattern the audit also flagged (finding P4) at a scale where it cannot hide.
- **`FilterProject`**: 184 479–184 480 allocs — one boxed scalar per scanned
  node for the `WHERE p.age > 47` predicate, plus two boxed scalars per
  matching row (roughly 53.8% of 120 000 nodes have `age > 47` given the
  uniform `18 + i%65` distribution).
- **`Expand1Hop`**: ~6.00M allocs for 960 000 result rows — about 6.25
  allocs/row, the same order of magnitude the audit measured (~8 allocs/row)
  for finding P2 (edge-label re-materialisation per candidate edge on top of
  per-row boxing).

Future work on P1/P2 (tracked separately in the `#1704` de-boxing epic) should
show a measurable drop in `allocs/op` here, compared with `benchstat` against
this file's raw numbers.
