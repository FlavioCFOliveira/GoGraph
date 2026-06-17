package procs_test

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// newManagerWithLabel creates an index.Manager with a single label index
// registered under name.
func newManagerWithLabel(t *testing.T, name string) *index.Manager {
	t.Helper()
	mgr := index.NewManager()
	lbl := label.NewIndex()
	if err := mgr.CreateIndex(name, lbl); err != nil {
		t.Fatalf("CreateIndex %q: %v", name, err)
	}
	return mgr
}

// ─────────────────────────────────────────────────────────────────────────────
// RegisterBuiltins
// ─────────────────────────────────────────────────────────────────────────────

func TestRegisterBuiltins_RegistersAll(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})

	expected := []struct {
		ns   []string
		name string
	}{
		{[]string{"db"}, "indexes"},
		{[]string{"db"}, "constraints"},
		{[]string{"db"}, "labels"},
		{[]string{"db"}, "relationshipTypes"},
		{[]string{"db"}, "propertyKeys"},
		{[]string{"db", "schema"}, "visualization"},
	}
	for _, tc := range expected {
		_, err := reg.Lookup(tc.ns, tc.name)
		if err != nil {
			t.Errorf("Lookup %v.%s: %v", tc.ns, tc.name, err)
		}
	}
}

func TestRegisterBuiltins_Idempotent_Error(t *testing.T) {
	t.Parallel()
	// Calling RegisterBuiltins twice should panic on the second call because
	// the procedures are already registered. Verify it panics.
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double registration, got none")
		}
	}()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
}

// ─────────────────────────────────────────────────────────────────────────────
// db.indexes()
// ─────────────────────────────────────────────────────────────────────────────

func TestDbIndexes_NilManager(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "indexes")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("db.indexes() with nil mgr: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows with nil manager, got %d", len(rows))
	}
}

func TestDbIndexes_WithManager(t *testing.T) {
	t.Parallel()
	mgr := newManagerWithLabel(t, "Person")
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, mgr, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "indexes")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("db.indexes(): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][0] != expr.StringValue("Person") {
		t.Errorf("name = %v, want Person", rows[0][0])
	}
	if rows[0][1] != expr.StringValue("label") {
		t.Errorf("type = %v, want label", rows[0][1])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.constraints()
// ─────────────────────────────────────────────────────────────────────────────

func TestDbConstraints_NilCallback(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "constraints")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("db.constraints() with nil callback: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestDbConstraints_WithCallback(t *testing.T) {
	t.Parallel()
	callback := func() [][]expr.Value {
		return [][]expr.Value{
			{
				expr.StringValue("Person.name"),
				expr.StringValue("UNIQUE"),
				expr.StringValue("Person"),
				expr.StringValue("name"),
			},
		}
	}
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{ListConstraints: callback})
	entry, _ := reg.Lookup([]string{"db"}, "constraints")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("db.constraints(): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][0] != expr.StringValue("Person.name") {
		t.Errorf("name = %v, want Person.name", rows[0][0])
	}
	if rows[0][1] != expr.StringValue("UNIQUE") {
		t.Errorf("type = %v, want UNIQUE", rows[0][1])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.labels()
// ─────────────────────────────────────────────────────────────────────────────

func TestDbLabels_NilManager(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "labels")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("db.labels() with nil mgr: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestDbLabels_FiltersLabelKind(t *testing.T) {
	t.Parallel()
	mgr := newManagerWithLabel(t, "Person")
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, mgr, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "labels")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil {
		t.Fatalf("db.labels(): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][0] != expr.StringValue("Person") {
		t.Errorf("label = %v, want Person", rows[0][0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.relationshipTypes() / db.propertyKeys() / db.schema.visualization()
// ─────────────────────────────────────────────────────────────────────────────

func TestDbRelationshipTypes_Empty(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "relationshipTypes")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil || len(rows) != 0 {
		t.Errorf("db.relationshipTypes(): got rows=%v err=%v", rows, err)
	}
}

func TestDbRelationshipTypes_WithClosure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		types []string
		want  []string
	}{
		{name: "two types", types: []string{"KNOWS", "LIKES"}, want: []string{"KNOWS", "LIKES"}},
		{name: "single type", types: []string{"REL"}, want: []string{"REL"}},
		{name: "empty slice", types: []string{}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := procs.NewRegistry()
			procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{
				RelationshipTypes: func() []string { return tc.types },
			})
			entry, _ := reg.Lookup([]string{"db"}, "relationshipTypes")
			rows, err := entry.Impl(context.Background(), nil)
			if err != nil {
				t.Fatalf("db.relationshipTypes(): %v", err)
			}
			if len(rows) != len(tc.want) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tc.want))
			}
			for i, want := range tc.want {
				if len(rows[i]) != 1 {
					t.Fatalf("row %d width = %d, want 1", i, len(rows[i]))
				}
				if rows[i][0] != expr.StringValue(want) {
					t.Errorf("row %d = %v, want %q", i, rows[i][0], want)
				}
			}
		})
	}
}

func TestDbPropertyKeys_Empty(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db"}, "propertyKeys")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil || len(rows) != 0 {
		t.Errorf("db.propertyKeys(): got rows=%v err=%v", rows, err)
	}
}

func TestDbSchemaVisualization_Empty(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	procs.RegisterBuiltins(reg, nil, procs.BuiltinSources{})
	entry, _ := reg.Lookup([]string{"db", "schema"}, "visualization")
	rows, err := entry.Impl(context.Background(), nil)
	if err != nil || len(rows) != 0 {
		t.Errorf("db.schema.visualization(): got rows=%v err=%v", rows, err)
	}
}
