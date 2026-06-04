package lpg_test

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
)

// TestValidateNode_RequiredPropertyEnforced is the task #1319 acceptance
// criterion: a schema declaring RequireProperty("User","email") must reject a
// finalised :User node that lacks email, while accepting one that carries it.
//
// Existence is enforced at the node-finalisation boundary
// (Graph.ValidateNode), NOT at the mutation point, because a node legitimately
// receives its label before the property that label requires. The subtests
// below pin both halves of that contract: the build itself never errors (the
// node is allowed to exist mid-construction), and the finalise check is the
// gate that accepts or rejects.
func TestValidateNode_RequiredPropertyEnforced(t *testing.T) {
	t.Parallel()

	newGraphWithSchema := func() *lpg.Graph[string, int64] {
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		s := schema.New(g.Registry(), g.PropertyKeys())
		if _, err := s.RegisterProperty("email", lpg.PropString); err != nil {
			t.Fatalf("RegisterProperty email: %v", err)
		}
		s.RequireProperty("User", "email")
		g.SetValidator(s)
		return g
	}

	// buildUserNode reproduces the executor's node-build order:
	// AddNode → SetNodeLabel → (optionally) SetNodeProperty. Every mutation is
	// asserted to succeed: setting the User label on a node that has no email
	// yet must NOT be rejected mid-construction, or legitimate CREATE would
	// break. Returns the finalise error so callers assert on it.
	buildUserNode := func(t *testing.T, g *lpg.Graph[string, int64], key string, withEmail bool) error {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%q): %v", key, err)
		}
		// The label is set BEFORE the required property exists. This must
		// succeed — the per-mutation path enforces typing, never existence.
		if err := g.SetNodeLabel(key, "User"); err != nil {
			t.Fatalf("SetNodeLabel(%q, User) rejected mid-construction: %v", key, err)
		}
		if withEmail {
			if err := g.SetNodeProperty(key, "email", lpg.StringValue("a@b.com")); err != nil {
				t.Fatalf("SetNodeProperty(%q, email) rejected: %v", key, err)
			}
		}
		return g.ValidateNode(key)
	}

	t.Run("missing required property rejected at finalise", func(t *testing.T) {
		t.Parallel()
		g := newGraphWithSchema()
		err := buildUserNode(t, g, "u1", false)
		if !errors.Is(err, schema.ErrMissingRequired) {
			t.Fatalf("ValidateNode: want ErrMissingRequired, got %v", err)
		}
		// The node was allowed to be built; only finalisation rejects it.
		if !g.HasNodeLabel("u1", "User") {
			t.Errorf("node was not built: :User label absent after construction")
		}
	})

	t.Run("required property present accepted at finalise", func(t *testing.T) {
		t.Parallel()
		g := newGraphWithSchema()
		if err := buildUserNode(t, g, "u2", true); err != nil {
			t.Fatalf("ValidateNode: want nil for a complete :User, got %v", err)
		}
	})

	t.Run("wrong-typed property rejected eagerly at mutation point", func(t *testing.T) {
		t.Parallel()
		g := newGraphWithSchema()
		if err := g.AddNode("u3"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel("u3", "User"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		// email is declared PropString; an Int64 value must be rejected by the
		// per-value Validate hook at the write itself, leaving the graph
		// unchanged — distinct from the existence check above.
		err := g.SetNodeProperty("u3", "email", lpg.Int64Value(42))
		if !errors.Is(err, schema.ErrTypeMismatch) {
			t.Fatalf("SetNodeProperty: want ErrTypeMismatch, got %v", err)
		}
		if _, ok := g.GetNodeProperty("u3", "email"); ok {
			t.Errorf("rejected typed write leaked into the graph")
		}
	})

	t.Run("node without the requiring label is unaffected", func(t *testing.T) {
		t.Parallel()
		g := newGraphWithSchema()
		// :Account has no required properties; a bare node finalises clean.
		if err := g.AddNode("a1"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel("a1", "Account"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.ValidateNode("a1"); err != nil {
			t.Fatalf("ValidateNode(:Account): want nil, got %v", err)
		}
	})
}

// errEveryNode is returned by the rejectAllValidator below.
var errEveryNode = errors.New("test: reject every node")

// rejectAllValidator implements both lpg.SchemaValidator and lpg.NodeValidator.
// Validate (per-value) always passes; ValidateNode (whole-node) always fails.
// It lets the test assert Graph.ValidateNode actually dispatches to the
// NodeValidator surface without depending on schema-package semantics.
type rejectAllValidator struct{}

func (rejectAllValidator) Validate(string, lpg.PropertyValue) error { return nil }

func (rejectAllValidator) ValidateNode([]string, map[string]lpg.PropertyValue) error {
	return errEveryNode
}

// TestValidateNode_NodeValidatorDispatch verifies the dispatch contract of
// Graph.ValidateNode in isolation from the schema package:
//   - no validator installed → nil;
//   - a validator that does not implement NodeValidator → nil;
//   - a NodeValidator → its verdict is returned;
//   - an unknown key → nil (nothing to finalise).
func TestValidateNode_NodeValidatorDispatch(t *testing.T) {
	t.Parallel()

	t.Run("no validator installed returns nil", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.ValidateNode("n"); err != nil {
			t.Fatalf("want nil with no validator, got %v", err)
		}
	})

	t.Run("validator without NodeValidator returns nil", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		g.SetValidator(propOnlyValidator{})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.ValidateNode("n"); err != nil {
			t.Fatalf("want nil for a non-NodeValidator, got %v", err)
		}
	})

	t.Run("NodeValidator verdict is returned", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		g.SetValidator(rejectAllValidator{})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.ValidateNode("n"); !errors.Is(err, errEveryNode) {
			t.Fatalf("want errEveryNode, got %v", err)
		}
	})

	t.Run("unknown key returns nil", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		g.SetValidator(rejectAllValidator{})
		// "ghost" was never interned: nothing to finalise, so no rejection.
		if err := g.ValidateNode("ghost"); err != nil {
			t.Fatalf("want nil for an uninterned key, got %v", err)
		}
	})
}

// propOnlyValidator implements lpg.SchemaValidator but NOT lpg.NodeValidator,
// to prove Graph.ValidateNode treats whole-node enforcement as opt-in.
type propOnlyValidator struct{}

func (propOnlyValidator) Validate(string, lpg.PropertyValue) error { return nil }
