package cypher_test

// constraint_identifier_len_test.go — regression for #1903 at the engine
// boundary: a CREATE CONSTRAINT with an over-long identifier is rejected before
// any in-memory registration or durable write, so nothing is left enforced.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestCreateConstraint_RejectsOverLongIdentifier(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	long := strings.Repeat("a", 5000) // > maxSchemaIdentifierLen (4096)
	if _, err := eng.Run(ctx, `CREATE CONSTRAINT `+long+` ON (n:Person) ASSERT n.email IS UNIQUE`, nil); err == nil {
		t.Fatal("expected error creating a constraint with an over-long name, got nil")
	}

	// Nothing must have been registered: two nodes sharing an email must be
	// insertable because no UNIQUE constraint is active.
	if e := tryWrite(eng, `CREATE (:Person {email: "dup@b.com"})`); e != nil {
		t.Fatalf("CREATE first node: %v", e)
	}
	if e := tryWrite(eng, `CREATE (:Person {email: "dup@b.com"})`); e != nil {
		t.Fatalf("duplicate must be accepted (no constraint should have registered), got: %v", e)
	}
}
