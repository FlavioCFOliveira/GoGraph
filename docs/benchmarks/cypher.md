# Cypher Engine Benchmarks — IC1–IC14

## Environment

| Key        | Value                                      |
|------------|--------------------------------------------|
| Platform   | darwin/arm64                               |
| CPU        | Apple M4                                   |
| Go version | go1.26.3                                   |
| Commit     | c48d267                                    |
| Run command | `go test -bench=BenchmarkIC -benchmem -count=3 ./bench/cypher_ldbc/...` |

## Seed Graph

1 000 nodes distributed across three labels: `Person` (indices 0, 3, 6, …),
`City` (indices 1, 4, 7, …), `Company` (indices 2, 5, 8, …). No edges. No
node properties except where a write query sets them.

## Query Shapes

| ID   | File         | Shape                                                        | Write? |
|------|--------------|--------------------------------------------------------------|--------|
| IC1  | ic1.cypher   | `MATCH (n) RETURN n` — full node scan                        | No     |
| IC2  | ic2.cypher   | `MATCH (n:Person) RETURN n` — label scan                     | No     |
| IC3  | ic3.cypher   | `MATCH (n:Person) RETURN n` — label scan (identical to IC2)  | No     |
| IC4  | ic4.cypher   | `MATCH (n:Person) WHERE n.name IS NOT NULL RETURN n` — IS NOT NULL filter | No |
| IC5  | ic5.cypher   | `CREATE (n:Person)` — bare CREATE                            | Yes    |
| IC6  | ic6.cypher   | `MERGE (n:City)` — bare MERGE                                | Yes    |
| IC7  | ic7.cypher   | `MATCH (n:City) RETURN n` — label scan                       | No     |
| IC8  | ic8.cypher   | `CREATE (n:Company)` — bare CREATE                           | Yes    |
| IC9  | ic9.cypher   | `MATCH (n:Person) WHERE n.age IS NOT NULL RETURN n` — IS NOT NULL property filter | No |
| IC10 | ic10.cypher  | `MATCH (n:Person) RETURN n.name` — property projection       | No     |
| IC11 | ic11.cypher  | `MATCH (n) WHERE n.active = true RETURN n` — boolean filter (zero matches) | No |
| IC12 | ic12.cypher  | `CREATE (n:Person {name: 'Alice'})` — CREATE with properties | Yes    |
| IC13 | ic13.cypher  | `MERGE (n:Person {name: 'Bob'})` — MERGE with properties     | Yes    |
| IC14 | ic14.cypher  | `MATCH (n:Company) RETURN n` — label scan                    | No     |

## Raw Benchmark Output

