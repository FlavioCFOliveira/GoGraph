package cypher_test

// constraint_modern_syntax_test.go — end-to-end regression for #1906: a
// constraint created with the modern FOR … REQUIRE grammar is actually
// registered and enforced (not merely parsed). Before the fix the engine
// accepted only the legacy ON … ASSERT form.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestCreateConstraint_ForRequire_UniqueEnforced(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE CONSTRAINT u_email FOR (n:User) REQUIRE n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT (FOR … REQUIRE): %v", err)
	}

	if e := tryWrite(eng, `CREATE (:User {email: "a@b.com"})`); e != nil {
		t.Fatalf("first insert: %v", e)
	}
	// The UNIQUE constraint declared with the modern grammar must actually bite.
	if e := tryWrite(eng, `CREATE (:User {email: "a@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected UNIQUE violation from FOR … REQUIRE constraint, got: %v", e)
	}
}

func TestCreateConstraint_ForRequire_NotNullEnforced(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE CONSTRAINT u_name IF NOT EXISTS FOR (n:User) REQUIRE n.name IS NOT NULL`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT (FOR … REQUIRE, NOT NULL): %v", err)
	}

	// A node carrying the label without the constrained property must be rejected.
	if e := tryWrite(eng, `CREATE (:User {email: "a@b.com"})`); !errors.Is(e, exec.ErrConstraintViolation) {
		t.Fatalf("expected NOT NULL violation from FOR … REQUIRE constraint, got: %v", e)
	}
	// A node with the property must be accepted.
	if e := tryWrite(eng, `CREATE (:User {name: "Alice"})`); e != nil {
		t.Fatalf("insert with name present: %v", e)
	}
}
