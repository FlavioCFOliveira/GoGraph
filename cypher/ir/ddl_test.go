package ir_test

import (
	"strings"
	"testing"

	"gograph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// IsDDL
// ─────────────────────────────────────────────────────────────────────────────

func TestIsDDL(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"CREATE INDEX foo FOR (n:Person) ON (n.name)", true},
		{"create index foo for (n:Person) on (n.name)", true},
		{"DROP INDEX foo", true},
		{"drop index foo", true},
		{"CREATE CONSTRAINT c ON (n:Person) ASSERT n.name IS UNIQUE", true},
		{"DROP CONSTRAINT c", true},
		{"MATCH (n) RETURN n", false},
		{"CREATE (n:Person)", false},
		{"", false},
	}
	for _, tc := range cases {
		got := ir.IsDDL(tc.query)
		if got != tc.want {
			t.Errorf("IsDDL(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ParseDDL — dispatch and error
// ─────────────────────────────────────────────────────────────────────────────

func TestParseDDL_UnknownStatement(t *testing.T) {
	_, err := ir.ParseDDL("ALTER INDEX foo")
	if err == nil {
		t.Fatal("expected error for unrecognised DDL")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CREATE INDEX
// ─────────────────────────────────────────────────────────────────────────────

func TestParseDDL_CreateIndex_Basic(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE INDEX myidx FOR (n:Person) ON (n.name)`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	ci, ok := plan.(*ir.CreateIndex)
	if !ok {
		t.Fatalf("expected *CreateIndex, got %T", plan)
	}
	if ci.Name != "myidx" {
		t.Errorf("Name = %q, want %q", ci.Name, "myidx")
	}
	if ci.Label != "Person" {
		t.Errorf("Label = %q, want %q", ci.Label, "Person")
	}
	if ci.Property != "name" {
		t.Errorf("Property = %q, want %q", ci.Property, "name")
	}
	if ci.Type != ir.IndexTypeHash {
		t.Errorf("Type = %v, want IndexTypeHash", ci.Type)
	}
	if ci.IfNotExists {
		t.Error("IfNotExists should be false")
	}
	if ci.Children() != nil {
		t.Error("expected nil Children()")
	}
	if ci.Vars() != nil {
		t.Error("expected nil Vars()")
	}
}

func TestParseDDL_CreateIndex_IfNotExists(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE INDEX IF NOT EXISTS myidx FOR (n:Person) ON (n.name)`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	ci := plan.(*ir.CreateIndex)
	if !ci.IfNotExists {
		t.Error("IfNotExists should be true")
	}
}

func TestParseDDL_CreateIndex_BTreeOption(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE INDEX mybtree FOR (n:Person) ON (n.name) OPTIONS {indexType: 'btree'}`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	ci := plan.(*ir.CreateIndex)
	if ci.Type != ir.IndexTypeBTree {
		t.Errorf("Type = %v, want IndexTypeBTree", ci.Type)
	}
}

func TestParseDDL_CreateIndex_HashOption(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE INDEX myh FOR (n:Person) ON (n.name) OPTIONS {indexType: 'hash'}`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	ci := plan.(*ir.CreateIndex)
	if ci.Type != ir.IndexTypeHash {
		t.Errorf("Type = %v, want IndexTypeHash", ci.Type)
	}
}

func TestParseDDL_CreateIndex_UnknownOption(t *testing.T) {
	_, err := ir.ParseDDL(`CREATE INDEX myidx FOR (n:Person) ON (n.name) OPTIONS {indexType: 'unknown'}`)
	if err == nil {
		t.Fatal("expected error for unknown indexType")
	}
}

func TestParseDDL_CreateIndex_AutoName(t *testing.T) {
	// No name — auto-name should be generated.
	plan, err := ir.ParseDDL(`CREATE INDEX FOR (n:Person) ON (n.name)`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	ci := plan.(*ir.CreateIndex)
	if ci.Name == "" {
		t.Error("expected auto-generated name, got empty string")
	}
	if !strings.Contains(ci.Name, "person") {
		t.Errorf("auto-name %q should contain label in lower case", ci.Name)
	}
}

func TestParseDDL_CreateIndex_AutoNameBTree(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE INDEX FOR (n:Tag) ON (n.val) OPTIONS {indexType: 'btree'}`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	ci := plan.(*ir.CreateIndex)
	if !strings.Contains(ci.Name, "btree") {
		t.Errorf("auto-name %q should contain btree", ci.Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DROP INDEX
// ─────────────────────────────────────────────────────────────────────────────

func TestParseDDL_DropIndex_Basic(t *testing.T) {
	plan, err := ir.ParseDDL(`DROP INDEX myidx`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	di, ok := plan.(*ir.DropIndex)
	if !ok {
		t.Fatalf("expected *DropIndex, got %T", plan)
	}
	if di.Name != "myidx" {
		t.Errorf("Name = %q, want %q", di.Name, "myidx")
	}
	if di.IfExists {
		t.Error("IfExists should be false")
	}
	if di.Children() != nil {
		t.Error("expected nil Children()")
	}
	if di.Vars() != nil {
		t.Error("expected nil Vars()")
	}
}

func TestParseDDL_DropIndex_IfExists(t *testing.T) {
	plan, err := ir.ParseDDL(`DROP INDEX myidx IF EXISTS`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	di := plan.(*ir.DropIndex)
	if !di.IfExists {
		t.Error("IfExists should be true")
	}
}

func TestParseDDL_DropIndex_MissingName(t *testing.T) {
	_, err := ir.ParseDDL(`DROP INDEX`)
	if err == nil {
		t.Fatal("expected error for missing index name")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CREATE CONSTRAINT
// ─────────────────────────────────────────────────────────────────────────────

func TestParseDDL_CreateConstraint_Unique(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE CONSTRAINT c1 ON (n:Person) ASSERT n.email IS UNIQUE`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	cc, ok := plan.(*ir.CreateConstraint)
	if !ok {
		t.Fatalf("expected *CreateConstraint, got %T", plan)
	}
	if cc.Name != "c1" {
		t.Errorf("Name = %q", cc.Name)
	}
	if cc.Label != "Person" {
		t.Errorf("Label = %q", cc.Label)
	}
	if cc.Property != "email" {
		t.Errorf("Property = %q", cc.Property)
	}
	if cc.Kind != ir.ConstraintUnique {
		t.Errorf("Kind = %v, want ConstraintUnique", cc.Kind)
	}
	if cc.IfNotExists {
		t.Error("IfNotExists should be false")
	}
	if cc.Children() != nil {
		t.Error("expected nil Children()")
	}
	if cc.Vars() != nil {
		t.Error("expected nil Vars()")
	}
}

func TestParseDDL_CreateConstraint_NotNull(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE CONSTRAINT c2 ON (n:Person) ASSERT n.name IS NOT NULL`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	cc := plan.(*ir.CreateConstraint)
	if cc.Kind != ir.ConstraintNotNull {
		t.Errorf("Kind = %v, want ConstraintNotNull", cc.Kind)
	}
}

func TestParseDDL_CreateConstraint_IfNotExists(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE CONSTRAINT c3 ON (n:Person) ASSERT n.email IS UNIQUE IF NOT EXISTS`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	cc := plan.(*ir.CreateConstraint)
	if !cc.IfNotExists {
		t.Error("IfNotExists should be true")
	}
}

func TestParseDDL_CreateConstraint_AutoName(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE CONSTRAINT ON (n:Email) ASSERT n.addr IS UNIQUE`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	cc := plan.(*ir.CreateConstraint)
	if cc.Name == "" {
		t.Error("expected auto-generated name")
	}
}

func TestParseDDL_CreateConstraint_AutoNameNotNull(t *testing.T) {
	plan, err := ir.ParseDDL(`CREATE CONSTRAINT ON (n:Tag) ASSERT n.key IS NOT NULL`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	cc := plan.(*ir.CreateConstraint)
	if !strings.Contains(cc.Name, "not_null") {
		t.Errorf("auto-name %q should contain 'not_null'", cc.Name)
	}
}

func TestParseDDL_CreateConstraint_UnknownAssertion(t *testing.T) {
	_, err := ir.ParseDDL(`CREATE CONSTRAINT c ON (n:Person) ASSERT n.email IS INDEXED`)
	if err == nil {
		t.Fatal("expected error for unknown assertion type")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DROP CONSTRAINT
// ─────────────────────────────────────────────────────────────────────────────

func TestParseDDL_DropConstraint_Basic(t *testing.T) {
	plan, err := ir.ParseDDL(`DROP CONSTRAINT c1`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	dc, ok := plan.(*ir.DropConstraint)
	if !ok {
		t.Fatalf("expected *DropConstraint, got %T", plan)
	}
	if dc.Name != "c1" {
		t.Errorf("Name = %q", dc.Name)
	}
	if dc.IfExists {
		t.Error("IfExists should be false")
	}
	if dc.Children() != nil {
		t.Error("expected nil Children()")
	}
	if dc.Vars() != nil {
		t.Error("expected nil Vars()")
	}
}

func TestParseDDL_DropConstraint_IfExists(t *testing.T) {
	plan, err := ir.ParseDDL(`DROP CONSTRAINT c1 IF EXISTS`)
	if err != nil {
		t.Fatalf("ParseDDL: %v", err)
	}
	dc := plan.(*ir.DropConstraint)
	if !dc.IfExists {
		t.Error("IfExists should be true")
	}
}

func TestParseDDL_DropConstraint_MissingName(t *testing.T) {
	_, err := ir.ParseDDL(`DROP CONSTRAINT`)
	if err == nil {
		t.Fatal("expected error for missing constraint name")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DDL IR node types: NewCreateIndex / NewDropIndex / NewCreateConstraint / NewDropConstraint
// ─────────────────────────────────────────────────────────────────────────────

func TestNewCreateIndex_Direct(t *testing.T) {
	ci := ir.NewCreateIndex("idx", "Label", "prop", ir.IndexTypeHash, false)
	if ci.Name != "idx" || ci.Label != "Label" || ci.Property != "prop" {
		t.Errorf("unexpected CreateIndex fields: %+v", ci)
	}
	if ci.Children() != nil || ci.Vars() != nil {
		t.Error("leaf node should return nil Children and Vars")
	}
}

func TestNewDropIndex_Direct(t *testing.T) {
	di := ir.NewDropIndex("idx", true)
	if di.Name != "idx" || !di.IfExists {
		t.Errorf("unexpected DropIndex fields: %+v", di)
	}
	if di.Children() != nil || di.Vars() != nil {
		t.Error("leaf node should return nil Children and Vars")
	}
}

func TestNewCreateConstraint_Direct(t *testing.T) {
	cc := ir.NewCreateConstraint("c", "Label", "prop", ir.ConstraintUnique, true)
	if cc.Name != "c" || cc.Label != "Label" || cc.Property != "prop" {
		t.Errorf("unexpected CreateConstraint fields: %+v", cc)
	}
	if cc.Children() != nil || cc.Vars() != nil {
		t.Error("leaf node should return nil Children and Vars")
	}
}

func TestNewDropConstraint_Direct(t *testing.T) {
	dc := ir.NewDropConstraint("c", "Label", "prop", ir.ConstraintUnique, false)
	if dc.Name != "c" || dc.Label != "Label" {
		t.Errorf("unexpected DropConstraint fields: %+v", dc)
	}
	if dc.Children() != nil || dc.Vars() != nil {
		t.Error("leaf node should return nil Children and Vars")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// operatorName coverage via Explain — uncovered operator types
// ─────────────────────────────────────────────────────────────────────────────

func TestExplain_OperatorNames_Coverage(t *testing.T) {
	scan := ir.NewAllNodesScan("n")

	plans := []ir.LogicalPlan{
		ir.NewOptionalExpand("n", "r", nil, ir.DirectionOutgoing, "m", scan),
		ir.NewVarLengthExpand("n", "r", nil, ir.DirectionOutgoing, "m", 1, 3, scan),
		ir.NewProjectEndpoints("r", "s", "e", scan),
		ir.NewTop([]ir.SortItem{{Expression: "n", Descending: false}}, 5, scan),
		ir.NewUnionAll(scan, scan),
		ir.NewSemiApply(scan, scan),
		ir.NewAntiSemiApply(scan, scan),
		ir.NewRollUpApply(scan, scan, "collected"),
		ir.NewEager(scan),
		ir.NewUnwind("list", "x", scan),
		ir.NewArgument([]string{"n"}),
		ir.NewCreateNode("n", []string{"Person"}, "", scan),
		ir.NewCreateRelationship("a", "b", "r", "KNOWS", "", scan),
		ir.NewSetProperty("n", "age", "30", scan),
		ir.NewSetLabels("n", []string{"Employee"}, scan),
		ir.NewRemoveProperty("n", "age", scan),
		ir.NewRemoveLabels("n", []string{"Temp"}, scan),
		ir.NewDeleteNode("n", scan),
		ir.NewDeleteRelationship("r", scan),
		ir.NewDetachDelete("n", scan),
		ir.NewMerge("(n:Person {name: 'Alice'})", nil, nil, nil, scan),
		ir.NewProcedureCall(nil, "db.info", nil, nil, scan),
	}

	for _, p := range plans {
		out := ir.Explain(p)
		if out == "" {
			t.Errorf("Explain returned empty string for %T", p)
		}
	}
}
