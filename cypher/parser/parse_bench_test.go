package parser

// parse_bench_test.go — cost of the parse front end (normalize → lex → parse →
// AST walk) on representative queries.
//
// Added with rmp #2167, which fills the token stream before parsing so the
// catch-all ERRCHAR rule can be reported instead of silently discarding a
// character. `script` is anchored at EOF, so an accepted query already consumed
// every token; filling fetches the same tokens eagerly rather than lazily, and
// adds one linear pass over them. These benchmarks are the evidence for that
// claim — compare with benchstat across the change.

import "testing"

// benchQueries spans the shapes callers actually parse: a trivial projection, a
// pattern match with a predicate, a multi-clause pipeline, and a write.
var benchQueries = []struct {
	name  string
	query string
}{
	{"Trivial", `RETURN 1 AS n`},
	{"MatchWhere", `MATCH (n:Person) WHERE n.age > 30 AND n.name = 'x' RETURN n.name AS name`},
	{"Pipeline", `MATCH (a:Person)-[:KNOWS]->(b:Person) WHERE a.age > 21 ` +
		`WITH a, count(b) AS friends ORDER BY friends DESC LIMIT 10 ` +
		`RETURN a.name AS name, friends`},
	{"Write", `UNWIND $rows AS row CREATE (n:Person {name: row.name, age: row.age}) RETURN count(n) AS c`},
	{"Regex", `MATCH (n:Person) WHERE n.name =~ '[a-z]+' RETURN n`},
	{"LongLiterals", `RETURN 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' AS a, ` +
		`'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' AS b, 'cccccccccccccccccccccccccccccc' AS c`},
}

func BenchmarkParse(b *testing.B) {
	for _, tc := range benchQueries {
		b.Run(tc.name, func(b *testing.B) {
			// Fail fast rather than benchmarking an error path.
			if _, err := Parse(tc.query); err != nil {
				b.Fatalf("Parse(%q) returned error: %v", tc.query, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Parse(tc.query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkParseRejected measures the rejection path, which now short-circuits
// before the parser runs.
func BenchmarkParseRejected(b *testing.B) {
	const query = `MATCH (n) WHERE n.v != 2 RETURN n`
	if _, err := Parse(query); err == nil {
		b.Fatalf("Parse(%q) unexpectedly accepted", query)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Parse(query)
	}
}
