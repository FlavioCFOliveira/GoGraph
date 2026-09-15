package sim

// ddl_counters_oracle.go — per-statement DDL counters oracle (rmp #2822).
//
// # The gap this closes
//
// [CheckOpCounters] (counters_oracle.go, rmp #2448) adjudicates the write-effect
// counters of every DATA statement the tick loop executes. DDL statements took a
// different route: [Simulator.engineRunDDL] ran them through the engine, drained
// the *Result for its error, and DISCARDED it. No DDL statement's counters ever
// reached an oracle, so all four schema effects — indexes-added, indexes-removed,
// constraints-added, constraints-removed — were outside the simulator's reach
// entirely.
//
// That was measured, not inferred: rmp #2818 (a `DROP INDEX <missing> IF EXISTS`
// that reported indexesRemoved: 1 across an index listing identical before and
// after) carried an acceptance criterion reading "the counters oracle agrees with
// the new behaviour". `./internal/sim/...` was green — but green because the
// oracle had never observed a single indexesRemoved value, before the fix or
// after. A defect of that class could not fail a simulation.
//
// # What the expectation is derived from
//
// NOT from the statement text, and not from the counters themselves. The engine's
// own schema registries are read BEFORE and AFTER the statement
// ([cypher.Engine.ListIndexes] and [cypher.Engine.Constraints], surfaced through
// [EngineAdapter]), and the counters must equal the difference:
//
//	indexesAdded       == |user indexes gained|
//	indexesRemoved     == |user indexes lost|
//	constraintsAdded   == |constraints gained|
//	constraintsRemoved == |constraints lost|
//
// and every DATA counter must be zero, because a DDL statement writes no data.
// A name set is used rather than a count so a statement that both adds and
// removes cannot cancel out.
//
// This derivation covers every DDL form the simulator issues — CREATE and DROP,
// INDEX and CONSTRAINT, with and without IF [NOT] EXISTS, and the SHOW statements
// that change nothing — with no parsing at all, and it is exactly the oracle
// #2818 needed: an absorbed IF EXISTS loses no index, so it must report no
// removal.
//
// # The internal index names, and why they are filtered out
//
// [cypher.Engine.ListIndexes] reports the live index.Manager, which holds two
// kinds of index a user never named and the counters never report:
//
//   - the UNIQUE backing index "__uniq__<Label>.<prop>", created and dropped with
//     its constraint (cypher/exec.uniqueIndexName), and
//   - the numeric companion "<label>_<prop>_btree_num" that every user btree and
//     hash index owns (cypher: numericCompanionSuffix, #1652 and #2226), created
//     and dropped with the user index that needs it.
//
// Counting either would attribute an index effect to a CONSTRAINT statement, or
// two index effects to one CREATE INDEX. Both are recognised by name, exactly as
// [SchemaModel] already derives the backing-index row for the introspection
// oracle. A user index deliberately named with the "_btree_num" suffix would be
// filtered too; the simulator issues none, and such a name is reserved.
//
// # Cost
//
// Two registry reads per DDL statement, each a slice of a handful of names. It
// issues no engine query, so it adds no statement to any metric the simulator
// asserts on and cannot perturb a deterministic run.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// uniqueBackingIndexPrefix is the reserved name prefix of the hash index that
// backs a UNIQUE constraint ("__uniq__<Label>.<prop>", cypher/exec.uniqueIndexName).
const uniqueBackingIndexPrefix = "__uniq__"

// numericCompanionIndexSuffix is the reserved name suffix of the internal numeric
// companion index the engine maintains alongside a user index over a numeric
// property (cypher: numericCompanionSuffix, #1652).
const numericCompanionIndexSuffix = "_btree_num"

// ddlSchemaNames is the engine's own schema name sets at one instant: the USER
// index names (internal companions and UNIQUE backing indexes excluded) and the
// constraint names. It is the surface [CheckDDLCounters] derives its expectation
// from, and it is independent of the counters being checked.
type ddlSchemaNames struct {
	indexes     map[string]struct{}
	constraints map[string]struct{}
}

// readDDLSchemaNames snapshots the engine's user index names and constraint
// names.
func readDDLSchemaNames(engine *EngineAdapter) ddlSchemaNames {
	s := ddlSchemaNames{
		indexes:     make(map[string]struct{}),
		constraints: make(map[string]struct{}),
	}
	for _, name := range engine.ListIndexes() {
		if isInternalIndexName(name) {
			continue
		}
		s.indexes[name] = struct{}{}
	}
	for _, name := range engine.ConstraintNames() {
		s.constraints[name] = struct{}{}
	}
	return s
}

// isInternalIndexName reports whether name is an index the engine maintains on a
// user index's or constraint's behalf, which no counter ever reports.
func isInternalIndexName(name string) bool {
	return strings.HasPrefix(name, uniqueBackingIndexPrefix) ||
		strings.HasSuffix(name, numericCompanionIndexSuffix)
}

