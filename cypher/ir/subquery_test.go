package ir_test

// subquery_test.go — unit tests for [ir.SubqueryExists] and [ir.SubqueryCount]
// IR node containers (task-396).

import (
	"reflect"
	"testing"

	"gograph/cypher/ir"
)

// TestSubqueryExists_BasicConstruction asserts the constructor seeds an
// ArgTag, copies the correlation variables, and exposes Inner via Children.
func TestSubqueryExists_BasicConstruction(t *testing.T) {
	inner := ir.NewAllNodesScan("a")
	corr := []string{"x", "y"}
	sq := ir.NewSubqueryExists(inner, corr)

	if sq.Inner != inner {
		t.Errorf("Inner = %v, want %v", sq.Inner, inner)
	}
	if !reflect.DeepEqual(sq.CorrelationVars, corr) {
		t.Errorf("CorrelationVars = %v, want %v", sq.CorrelationVars, corr)
	}
	if sq.ArgTag == 0 {
		t.Errorf("ArgTag should be non-zero")
	}

	// CorrelationVars must be a defensive copy: mutating the caller's slice
	// must not affect the IR node's internal state.
	corr[0] = "MUTATED"
	if sq.CorrelationVars[0] == "MUTATED" {
		t.Errorf("CorrelationVars not defensively copied; mutation leaked: %v", sq.CorrelationVars)
	}

	// Children() must return [Inner].
	children := sq.Children()
	if len(children) != 1 || children[0] != inner {
		t.Errorf("Children() = %v, want [%v]", children, inner)
	}

	// Vars() must return nil — the subquery yields a boolean, not a variable.
	if vs := sq.Vars(); vs != nil {
		t.Errorf("Vars() = %v, want nil", vs)
	}
}

// TestSubqueryExists_WithTag asserts NewSubqueryExistsWithTag preserves the
// caller-supplied tag verbatim.
func TestSubqueryExists_WithTag(t *testing.T) {
	const tag uint32 = 42
	sq := ir.NewSubqueryExistsWithTag(ir.NewAllNodesScan("a"), []string{"x"}, tag)
	if sq.ArgTag != tag {
		t.Errorf("ArgTag = %d, want %d", sq.ArgTag, tag)
	}
}

// TestSubqueryCount_BasicConstruction mirrors TestSubqueryExists_BasicConstruction
// for the COUNT { … } container.
func TestSubqueryCount_BasicConstruction(t *testing.T) {
	inner := ir.NewAllNodesScan("a")
	corr := []string{"x", "y"}
	sq := ir.NewSubqueryCount(inner, corr)

	if sq.Inner != inner {
		t.Errorf("Inner = %v, want %v", sq.Inner, inner)
	}
	if !reflect.DeepEqual(sq.CorrelationVars, corr) {
		t.Errorf("CorrelationVars = %v, want %v", sq.CorrelationVars, corr)
	}
	if sq.ArgTag == 0 {
		t.Errorf("ArgTag should be non-zero")
	}

	corr[0] = "MUTATED"
	if sq.CorrelationVars[0] == "MUTATED" {
		t.Errorf("CorrelationVars not defensively copied")
	}

	children := sq.Children()
	if len(children) != 1 || children[0] != inner {
		t.Errorf("Children() = %v, want [%v]", children, inner)
	}

	if vs := sq.Vars(); vs != nil {
		t.Errorf("Vars() = %v, want nil", vs)
	}
}

// TestSubqueryCount_WithTag asserts the explicit-tag constructor preserves
// the caller-supplied tag.
func TestSubqueryCount_WithTag(t *testing.T) {
	const tag uint32 = 99
	sq := ir.NewSubqueryCountWithTag(ir.NewAllNodesScan("a"), []string{"x"}, tag)
	if sq.ArgTag != tag {
		t.Errorf("ArgTag = %d, want %d", sq.ArgTag, tag)
	}
}

// TestSubqueryNodes_DistinctTags asserts subsequent NewSubqueryExists /
// NewSubqueryCount calls receive distinct ArgTags so they cannot accidentally
// share an exec.Argument instance under the physical builder.
func TestSubqueryNodes_DistinctTags(t *testing.T) {
	a := ir.NewSubqueryExists(ir.NewAllNodesScan("x"), nil)
	b := ir.NewSubqueryExists(ir.NewAllNodesScan("y"), nil)
	c := ir.NewSubqueryCount(ir.NewAllNodesScan("z"), nil)
	if a.ArgTag == b.ArgTag || a.ArgTag == c.ArgTag || b.ArgTag == c.ArgTag {
		t.Errorf("expected distinct tags, got %d, %d, %d", a.ArgTag, b.ArgTag, c.ArgTag)
	}
}
