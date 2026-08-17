package sim

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// schema_introspect.go — DDL-model schema-introspection oracle (rmp #2455).
//
// The harness keeps ITS OWN model of every index and constraint it has issued
// ([SchemaModel]) and, at DDL boundaries, after crash/recovery, and at the end
// of a run, asserts that the engine's TWO independent introspection surfaces —
// the modern SHOW INDEXES / SHOW CONSTRAINTS statements (cypher/show.go) and
// the legacy db.indexes() / db.constraints() procedures
// (cypher/procs/builtin_db.go) — both reproduce the model exactly, and agree
// with each other. A recovered-DDL divergence (an index or constraint that
// recovery dropped, resurrected, or re-registered with the wrong shape) is
// therefore observable, where before this oracle no simulation ever invoked
// SHOW or the db.indexes()/db.constraints() procedures at all.
//
// The expected row shapes are pinned against the engine's actual projection
// contracts, verified empirically against cypher/show.go:
//
//	SHOW INDEXES     → [name, state, type, entityType, labelsOrTypes, properties]
//	                   state is always "ONLINE", entityType always "NODE";
//	                   a UNIQUE constraint's backing index ("__uniq__<L>.<p>",
//	                   kind "hash") is listed with EMPTY labelsOrTypes and
//	                   properties lists (it is not in the index-def registry).
//	SHOW CONSTRAINTS → [name, type, entityType, labelsOrTypes, properties]
//	                   type is "UNIQUE" | "NOT_NULL", entityType always "NODE".
//	db.indexes()     → [name, type]
//	db.constraints() → [name, type, label, property]
//
// Each check also drives the SHOW ... YIELD ... WHERE ... RETURN projection
// path (#2044) with one constraints form and one indexes form (the latter with
// a YIELD alias), and executes db.schema.visualization() end-to-end (its row
// set is not pinned — the procedure currently yields no rows — but it must
// execute and drain without error).

// Schema-model kind vocabulary, matching the engine's introspection values.
const (
	// SchemaIndexHash is the hash index kind as reported by SHOW INDEXES and
	// db.indexes(). It is also the default kind of CREATE INDEX without OPTIONS.
	SchemaIndexHash = "hash"
	// SchemaIndexBTree is the btree index kind.
	SchemaIndexBTree = "btree"
	// SchemaConstraintUnique is the UNIQUE constraint kind as reported by
	// SHOW CONSTRAINTS and db.constraints().
	SchemaConstraintUnique = "UNIQUE"
	// SchemaConstraintNotNull is the NOT NULL (existence) constraint kind.
	SchemaConstraintNotNull = "NOT_NULL"
)

// schemaIndexDef is one modelled user index.
type schemaIndexDef struct {
	kind  string // SchemaIndexHash | SchemaIndexBTree
	label string
	prop  string
}

// schemaConstraintDef is one modelled constraint.
type schemaConstraintDef struct {
	kind  string // SchemaConstraintUnique | SchemaConstraintNotNull
	label string
	prop  string
}

// SchemaModel is the harness's own model of every index and constraint the
// scenario has issued. Scenarios mutate it in lock-step with the DDL they run
// (AddIndex on CREATE INDEX, DropConstraint on DROP CONSTRAINT, ...) and
// [CheckSchemaIntrospection] holds the engine's introspection surfaces to it.
//
// A UNIQUE constraint implies its hash backing index ("__uniq__<label>.<prop>"),
// which the engine lists in SHOW INDEXES / db.indexes(); the model derives that
// row automatically, so scenarios only declare what they issued.
//
// # Concurrency contract
//
// SchemaModel is NOT safe for concurrent use; it is driven from the single
// simulation goroutine.
type SchemaModel struct {
	indexes     map[string]schemaIndexDef
	constraints map[string]schemaConstraintDef
}

// NewSchemaModel returns an empty schema model.
func NewSchemaModel() *SchemaModel {
	return &SchemaModel{
		indexes:     make(map[string]schemaIndexDef),
		constraints: make(map[string]schemaConstraintDef),
	}
}

