package procs

// builtin_db.go — built-in db.* procedure registrations (task-300).
//
// RegisterBuiltins registers the standard db.* introspection procedures into
// reg. mgr and every closure in BuiltinSources may be nil; procedures that
// depend on a nil source return empty result sets in that case.
//
// Registered procedures:
//
//   - db.indexes()           → name string, type string
//   - db.constraints()       → name string, type string, label string, property string
//   - db.labels()            → label string
//   - db.relationshipTypes() → relationshipType string
//   - db.propertyKeys()      → propertyKey string
//   - db.schema.visualization() → nodes list, relationships list

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// BuiltinSources bundles the data-source callbacks the built-in db.* procedures
// query at invocation time. It decouples the procs package from the concrete
// graph type: callers (typically the engine) supply pure closures that read
// live graph state, and procs never imports the graph layer.
//
// Every field is optional. A nil closure makes its corresponding procedure
// return an empty result set, mirroring the nil-index-manager behaviour of
// db.indexes() and db.labels().
type BuiltinSources struct {
	// ListConstraints is invoked by db.constraints() to obtain the current
	// constraint rows (each row is [name, type, label, property]).
	ListConstraints func() [][]expr.Value
	// Labels is invoked by db.labels() to obtain the distinct node labels in
	// use, one per returned name. (Wired but not yet consumed by db.labels().)
	Labels func() []string
	// RelationshipTypes is invoked by db.relationshipTypes() to obtain the
	// distinct relationship types in use, one per returned name.
	RelationshipTypes func() []string
	// PropertyKeys is invoked by db.propertyKeys() to obtain the distinct
	// property keys in use, one per returned name. (Wired but not yet consumed
	// by db.propertyKeys().)
	PropertyKeys func() []string
}

// RegisterBuiltins registers all built-in db.* procedures into reg.
//
// mgr is the index manager used by db.indexes() and db.labels(); it may be nil
// (both procedures return empty results).
//
// src carries the enumeration closures backing db.constraints(), db.labels(),
// db.relationshipTypes() and db.propertyKeys(); see [BuiltinSources]. Each
// closure may be nil, in which case its procedure returns an empty set.
//
//nolint:gocritic // hugeParam: BuiltinSources is small and passed by value intentionally
func RegisterBuiltins(reg *Registry, mgr *index.Manager, src BuiltinSources) {
	mustRegister(reg, dbIndexes(mgr))
	mustRegister(reg, dbConstraints(src.ListConstraints))
	mustRegister(reg, dbLabels(mgr))
	mustRegister(reg, dbRelationshipTypes(src.RelationshipTypes))
	mustRegister(reg, dbPropertyKeys())
	mustRegister(reg, dbSchemaVisualization())
}

// mustRegister panics when Register returns an error. It is only called for
// built-in procedures that are known to have no duplicates among themselves;
// user code should call reg.Register directly and handle the error.
//
//nolint:gocritic // hugeParam: ProcEntry is passed by value intentionally; callers own the struct
func mustRegister(reg *Registry, entry ProcEntry) {
	if err := reg.Register(entry.Sig, entry.Impl); err != nil {
		panic("procs: RegisterBuiltins: " + err.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.indexes()
// ─────────────────────────────────────────────────────────────────────────────

func dbIndexes(mgr *index.Manager) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "indexes",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "name", Kind: expr.KindString},
				{Name: "type", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if mgr == nil {
				return nil, nil
			}
			names := mgr.ListIndexes()
			rows := make([][]expr.Value, 0, len(names))
			for _, name := range names {
				sub, err := mgr.GetIndex(name)
				if err != nil {
					continue
				}
				rows = append(rows, []expr.Value{
					expr.StringValue(name),
					expr.StringValue(sub.Kind()),
				})
			}
			return rows, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.constraints()
// ─────────────────────────────────────────────────────────────────────────────

func dbConstraints(listConstraints func() [][]expr.Value) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "constraints",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "name", Kind: expr.KindString},
				{Name: "type", Kind: expr.KindString},
				{Name: "label", Kind: expr.KindString},
				{Name: "property", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if listConstraints == nil {
				return nil, nil
			}
			return listConstraints(), nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.labels()
// ─────────────────────────────────────────────────────────────────────────────

func dbLabels(mgr *index.Manager) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "labels",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "label", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if mgr == nil {
				return nil, nil
			}
			names := mgr.ListIndexes()
			rows := make([][]expr.Value, 0)
			for _, name := range names {
				sub, err := mgr.GetIndex(name)
				if err != nil {
					continue
				}
				if sub.Kind() == "label" {
					rows = append(rows, []expr.Value{expr.StringValue(name)})
				}
			}
			return rows, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.relationshipTypes()
// ─────────────────────────────────────────────────────────────────────────────

func dbRelationshipTypes(listTypes func() []string) ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "relationshipTypes",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "relationshipType", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			if listTypes == nil {
				return nil, nil
			}
			names := listTypes()
			rows := make([][]expr.Value, 0, len(names))
			for _, name := range names {
				rows = append(rows, []expr.Value{expr.StringValue(name)})
			}
			return rows, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.propertyKeys()
// ─────────────────────────────────────────────────────────────────────────────

func dbPropertyKeys() ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db"},
			Name:      "propertyKeys",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "propertyKey", Kind: expr.KindString},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			return nil, nil
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// db.schema.visualization()
// ─────────────────────────────────────────────────────────────────────────────

func dbSchemaVisualization() ProcEntry {
	return ProcEntry{
		Sig: Signature{
			Namespace: []string{"db", "schema"},
			Name:      "visualization",
			Inputs:    nil,
			Outputs: []NamedType{
				{Name: "nodes", Kind: expr.KindList},
				{Name: "relationships", Kind: expr.KindList},
			},
		},
		Impl: func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			return nil, nil
		},
	}
}
