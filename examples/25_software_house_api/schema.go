package main

// Schema constants for the multi-layer software-house graph. The full
// model is specified in SPEC.md. Every node carries exactly one layer
// label and one type label; every relationship has exactly one type with
// a fixed direction.

// Layer labels — the multilayer "layer" each node belongs to (SPEC §2).
const (
	layerCode   = "Code"
	layerWork   = "Work"
	layerPeople = "People"
)

// Type labels — the heterogeneous-information-network object type of each
// node (SPEC §3).
const (
	typeRepository    = "Repository"
	typeModule        = "Module"
	typeComponent     = "Component"
	typeTask          = "Task"
	typeSprint        = "Sprint"
	typeWorkflowState = "WorkflowState"
	typeDeveloper     = "Developer"
	typeTeam          = "Team"
)

// Relationship types with their fixed src->dst direction (SPEC §4).
const (
	relContains   = "CONTAINS"    // Repository->Module, Module->Component
	relDependsOn  = "DEPENDS_ON"  // Component->Component (dependent->dependency)
	relSubtaskOf  = "SUBTASK_OF"  // Task->Task
	relNext       = "NEXT"        // Task->Task (sequencing)
	relBlocks     = "BLOCKS"      // Task->Task (blocker->blocked)
	relHasState   = "HAS_STATE"   // Task->WorkflowState
	relInSprint   = "IN_SPRINT"   // Task->Sprint
	relMemberOf   = "MEMBER_OF"   // Developer->Team
	relAssignedTo = "ASSIGNED_TO" // Developer->Task  (inter-layer coupling)
	relTouches    = "TOUCHES"     // Task->Component   (inter-layer coupling)
)

// nodeTypeLabels and relTypes drive the /stats counters and the tests.
// Listing them once keeps the stats contract and the seed in lock-step.
var nodeTypeLabels = []string{
	typeRepository, typeModule, typeComponent,
	typeTask, typeSprint, typeWorkflowState,
	typeDeveloper, typeTeam,
}

var relTypes = []string{
	relContains, relDependsOn, relSubtaskOf, relNext, relBlocks,
	relHasState, relInSprint, relMemberOf, relAssignedTo, relTouches,
}

// Schema objects declared as Cypher DDL at store initialisation (SPEC §11).
// The graph carries a natural `key` property on every node; these constants
// name the two schema objects the example creates over it:
//
//   - constraintComponentKeyName — a UNIQUE constraint on Component.key. A
//     duplicate component key is a genuine integrity violation (two files
//     claiming the same path), so the engine rejects it at write time. The
//     constraint also installs a backing hash index, so equality lookups on
//     Component.key (catalogue queries Q1, Q6, Q8) plan as a NodeByIndexSeek.
//   - indexDeveloperKeyName — a plain secondary index on Developer.key, which
//     accelerates the Developer.key equality lookups of catalogue query Q4.
//
// A UNIQUE-backing index is reported by db.indexes() under the reserved name
// __uniq__Component.key; that is expected and surfaced faithfully by /schema.
const (
	constraintComponentKeyName = "component_key_unique"
	indexDeveloperKeyName      = "developer_key_idx"
)

// schemaDDL is the ordered list of idempotent DDL statements that declare the
// example's schema. Each uses IF NOT EXISTS so re-opening a populated store is
// a no-op (a no-op IF NOT EXISTS writes no WAL frame, so repeated opens do not
// grow the log). The two statements differ in IF NOT EXISTS placement, matching
// the engine's DDL grammar: after the name for CREATE CONSTRAINT, before the
// name for CREATE INDEX.
//
// These statements are issued through the engine so their backing indexes are
// backfilled from the live graph and self-maintain from the engine write path.
// They must therefore run AFTER the bulk fixture is loaded (see dataStore.seed):
// the fixture is applied through the txn.Store directly, which does not feed the
// engine's index change fan-out, so an index created before the fixture would
// not see the fixture's nodes until a restart re-backfilled it.
var schemaDDL = []string{
	"CREATE CONSTRAINT " + constraintComponentKeyName +
		" IF NOT EXISTS FOR (c:" + typeComponent + ") REQUIRE c.key IS UNIQUE",
	"CREATE INDEX IF NOT EXISTS " + indexDeveloperKeyName +
		" FOR (d:" + typeDeveloper + ") ON (d.key)",
}

// explainDemoQuery is a representative keyed lookup — catalogue query Q4's
// Developer.key filter — whose physical plan the server prints at startup (as
// "# " telemetry) after a scaled seed, to demonstrate that the declared index
// turns the equality lookup into a NodeByIndexSeek rather than a full
// NodeByLabelScan.
const explainDemoQuery = "MATCH (d:" + typeDeveloper + ") WHERE d.key = 'dev:alice' RETURN d.key AS key"