// AddIndex records a user index the scenario created. kind is
// [SchemaIndexHash] or [SchemaIndexBTree]; label/prop are the indexed
// (label, property) pair recorded by the engine's index-def registry.
func (m *SchemaModel) AddIndex(name, kind, label, prop string) {
	m.indexes[name] = schemaIndexDef{kind: kind, label: label, prop: prop}
}

// DropIndex removes a user index from the model (a DROP INDEX was issued).
func (m *SchemaModel) DropIndex(name string) { delete(m.indexes, name) }

// AddUniqueConstraint records a UNIQUE constraint the scenario created. Its
// hash backing index row is derived automatically.
func (m *SchemaModel) AddUniqueConstraint(name, label, prop string) {
	m.constraints[name] = schemaConstraintDef{kind: SchemaConstraintUnique, label: label, prop: prop}
}

// AddNotNullConstraint records a NOT NULL (existence) constraint the scenario
// created. Existence constraints have no backing index.
func (m *SchemaModel) AddNotNullConstraint(name, label, prop string) {
	m.constraints[name] = schemaConstraintDef{kind: SchemaConstraintNotNull, label: label, prop: prop}
}

// DropConstraint removes a constraint from the model (a DROP CONSTRAINT was
// issued). For a UNIQUE constraint the derived backing-index row disappears
// with it, matching the engine's by-name drop (#1556), which removes the
// "__uniq__" backing index alongside the constraint.
func (m *SchemaModel) DropConstraint(name string) { delete(m.constraints, name) }

// backingIndexName returns the engine's reserved backing-index name for a
// UNIQUE constraint on (label, prop), pinned by
// cypher: TestCreateConstraint_BackingIndexName ("__uniq__<Label>.<property>").
func backingIndexName(label, prop string) string {
	return "__uniq__" + label + "." + prop
}

// indexEnumRow is one entry of the model's expected index enumeration: the
// user indexes plus each UNIQUE constraint's backing index.
type indexEnumRow struct {
	name    string
	kind    string
	label   string
	prop    string
	backing bool // backing rows report empty labelsOrTypes/properties in SHOW
}

