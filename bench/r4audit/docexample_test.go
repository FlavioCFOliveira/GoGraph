//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
)

// TestDocumentedExistsExampleIsRejected runs the EXACT example docs/cypher.md:132
// publishes for `EXISTS { MATCH … }`, so the finding rests on the documented
// query verbatim rather than on a paraphrase of it.
func TestDocumentedExistsExampleIsRejected(t *testing.T) {
	eng := newEng(t, 20)
	// docs/cypher.md:132 — | `EXISTS { MATCH … }` | `WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) }` |
	const documented = `MATCH (n) WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) } RETURN count(n)`
	_, err := eng.RunAny(context.Background(), documented, nil)
	if err == nil {
		fmt.Println("documented EXISTS example: ACCEPT (documentation is accurate)")
		return
	}
	fmt.Printf("documented EXISTS example: REJECT\n  query: %s\n  error: %v\n", documented, err)

	// The same query with an explicit RETURN inside the subquery is accepted,
	// which localises the gap to the optional <primitive result statement>.
	const withReturn = `MATCH (n) WHERE EXISTS { MATCH (n)-[:KNOWS]->(m) RETURN m } RETURN count(n)`
	if _, err2 := eng.RunAny(context.Background(), withReturn, nil); err2 != nil {
		fmt.Printf("  control (with RETURN) ALSO rejected: %v\n", err2)
	} else {
		fmt.Println("  control (with RETURN): ACCEPT — the gap is exactly the omitted RETURN")
	}
}
