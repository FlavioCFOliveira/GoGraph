package parser

import (
	"fmt"
	"sync"
	"testing"

	"gograph/cypher/ast"
)

// TestConcurrentParsing verifies that Parse and ParseStrict are safe to call
// concurrently from many goroutines. Each goroutine parses a distinct query so
// that the test would expose any inadvertent shared-state write under the race
// detector.
//
// Run with: go test -race -run TestConcurrentParsing ./cypher/parser/...
func TestConcurrentParsing(t *testing.T) {
	const goroutines = 100

	// Distinct queries — varying structure to exercise multiple visitor paths.
	queries := make([]string, goroutines)
	for i := range queries {
		switch i % 5 {
		case 0:
			queries[i] = fmt.Sprintf("MATCH (n) WHERE n.id = %d RETURN n", i)
		case 1:
			queries[i] = fmt.Sprintf("CREATE (n:Node {val: %d})", i)
		case 2:
			queries[i] = fmt.Sprintf("MATCH (a)-[:REL]->(b) WHERE a.x = %d RETURN b.name", i)
		case 3:
			queries[i] = fmt.Sprintf("MATCH (n) RETURN n.val ORDER BY n.val LIMIT %d", i+1)
		case 4:
			queries[i] = fmt.Sprintf("MATCH (n) SET n.count = %d", i)
		}
	}

	type result struct {
		q   ast.Query
		err error
	}
	results := make([]result, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			q, err := Parse(queries[idx])
			results[idx] = result{q: q, err: err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: Parse(%q) returned unexpected error: %v", i, queries[i], r.err)
			continue
		}
		if r.q == nil {
			t.Errorf("goroutine %d: Parse(%q) returned nil AST with no error", i, queries[i])
		}
	}
}

// TestConcurrentParseStrict verifies the same guarantee for ParseStrict.
func TestConcurrentParseStrict(t *testing.T) {
	const goroutines = 100

	queries := make([]string, goroutines)
	for i := range queries {
		queries[i] = fmt.Sprintf("MATCH (n:T%d) RETURN n.name", i)
	}

	type result struct {
		q    ast.Query
		errs []error
	}
	results := make([]result, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			q, errs := ParseStrict(queries[idx])
			results[idx] = result{q: q, errs: errs}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if len(r.errs) != 0 {
			t.Errorf("goroutine %d: ParseStrict(%q) returned unexpected errors: %v", i, queries[i], r.errs)
			continue
		}
		if r.q == nil {
			t.Errorf("goroutine %d: ParseStrict(%q) returned nil AST with no error", i, queries[i])
		}
	}
}
