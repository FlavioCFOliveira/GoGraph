//go:build threeway

// dialect_test.go — round-4 audit: which Cypher surface forms does each engine
// ACCEPT?
//
// The round-3 audit compared clause support from documentation and grammar
// files. This asks the three engines directly, over the same driver, so the
// answer is the implementations' behaviour rather than their documentation.
// It is a parse/accept probe, not a benchmark: every query is bounded by
// LIMIT or is a no-op on an empty graph, so it may run against a loaded or an
// empty database.
//
// Run with the same containers as TestThreeWay:
//
//	go test -tags=threeway -run TestDialectMatrix -v ./bench/comparison/
package comparison

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// dialectProbe is one syntactic form and the authority that defines it.
type dialectProbe struct {
	name string
	cy   string
	// authority cites the primary source that rules on the form.
	authority string
}

var dialectProbes = []dialectProbe{
	{
		"exists-match-no-return",
		`MATCH (a) WHERE EXISTS { MATCH (a)-[:K]->() } RETURN count(a)`,
		"openCypher 2024.3 BNF: <linear statement> ::= <primitive statement>... [ <primitive result statement> ] — RETURN optional",
	},
	{
		"exists-match-where-no-return",
		`MATCH (a) WHERE EXISTS { MATCH (a)-[:K]->(b) WHERE b.x > 1 } RETURN count(a)`,
		"openCypher 2024.3 BNF: <graph pattern where clause>",
	},
	{
		"exists-match-return",
		`MATCH (a) WHERE EXISTS { MATCH (a)-[:K]->(b) RETURN b } RETURN count(a)`,
		"openCypher 2024.3 BNF: <procedure specification>",
	},
	{
		"exists-pattern-only",
		`MATCH (a) WHERE EXISTS { (a)-[:K]->() } RETURN count(a)`,
		"openCypher 2024.3 BNF: <graph pattern>",
	},
	{
		"count-match-no-return",
		`MATCH (a) WHERE COUNT { MATCH (a)-[:K]->() } > 0 RETURN count(a)`,
		"Neo4j 5 COUNT subquery",
	},
	{
		"collect-subquery",
		`MATCH (a) RETURN COLLECT { MATCH (a)-[:K]->(b) RETURN b } AS bs LIMIT 1`,
		"Neo4j 5 COLLECT subquery",
	},
	{
		"call-subquery",
		`MATCH (a) CALL { WITH a MATCH (a)-[:K]->(b) RETURN b } RETURN count(b)`,
		"openCypher CALL subquery",
	},
	{
		"offset-synonym",
		`MATCH (a) RETURN a OFFSET 1 LIMIT 1`,
		"openCypher 2024.3 <offset synonym>",
	},
	{
		"order-by-nulls-last",
		`MATCH (a) RETURN a.x ORDER BY a.x NULLS LAST LIMIT 1`,
		"Neo4j 5 ORDER BY … NULLS LAST",
	},
	{
		"union-distinct",
		`RETURN 1 AS x UNION DISTINCT RETURN 2 AS x`,
		"openCypher UNION DISTINCT",
	},
	{
		"explain-prefix",
		`EXPLAIN MATCH (a) RETURN a LIMIT 1`,
		"Neo4j/Memgraph EXPLAIN query prefix",
	},
	{
		"profile-prefix",
		`PROFILE MATCH (a) RETURN a LIMIT 1`,
		"Neo4j/Memgraph PROFILE query prefix",
	},
	{
		"show-indexes",
		`SHOW INDEXES`,
		"Neo4j 5 SHOW INDEXES",
	},
	{
		"exists-function",
		`MATCH (a) WHERE exists(a.x) RETURN count(a)`,
		"openCypher 9 exists() function (removed in Neo4j 5)",
	},
	{
		"quantified-path-pattern",
		`MATCH (a)-[:K]->{1,3}(b) RETURN count(*)`,
		"GQL/Cypher 25 quantified path pattern",
	},
}

// TestDialectMatrix reports, per engine, ACCEPT or REJECT for each form.
func TestDialectMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type engine struct {
		name string
		run  func(ctx context.Context, cy string) error
	}
	var engines []engine

	emb := newEmbeddedTarget("sid")
	engines = append(engines, engine{"gograph", func(ctx context.Context, cy string) error {
		_, err := emb.execRead(ctx, cy, nil)
		return err
	}})

	for _, r := range []struct {
		name string
		uri  string
		auth neo4j.AuthToken
	}{
		{"neo4j", "bolt://localhost:7687", neo4j.BasicAuth("neo4j", "gographbench", "")},
		{"memgraph", "bolt://localhost:7688", neo4j.NoAuth()},
	} {
		d, err := neo4j.NewDriverWithContext(r.uri, r.auth)
		if err != nil {
			t.Logf("%s: driver: %v (skipped)", r.name, err)
			continue
		}
		if err := d.VerifyConnectivity(ctx); err != nil {
			t.Logf("%s: not reachable: %v (skipped)", r.name, err)
			_ = d.Close(ctx)
			continue
		}
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		engines = append(engines, engine{r.name, func(ctx context.Context, cy string) error {
			s := d.NewSession(ctx, neo4j.SessionConfig{})
			defer s.Close(ctx) //nolint:errcheck
			res, err := s.Run(ctx, cy, nil)
			if err != nil {
				return err
			}
			for res.Next(ctx) {
			}
			return res.Err()
		}})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n| form | ")
	for _, e := range engines {
		fmt.Fprintf(&b, "%s | ", e.name)
	}
	fmt.Fprintf(&b, "authority |\n|---|")
	for range engines {
		fmt.Fprintf(&b, "---|")
	}
	fmt.Fprintf(&b, "---|\n")

	for _, p := range dialectProbes {
		fmt.Fprintf(&b, "| `%s` | ", p.name)
		for _, e := range engines {
			err := e.run(ctx, p.cy)
			verdict := "ACCEPT"
			if err != nil {
				verdict = "**REJECT**"
			}
			fmt.Fprintf(&b, "%s | ", verdict)
		}
		fmt.Fprintf(&b, "%s |\n", p.authority)
	}
	t.Log(b.String())
}