// gainedLost returns the names present in after and not before, and vice versa,
// each sorted so a violation message is deterministic.
func gainedLost(before, after map[string]struct{}) (gained, lost []string) {
	for name := range after {
		if _, ok := before[name]; !ok {
			gained = append(gained, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			lost = append(lost, name)
		}
	}
	sort.Strings(gained)
	sort.Strings(lost)
	return gained, lost
}

// CheckDDLCounters compares the engine-reported schema-effect counters of one
// executed DDL statement against the effect its own registries show it applied,
// and returns a [ViolationOracleDeviation] per disagreement.
//
// before and after are [readDDLSchemaNames] snapshots taken immediately either
// side of the statement. got is the statement's counters, read from the drained
// [cypher.Result]; nil is the engine's report for a statement that recorded no
// schema effect and is treated as an all-zero effect set here, because whether a
// no-op DDL reports nil or a zero counter set is a distinction the module has not
// pinned — what IS pinned, and what this checks, is the VALUE of each effect.
//
// The rules:
//
//   - each of the four schema counters must equal the corresponding name-set
//     difference: an absorbed IF EXISTS loses nothing and must report nothing,
//     and an applied DROP loses one name and must report one removal;
//   - every data counter must be zero, so a schema statement cannot leak a node,
//     relationship, property or label effect;
//   - a statement that changed the schema must report NON-nil counters, or the
//     effect went unreported altogether.
//
// query is carried only for the message; nothing is parsed out of it.
func CheckDDLCounters(tick int64, query string, before, after ddlSchemaNames, got *exec.QueryCounters) []Violation {
	idxGained, idxLost := gainedLost(before.indexes, after.indexes)
	conGained, conLost := gainedLost(before.constraints, after.constraints)

	want := exec.QueryCounters{
		IndexesAdded:       int64(len(idxGained)),
		IndexesRemoved:     int64(len(idxLost)),
		ConstraintsAdded:   int64(len(conGained)),
		ConstraintsRemoved: int64(len(conLost)),
	}
	if got == nil {
		if !want.ContainsUpdates() {
			return nil
		}
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "DDL counters",
			Message: fmt.Sprintf("DDL %q reported nil counters but changed the schema: %s",
				query, renderSchemaDelta(idxGained, idxLost, conGained, conLost)),
		}}
	}
	diff := diffCounters(&want, got)
	if diff == "" {
		return nil
	}
	return []Violation{{
		Kind: ViolationOracleDeviation, Tick: tick, Op: "DDL counters",
		Message: fmt.Sprintf("DDL %q counters disagree with the schema effect the engine's own registries show: %s (registries: %s)",
			query, diff, renderSchemaDelta(idxGained, idxLost, conGained, conLost)),
	}}
}

// renderSchemaDelta names what the registries gained and lost, so a violation
// says which index or constraint the engine's report failed to describe.
func renderSchemaDelta(idxGained, idxLost, conGained, conLost []string) string {
	parts := make([]string, 0, 4)
	for _, p := range []struct {
		label string
		names []string
	}{
		{"indexes gained", idxGained}, {"indexes lost", idxLost},
		{"constraints gained", conGained}, {"constraints lost", conLost},
	} {
		if len(p.names) > 0 {
			parts = append(parts, p.label+" "+strings.Join(p.names, ","))
		}
	}
	if len(parts) == 0 {
		return "no schema change"
	}
	return strings.Join(parts, "; ")
}

// runDDLChecked runs one DDL statement through engine, drains it, and adjudicates
// its counters with [CheckDDLCounters]. It is the single route every DDL statement
// the simulator issues takes ([Simulator.engineRunDDL] and [engineRunDDLOn] both
// delegate here), so no DDL escapes the oracle.
//
// A statement that ERRORS returns its error and no violations: the counters of a
// failed statement are not this oracle's subject, and the schema snapshots either
// side of a failure describe a rollback, not an applied effect.
//
// tick labels any violation with the simulation tick the statement ran at; a call
// site outside the tick loop (scenario setup, a crash scenario driving its own
// engine) passes 0.
func runDDLChecked(ctx context.Context, engine *EngineAdapter, query string, tick int64) ([]Violation, error) {
	before := readDDLSchemaNames(engine)
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	for res.Next() { // draining is the point: DDL emits no rows but must complete
	}
	drainErr := res.Err()
	// Through the same optional facet [CheckOpCounters] reads the data statements'
	// counters through, so both oracles observe the identical surface.
	var counters *exec.QueryCounters
	if cr, ok := res.(counterReporter); ok {
		counters = cr.Counters()
	}
	if cerr := res.Close(); cerr != nil && drainErr == nil {
		drainErr = cerr
	}
	if drainErr != nil {
		return nil, drainErr
	}
	after := readDDLSchemaNames(engine)
	return CheckDDLCounters(tick, query, before, after, counters), nil
}

// ddlCountersError renders oracle violations as an error, for the DDL call sites
// that report a failure as an error rather than as a [SimReport].
func ddlCountersError(query string, violations []Violation) error {
	msgs := make([]string, 0, len(violations))
	for _, v := range violations {
		msgs = append(msgs, v.Message)
	}
	return fmt.Errorf("sim: DDL counters oracle on %q: %s", query, strings.Join(msgs, "; "))
}