```
goos: darwin
goarch: arm64
pkg: gograph/bench/cypher_ldbc
cpu: Apple M4
BenchmarkIC1-10             	    8085	    150035 ns/op	  439877 B/op	    5780 allocs/op
BenchmarkIC1-10             	    7966	    150178 ns/op	  439877 B/op	    5780 allocs/op
BenchmarkIC1-10             	    8054	    150648 ns/op	  439877 B/op	    5780 allocs/op
BenchmarkIC2-10             	   24264	     49307 ns/op	  140121 B/op	    1946 allocs/op
BenchmarkIC2-10             	   23900	     49655 ns/op	  140121 B/op	    1946 allocs/op
BenchmarkIC2-10             	   24135	     49562 ns/op	  140121 B/op	    1946 allocs/op
BenchmarkIC3-10             	   23990	     49925 ns/op	  140121 B/op	    1946 allocs/op
BenchmarkIC3-10             	   24054	     49715 ns/op	  140121 B/op	    1946 allocs/op
BenchmarkIC3-10             	   24105	     49795 ns/op	  140121 B/op	    1946 allocs/op
BenchmarkIC4-10             	   23810	     50621 ns/op	  140201 B/op	    1947 allocs/op
BenchmarkIC4-10             	   23516	     50717 ns/op	  140201 B/op	    1947 allocs/op
BenchmarkIC4-10             	   23604	     50481 ns/op	  140201 B/op	    1947 allocs/op
BenchmarkIC5-10             	 1361751	       883.1 ns/op	     979 B/op	      22 allocs/op
BenchmarkIC5-10             	 1316238	       880.4 ns/op	     978 B/op	      22 allocs/op
BenchmarkIC5-10             	 1329238	       885.2 ns/op	     981 B/op	      22 allocs/op
BenchmarkIC6-10             	 1000000	      1115 ns/op	    1449 B/op	      25 allocs/op
BenchmarkIC6-10             	 1000000	      1116 ns/op	    1449 B/op	      25 allocs/op
BenchmarkIC6-10             	 1000000	      1117 ns/op	    1449 B/op	      25 allocs/op
BenchmarkIC7-10             	   23973	     50261 ns/op	  139841 B/op	    1958 allocs/op
BenchmarkIC7-10             	   23870	     50320 ns/op	  139841 B/op	    1958 allocs/op
BenchmarkIC7-10             	   23829	     50396 ns/op	  139841 B/op	    1958 allocs/op
BenchmarkIC8-10             	 1324231	       882.6 ns/op	     979 B/op	      22 allocs/op
BenchmarkIC8-10             	 1324922	       884.3 ns/op	     980 B/op	      22 allocs/op
BenchmarkIC8-10             	 1315780	       890.7 ns/op	     978 B/op	      22 allocs/op
BenchmarkIC9-10             	   23439	     51357 ns/op	  140202 B/op	    1947 allocs/op
BenchmarkIC9-10             	   23450	     51273 ns/op	  140202 B/op	    1947 allocs/op
BenchmarkIC9-10             	   23350	     51377 ns/op	  140202 B/op	    1947 allocs/op
BenchmarkIC10-10            	   23883	     50467 ns/op	  140106 B/op	    1945 allocs/op
BenchmarkIC10-10            	   23678	     50340 ns/op	  140105 B/op	    1945 allocs/op
BenchmarkIC10-10            	   23889	     50232 ns/op	  140105 B/op	    1945 allocs/op
BenchmarkIC11-10            	    7636	    156012 ns/op	  439950 B/op	    5781 allocs/op
BenchmarkIC11-10            	    7662	    156823 ns/op	  439950 B/op	    5781 allocs/op
BenchmarkIC11-10            	    7803	    155895 ns/op	  439950 B/op	    5781 allocs/op
BenchmarkIC12-10            	  963093	      1516 ns/op	    1699 B/op	      29 allocs/op
BenchmarkIC12-10            	  967741	      1546 ns/op	    1700 B/op	      29 allocs/op
BenchmarkIC12-10            	  983384	      1525 ns/op	    1696 B/op	      29 allocs/op
BenchmarkIC13-10            	  836292	      1718 ns/op	    1990 B/op	      32 allocs/op
BenchmarkIC13-10            	  818439	      1722 ns/op	    1994 B/op	      32 allocs/op
BenchmarkIC13-10            	  822273	      1714 ns/op	    1993 B/op	      32 allocs/op
BenchmarkIC14-10            	   23910	     50160 ns/op	  139689 B/op	    1938 allocs/op
BenchmarkIC14-10            	   23744	     50355 ns/op	  139689 B/op	    1938 allocs/op
BenchmarkIC14-10            	   23738	     50674 ns/op	  139689 B/op	    1938 allocs/op
BenchmarkIC1_Parallel-10    	   10000	    111651 ns/op	  439899 B/op	    5780 allocs/op
BenchmarkIC1_Parallel-10    	   10000	    110428 ns/op	  439897 B/op	    5780 allocs/op
BenchmarkIC1_Parallel-10    	   10000	    109786 ns/op	  439896 B/op	    5780 allocs/op
BenchmarkIC2_Parallel-10    	   35190	     34452 ns/op	  140127 B/op	    1946 allocs/op
BenchmarkIC2_Parallel-10    	   34956	     34863 ns/op	  140128 B/op	    1946 allocs/op
BenchmarkIC2_Parallel-10    	   34293	     34409 ns/op	  140127 B/op	    1946 allocs/op
BenchmarkIC9_Parallel-10    	   34861	     34337 ns/op	  140208 B/op	    1947 allocs/op
BenchmarkIC9_Parallel-10    	   35112	     34236 ns/op	  140209 B/op	    1947 allocs/op
BenchmarkIC9_Parallel-10    	   34136	     34596 ns/op	  140208 B/op	    1947 allocs/op
PASS
ok  	gograph/bench/cypher_ldbc	81.113s
```

