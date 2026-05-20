package tck

// DDLScenario represents one engine-level integration scenario for
// DDL/procedure/parameter testing.
type DDLScenario struct {
	// Name is a short identifier for the scenario, used as the sub-test name.
	Name string
	// Query is the Cypher statement to execute.
	Query string
	// WantErr reports whether the scenario expects an error from the engine.
	WantErr bool
	// ErrContains is a substring expected in the error message when WantErr is
	// true. An empty string means any non-nil error satisfies the expectation.
	ErrContains string
}

// DDLScenarios returns the set of DDL/procedure/parameter integration
// scenarios. These test the execution engine directly, not just parsing.
// They cover extensions added in Sprint 29 (index DDL, constraint DDL,
// parameters, and built-in procedure calls).
//
// Notes on supported DDL syntax:
//
//   - CREATE INDEX: the engine's DDL parser requires the optional IF NOT EXISTS
//     clause to appear before the index name, i.e.:
//     CREATE INDEX [IF NOT EXISTS] [name] FOR (n:Label) ON (n.prop)
//
//   - CREATE CONSTRAINT: the engine uses the pre-4.x ASSERT syntax, i.e.:
//     CREATE CONSTRAINT [name] ON (n:Label) ASSERT n.prop IS UNIQUE
//
//   - Parameters in standalone RETURN require a driving clause. Use
//     MATCH (n) RETURN $param to ensure the param is resolved at execution time.
func DDLScenarios() []DDLScenario {
	return []DDLScenario{
		// ── Index DDL ────────────────────────────────────────────────────────
		{
			Name:  "create_hash_index",
			Query: "CREATE INDEX my_idx FOR (n:Person) ON (n.name)",
		},
		{
			Name:  "create_hash_index_if_not_exists",
			Query: "CREATE INDEX IF NOT EXISTS my_idx FOR (n:Person) ON (n.name)",
		},
		{
			Name:  "drop_index",
			Query: "DROP INDEX my_idx IF EXISTS",
		},

		// ── Constraint DDL ───────────────────────────────────────────────────
		{
			// The engine's DDL parser uses the pre-4.x ON … ASSERT … IS UNIQUE syntax.
			Name:  "create_unique_constraint",
			Query: "CREATE CONSTRAINT unique_name ON (n:Person) ASSERT n.name IS UNIQUE",
		},
		{
			Name:  "drop_constraint",
			Query: "DROP CONSTRAINT unique_name IF EXISTS",
		},

		// ── Parameters ───────────────────────────────────────────────────────
		{
			// Standalone RETURN $param requires a driving clause; MATCH provides
			// one record on which the param binding is evaluated.
			Name:  "param_string",
			Query: "MATCH (n) RETURN $name AS name",
		},
		{
			Name:  "param_integer",
			Query: "MATCH (n) RETURN $age AS age",
		},

		// ── CALL procedures ──────────────────────────────────────────────────
		{
			Name:  "call_db_indexes",
			Query: "CALL db.indexes() YIELD name, type RETURN name, type",
		},
		{
			Name:  "call_db_labels",
			Query: "CALL db.labels() YIELD label RETURN label",
		},
		{
			Name:  "call_db_constraints",
			Query: "CALL db.constraints() YIELD name, type RETURN name, type",
		},
	}
}
