package parser

// PlanMode records which plan-inspection prefix introduced a parsed statement.
//
// The prefix is part of the grammar (see `script` in
// cypher/parser/grammar/CypherParser.g4), so the parser — not a textual scan
// ahead of it — is what decides whether a statement carries one. The two
// prefixes are syntactically identical and differ only in what the engine is
// then permitted to do:
//
//   - [PlanModeExplain] — plan the statement and execute NOTHING.
//   - [PlanModeProfile] — execute the statement and report what each operator
//     cost.
//
// That distinction is the whole reason both spellings exist, and it is a safety
// property as much as a diagnostic one: a user reaching for EXPLAIN on a
// DETACH DELETE must not lose their graph.
type PlanMode uint8

const (
	// PlanModeNone is an ordinary statement, written with no prefix.
	PlanModeNone PlanMode = iota
	// PlanModeExplain is a statement written with the EXPLAIN prefix.
	PlanModeExplain
	// PlanModeProfile is a statement written with the PROFILE prefix.
	PlanModeProfile
)

// String returns the prefix keyword, or "" for [PlanModeNone].
func (m PlanMode) String() string {
	switch m {
	case PlanModeExplain:
		return "EXPLAIN"
	case PlanModeProfile:
		return "PROFILE"
	default:
		return ""
	}
}
