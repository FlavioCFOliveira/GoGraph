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