// indexEnumeration returns the expected index enumeration sorted by name — the
// order both CollectIndexRows (db.indexes()) and SHOW INDEXES emit.
func (m *SchemaModel) indexEnumeration() []indexEnumRow {
	rows := make([]indexEnumRow, 0, len(m.indexes)+len(m.constraints))
	for name, def := range m.indexes {
		rows = append(rows, indexEnumRow{name: name, kind: def.kind, label: def.label, prop: def.prop})
	}
	for _, def := range m.constraints {
		if def.kind == SchemaConstraintUnique {
			rows = append(rows, indexEnumRow{
				name: backingIndexName(def.label, def.prop), kind: SchemaIndexHash, backing: true,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	return rows
}

// constraintEnumeration returns the expected constraint rows sorted by name.
func (m *SchemaModel) constraintEnumeration() []struct {
	name string
	def  schemaConstraintDef
} {
	rows := make([]struct {
		name string
		def  schemaConstraintDef
	}, 0, len(m.constraints))
	for name, def := range m.constraints {
		rows = append(rows, struct {
			name string
			def  schemaConstraintDef
		}{name, def})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	return rows
}

// expectedShowIndexRows renders the expected SHOW INDEXES row set in the
// engine's canonical value strings.
func (m *SchemaModel) expectedShowIndexRows() [][]string {
	enum := m.indexEnumeration()
	rows := make([][]string, 0, len(enum))
	for _, r := range enum {
		labels, props := []any{}, []any{}
		if !r.backing {
			labels, props = []any{r.label}, []any{r.prop}
		}
		rows = append(rows, []string{
			canonicalValueString(r.name),
			canonicalValueString("ONLINE"),
			canonicalValueString(r.kind),
			canonicalValueString("NODE"),
			canonicalValueString(labels),
			canonicalValueString(props),
		})
	}
	return rows
}

// expectedDBIndexRows renders the expected db.indexes() row set.
func (m *SchemaModel) expectedDBIndexRows() [][]string {
	enum := m.indexEnumeration()
	rows := make([][]string, 0, len(enum))
	for _, r := range enum {
		rows = append(rows, []string{canonicalValueString(r.name), canonicalValueString(r.kind)})
	}
	return rows
}

// expectedShowConstraintRows renders the expected SHOW CONSTRAINTS row set.
func (m *SchemaModel) expectedShowConstraintRows() [][]string {
	enum := m.constraintEnumeration()
	rows := make([][]string, 0, len(enum))
	for _, r := range enum {
		rows = append(rows, []string{
			canonicalValueString(r.name),
			canonicalValueString(r.def.kind),
			canonicalValueString("NODE"),
			canonicalValueString([]any{r.def.label}),
			canonicalValueString([]any{r.def.prop}),
		})
	}
	return rows
}

// expectedDBConstraintRows renders the expected db.constraints() row set.
func (m *SchemaModel) expectedDBConstraintRows() [][]string {
	enum := m.constraintEnumeration()
	rows := make([][]string, 0, len(enum))
	for _, r := range enum {
		rows = append(rows, []string{
			canonicalValueString(r.name),
			canonicalValueString(r.def.kind),
			canonicalValueString(r.def.label),
			canonicalValueString(r.def.prop),
		})
	}
	return rows
}

// uniqueConstraintNames returns the modelled UNIQUE constraint names sorted,
// backing the YIELD/WHERE/RETURN projection probe.
func (m *SchemaModel) uniqueConstraintNames() []string {
	var names []string
	for name, def := range m.constraints {
		if def.kind == SchemaConstraintUnique {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// btreeIndexNames returns the modelled btree user-index names sorted, backing
// the aliased YIELD projection probe (backing indexes are hash, never btree).
func (m *SchemaModel) btreeIndexNames() []string {
	var names []string
	for name, def := range m.indexes {
		if def.kind == SchemaIndexBTree {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// constraintNameList returns every modelled constraint name, sorted — the
// enumeration the `CALL db.constraints() YIELD … WHERE …` probe filters
// (rmp #2462).
func (m *SchemaModel) constraintNameList() []string {
	enum := m.constraintEnumeration()
	names := make([]string, 0, len(enum))
	for _, r := range enum {
		names = append(names, r.name)
	}
	return names
}

// indexNameList returns every modelled index name — user indexes and the
// backing indexes implied by UNIQUE constraints — sorted, the enumeration the
// `CALL db.indexes() YIELD … WHERE …` probe filters (rmp #2462).
func (m *SchemaModel) indexNameList() []string {
	enum := m.indexEnumeration()
	names := make([]string, 0, len(enum))
	for _, r := range enum {
		names = append(names, r.name)
	}
	return names
}

// joinRows flattens rows into sorted "a | b | c" lines for order-insensitive
// set comparison and readable mismatch reports. (Both surfaces are name-sorted
// already; sorting again costs nothing and keeps the comparison a pure
// row-SET equality.)
func joinRows(rows [][]string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r, " | ")
	}
	sort.Strings(out)
	return out
}

// rowSetDiff compares two row sets and, when they differ, renders a compact
// got/want report. Schema row sets are small (a handful of rows), so the full
// dump stays readable.
func rowSetDiff(got, want [][]string) (string, bool) {
	g, w := joinRows(got), joinRows(want)
	if len(g) == len(w) {
		equal := true
		for i := range g {
			if g[i] != w[i] {
				equal = false
				break
			}
		}
		if equal {
			return "", false
		}
	}
	return fmt.Sprintf("engine rows:\n  %s\nmodel rows:\n  %s",
		strings.Join(g, "\n  "), strings.Join(w, "\n  ")), true
}

// Introspection queries. SHOW draws from the same enumerations the db.*
// procedures use (cypher/show.go), but the oracle still queries BOTH surfaces
// independently: each is held to the model, and the pair is held to each
// other, so a future divergence between the statement path and the procedure
// path cannot hide.
const (
	queryShowIndexes      = "SHOW INDEXES"
	queryShowConstraints  = "SHOW CONSTRAINTS"
	queryDBIndexes        = "CALL db.indexes() YIELD name, type RETURN name, type"
	queryDBConstraints    = "CALL db.constraints() YIELD name, type, label, property RETURN name, type, label, property"
	queryShowConstraintsY = "SHOW CONSTRAINTS YIELD name, type WHERE type = 'UNIQUE' RETURN name"
	queryShowIndexesYield = "SHOW INDEXES YIELD name, type AS kind WHERE kind = 'btree' RETURN name"
	querySchemaViz        = "CALL db.schema.visualization()"
)

// CheckSchemaIntrospection asserts the engine's schema-introspection surfaces
// against the harness's DDL model:
//
//   - SHOW INDEXES and SHOW CONSTRAINTS row sets equal the model's expected
//     rows (full column shape);
//   - db.indexes() and db.constraints() row sets equal the model too, and the
//     (name, type) enumeration agrees between SHOW and the procedures — two
//     independent surfaces must match each other and the model;
//   - one SHOW ... YIELD ... WHERE ... RETURN projection per statement kind
//     reproduces the model-side filter (#2044), one of them through a YIELD
//     alias;
//   - db.schema.visualization() executes and drains without error.
//
// A model mismatch is a [ViolationOracleDeviation]; a disagreement between the
// two engine surfaces is a [ViolationGraphIntegrity] (the engine is internally
// inconsistent). A clean pass returns nil.
func CheckSchemaIntrospection(tick int64, model *SchemaModel, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	add := func(kind ViolationKind, op, msg string) {
		vs = append(vs, Violation{Kind: kind, Tick: tick, Op: op, Message: msg})
	}
	fetch := func(op, query string, ncols int) ([][]string, bool) {
		rows, err := engine.queryRowStrings(ctx, query, ncols)
		if err != nil {
			add(ViolationGraphIntegrity, op, fmt.Sprintf("%s failed: %v", query, err))
			return nil, false
		}
		return rows, true
	}

	showIdx, okShowIdx := fetch("schema introspection (SHOW INDEXES)", queryShowIndexes, 6)
	if okShowIdx {
		if diff, bad := rowSetDiff(showIdx, model.expectedShowIndexRows()); bad {
			add(ViolationOracleDeviation, "schema introspection (SHOW INDEXES)",
				"SHOW INDEXES diverges from the harness DDL model:\n"+diff)
		}
	}
	dbIdx, okDBIdx := fetch("schema introspection (db.indexes)", queryDBIndexes, 2)
	if okDBIdx {
		if diff, bad := rowSetDiff(dbIdx, model.expectedDBIndexRows()); bad {
			add(ViolationOracleDeviation, "schema introspection (db.indexes)",
				"db.indexes() diverges from the harness DDL model:\n"+diff)
		}
	}
	if okShowIdx && okDBIdx {
		// Cross-surface agreement on the shared (name, type) enumeration:
		// SHOW INDEXES columns are [name, state, type, ...], db.indexes()
		// columns are [name, type].
		showPairs := make([][]string, len(showIdx))
		for i, r := range showIdx {
			showPairs[i] = []string{r[0], r[2]}
		}
		if diff, bad := rowSetDiff(showPairs, dbIdx); bad {
			add(ViolationGraphIntegrity, "schema introspection (SHOW vs db.indexes)",
				"SHOW INDEXES and db.indexes() disagree on the (name, type) enumeration:\n"+diff)
		}
	}

	showCon, okShowCon := fetch("schema introspection (SHOW CONSTRAINTS)", queryShowConstraints, 5)
	if okShowCon {
		if diff, bad := rowSetDiff(showCon, model.expectedShowConstraintRows()); bad {
			add(ViolationOracleDeviation, "schema introspection (SHOW CONSTRAINTS)",
				"SHOW CONSTRAINTS diverges from the harness DDL model:\n"+diff)
		}
	}
	dbCon, okDBCon := fetch("schema introspection (db.constraints)", queryDBConstraints, 4)
	if okDBCon {
		if diff, bad := rowSetDiff(dbCon, model.expectedDBConstraintRows()); bad {
			add(ViolationOracleDeviation, "schema introspection (db.constraints)",
				"db.constraints() diverges from the harness DDL model:\n"+diff)
		}
	}
	if okShowCon && okDBCon {
		showPairs := make([][]string, len(showCon))
		for i, r := range showCon {
			showPairs[i] = []string{r[0], r[1]}
		}
		dbPairs := make([][]string, len(dbCon))
		for i, r := range dbCon {
			dbPairs[i] = []string{r[0], r[1]}
		}
		if diff, bad := rowSetDiff(showPairs, dbPairs); bad {
			add(ViolationGraphIntegrity, "schema introspection (SHOW vs db.constraints)",
				"SHOW CONSTRAINTS and db.constraints() disagree on the (name, type) enumeration:\n"+diff)
		}
	}

	// YIELD / WHERE / RETURN projections (#2044): the filtered single-column
	// result must reproduce the model-side filter.
	if rows, ok := fetch("schema introspection (SHOW CONSTRAINTS YIELD)", queryShowConstraintsY, 1); ok {
		want := make([][]string, 0, 2)
		for _, name := range model.uniqueConstraintNames() {
			want = append(want, []string{canonicalValueString(name)})
		}
		if diff, bad := rowSetDiff(rows, want); bad {
			add(ViolationOracleDeviation, "schema introspection (SHOW CONSTRAINTS YIELD)",
				"SHOW CONSTRAINTS YIELD/WHERE/RETURN projection diverges from the model:\n"+diff)
		}
	}
	if rows, ok := fetch("schema introspection (SHOW INDEXES YIELD alias)", queryShowIndexesYield, 1); ok {
		want := make([][]string, 0, 2)
		for _, name := range model.btreeIndexNames() {
			want = append(want, []string{canonicalValueString(name)})
		}
		if diff, bad := rowSetDiff(rows, want); bad {
			add(ViolationOracleDeviation, "schema introspection (SHOW INDEXES YIELD alias)",
				"SHOW INDEXES YIELD-alias/WHERE/RETURN projection diverges from the model:\n"+diff)
		}
	}

	// The PROCEDURE form of the same projection (rmp #2462). The statement forms
	// above cover `SHOW … YIELD … WHERE`; this covers `CALL … YIELD … WHERE`,
	// which is a distinct code path (cypher/ir/translator.go lifts the predicate
	// as a Selection over the ProcedureCall) and the one that silently dropped
	// its WHERE until #1966 was fixed. Both surfaces are held to the same model.
	vs = append(vs, checkCallYieldWhere(ctx, tick, model, engine)...)

	// db.schema.visualization() must execute and drain cleanly under
	// simulation. Its row set is deliberately not pinned: the procedure
	// currently yields no rows, and this probe holds only the execute/drain
	// contract, not the (evolvable) payload shape.
	if res, err := engine.Run(ctx, querySchemaViz, nil); err != nil {
		add(ViolationGraphIntegrity, "schema introspection (db.schema.visualization)",
			fmt.Sprintf("CALL db.schema.visualization() failed: %v", err))
	} else {
		for res.Next() { //nolint:revive // draining is the point
		}
		if derr := res.Err(); derr != nil {
			add(ViolationGraphIntegrity, "schema introspection (db.schema.visualization)",
				fmt.Sprintf("CALL db.schema.visualization() drain failed: %v", derr))
		}
		_ = res.Close()
	}

	return vs
}
