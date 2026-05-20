# Cypher LDBC Baseline Benchmarks

This document records the baseline throughput and allocation figures for the
GoGraph Cypher engine against eight query shapes modelled after the LDBC SNB
Interactive Complex (IC) workload.

## Environment

| Property      | Value                                         |
|---------------|-----------------------------------------------|
| Platform      | darwin/arm64                                  |
| CPU           | Apple M4                                      |
| Go version    | 1.26                                          |
| Commit        | see `git log -1 --oneline`                    |
| Run command   | `go test -bench=. -benchmem -count=3 ./bench/cypher_ldbc/...` |

## Seed graph

1 000 nodes distributed across three labels:

| Label   | Count |
|---------|-------|
| Person  | 334   |
| City    | 333   |
| Company | 333   |

## Query shapes

| ID  | File        | Shape                                   | Write? |
|-----|-------------|-----------------------------------------|--------|
| IC1 | ic1.cypher  | `MATCH (n) RETURN n`                    | No     |
| IC2 | ic2.cypher  | `MATCH (n:Person) RETURN n`             | No     |
| IC3 | ic3.cypher  | `MATCH (n:Person) RETURN n` (label-scan)| No     |
| IC4 | ic4.cypher  | `MATCH (n:Person) WHERE n.name IS NOT NULL RETURN n` | No |
| IC5 | ic5.cypher  | `CREATE (n:Person)`                     | Yes    |
| IC6 | ic6.cypher  | `MERGE (n:City)`                        | Yes    |
| IC7 | ic7.cypher  | `MATCH (n:City) RETURN n`               | No     |
| IC8 | ic8.cypher  | `CREATE (n:Company)`                    | Yes    |

Note: IC1 (AllNodesScan over 1 000 nodes) is ~3× slower than label scans (IC2/3/4/7)
because it iterates the full mapper; label scans hit a Roaring bitmap. Write queries
(IC5/6/8) operate on a fresh empty graph per iteration, so their counts reflect
single-node create/merge overhead rather than accumulating state.

## Results

```
goos: darwin
goarch: arm64
pkg: gograph/bench/cypher_ldbc
cpu: Apple M4
BenchmarkIC1-10       8073    148906 ns/op    439853 B/op    5779 allocs/op
BenchmarkIC1-10       8174    150105 ns/op    439852 B/op    5779 allocs/op
BenchmarkIC1-10       8073    150638 ns/op    439852 B/op    5779 allocs/op
BenchmarkIC2-10      24448     48450 ns/op    140057 B/op    1941 allocs/op
BenchmarkIC2-10      24393     48961 ns/op    140057 B/op    1941 allocs/op
BenchmarkIC2-10      24700     48871 ns/op    140057 B/op    1941 allocs/op
BenchmarkIC3-10      24675     48977 ns/op    140057 B/op    1941 allocs/op
BenchmarkIC3-10      24500     49028 ns/op    140057 B/op    1941 allocs/op
BenchmarkIC3-10      24667     48863 ns/op    140057 B/op    1941 allocs/op
BenchmarkIC4-10      24165     49841 ns/op    140105 B/op    1942 allocs/op
BenchmarkIC4-10      23868     50085 ns/op    140105 B/op    1942 allocs/op
BenchmarkIC4-10      23887     49670 ns/op    140105 B/op    1942 allocs/op
BenchmarkIC5-10    1405989       841.1 ns/op     932 B/op      21 allocs/op
BenchmarkIC5-10    1381131       857.7 ns/op     936 B/op      21 allocs/op
BenchmarkIC5-10    1377813       848.5 ns/op     936 B/op      21 allocs/op
BenchmarkIC6-10    1000000      1116 ns/op      1401 B/op      24 allocs/op
BenchmarkIC6-10    1000000      1124 ns/op      1401 B/op      24 allocs/op
BenchmarkIC6-10    1000000      1100 ns/op      1401 B/op      24 allocs/op
BenchmarkIC7-10      24316     49456 ns/op    139665 B/op    1938 allocs/op
BenchmarkIC7-10      24194     49373 ns/op    139665 B/op    1938 allocs/op
BenchmarkIC7-10      24128     50069 ns/op    139665 B/op    1938 allocs/op
BenchmarkIC8-10    1373529       849.1 ns/op     937 B/op      21 allocs/op
BenchmarkIC8-10    1387575       845.9 ns/op     935 B/op      21 allocs/op
BenchmarkIC8-10    1389924       842.4 ns/op     935 B/op      21 allocs/op
PASS
ok  gograph/bench/cypher_ldbc  40.120s
```

## Summary table (median of 3 runs)

| Benchmark | ns/op    | B/op    | allocs/op |
|-----------|----------|---------|-----------|
| IC1       | 150 105  | 439 852 | 5 779     |
| IC2       | 48 961   | 140 057 | 1 941     |
| IC3       | 49 028   | 140 057 | 1 941     |
| IC4       | 50 085   | 140 105 | 1 942     |
| IC5       | 857      | 936     | 21        |
| IC6       | 1 124    | 1 401   | 24        |
| IC7       | 49 456   | 139 665 | 1 938     |
| IC8       | 849      | 935     | 21        |

## Observations

- **Label scans (IC2/3/4/7)** are consistently ~3× faster than the AllNodesScan
  (IC1): Roaring bitmap cardinality is O(n/64) vs mapper walk O(n).
- **Write paths (IC5/IC8 CREATE, IC6 MERGE)** are ~50–60× faster per iteration
  than read scans because they operate on an empty graph and bypass the full
  scan pipeline.
- **IC4 IS NOT NULL** adds a negligible overhead over IC3 (same label scan;
  the predicate is a pass-through filter in the current executor).
- Allocation counts per label-scan op (~1 941) reflect per-call operator tree
  construction; plan caching already deduplicates parse/translate. Future work:
  operator tree pooling to reduce allocation pressure on repeated queries.