## Summary Table (median of 3 runs)

Sequential benchmarks:

| Benchmark    | Median ns/op | B/op    | allocs/op | Notes                          |
|--------------|-------------:|--------:|----------:|--------------------------------|
| IC1          |      150,178 | 439,877 |     5,780 | Full scan: 1 000 nodes         |
| IC2          |       49,562 | 140,121 |     1,946 | Person label scan: ~334 nodes  |
| IC3          |       49,795 | 140,121 |     1,946 | Identical shape to IC2         |
| IC4          |       50,481 | 140,201 |     1,947 | IS NOT NULL filter on Person   |
| IC5          |          883 |     979 |        22 | CREATE bare node               |
| IC6          |        1,116 |   1,449 |        25 | MERGE bare node                |
| IC7          |       50,320 | 139,841 |     1,958 | City label scan: ~333 nodes    |
| IC8          |          884 |     979 |        22 | CREATE bare node               |
| IC9          |       51,273 | 140,202 |     1,947 | IS NOT NULL filter on Person   |
| IC10         |       50,340 | 140,105 |     1,945 | Property projection on Person  |
| IC11         |      156,012 | 439,950 |     5,781 | Boolean filter — full scan, 0 matches |
| IC12         |        1,525 |   1,699 |        29 | CREATE with properties         |
| IC13         |        1,718 |   1,993 |        32 | MERGE with properties          |
| IC14         |       50,355 | 139,689 |     1,938 | Company label scan: ~333 nodes |

## Parallel Benchmark Results

10 goroutines (GOMAXPROCS=10 on Apple M4):

| Benchmark        | Median ns/op | B/op    | allocs/op | Speedup vs sequential |
|------------------|--------------:|--------:|----------:|-----------------------|
| IC1_Parallel     |       110,428 | 439,897 |     5,780 | ~1.36×                |
| IC2_Parallel     |        34,452 | 140,127 |     1,946 | ~1.44×                |
| IC9_Parallel     |        34,337 | 140,208 |     1,947 | ~1.49×                |

## Observations

**Write vs read cost ratio.** Bare CREATE (IC5, IC8) costs ~883 ns versus ~50 µs
for a same-cardinality label scan. The scan is ~57× slower because it must
iterate ~333 result nodes and materialise each record; the write path creates
one node and returns no rows.

**Property-enriched writes.** Adding a properties map to CREATE (IC12 ≈ 1 525 ns)
and MERGE (IC13 ≈ 1 718 ns) adds ~640–835 ns over bare variants. This reflects
the extra map allocation and property assignment.

**Filter cost.** IC4 and IC9 both apply an IS NOT NULL predicate on Person nodes.
Both land within ~1 µs of the plain IC2 label scan (~50 µs), showing the filter
evaluation overhead is negligible compared to node iteration.

**Full-scan vs label-filtered.** IC1 (all nodes, 1 000) is ~3× slower than IC2
(Person, ~334 nodes) — roughly proportional to the node count difference. IC11
(full scan with a boolean filter that matches nothing) costs ~156 µs, essentially
the same as IC1, confirming the engine cannot short-circuit on zero-selectivity
property filters without an index.

**Parallel scaling.** The three parallel variants achieve 1.36–1.49× speedup at
GOMAXPROCS=10. Sub-linear scaling indicates contention on shared read paths
(likely the graph's RWMutex). Each query allocates the same amount as the
sequential path — concurrent execution does not introduce extra allocations.
