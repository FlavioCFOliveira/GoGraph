package cypher_test

// rel_presence_bench_test.go — sprint 222 #1638 regression benchmark.
//
// A relationship bound only to answer `r.k IS NOT NULL` must resolve via the
// kind-gated storage presence check (lpg.EdgeHasProperty) WITHOUT materialising
// the edge's property value. BenchmarkRelPresenceIsNotNull drives the presence
// path; BenchmarkRelValueComparison drives the value-materialising path over an
// identical graph (the property is read as a value, so C1 forces the full
// EdgeProperties fetch). The allocation delta is the observable evidence that
// the presence path skips the value build.
//
// Run with:
//
//	go test -run=^$ -bench='BenchmarkRelPresence|BenchmarkRelValue' -benchmem -count=6 ./cypher/

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// seedFriendGraph builds nNodes nodes connected in a ring by FRIEND edges, each
// carrying a `since` integer property, so a relationship-bound query has a
// property-bearing edge to either presence-check or materialise.
func seedFriendGraph(b *testing.B, eng *cypher.Engine, nNodes int) {
	b.Helper()
	mk := func(q string) {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("seed drain %q: %v", q, err)
		}
		_ = res.Close()
	}
	for i := 0; i < nNodes; i++ {
		mk(fmt.Sprintf("CREATE (:P {i:%d})", i))
	}
	for i := 0; i < nNodes; i++ {
		j := (i + 1) % nNodes
		mk(fmt.Sprintf("MATCH (a:P {i:%d}),(b:P {i:%d}) CREATE (a)-[:FRIEND {since:%d}]->(b)", i, j, 2000+i))
	}
}

// BenchmarkRelPresenceIsNotNull binds r and reads since ONLY via IS NOT NULL, so
// the presence fast path applies and no property value is materialised.
func BenchmarkRelPresenceIsNotNull(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedFriendGraph(b, eng, 2000)
	const q = "MATCH ()-[r:FRIEND]->() WHERE r.since IS NOT NULL RETURN count(r)"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatal(err)
		}
		_ = res.Close()
	}
}

// BenchmarkRelValueComparison binds r and reads since as a VALUE (a comparison),
// so C1 forces the full EdgeProperties materialisation. It is the value-path
// baseline against which the presence path's allocation reduction is read.
func BenchmarkRelValueComparison(b *testing.B) {
	g := newBenchGraph()
	eng := cypher.NewEngine(g)
	seedFriendGraph(b, eng, 2000)
	const q = "MATCH ()-[r:FRIEND]->() WHERE r.since >= 0 RETURN count(r)"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatal(err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatal(err)
		}
		_ = res.Close()
	}
}
