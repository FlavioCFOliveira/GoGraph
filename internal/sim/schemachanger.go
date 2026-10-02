package sim

import (
	"context"
	"fmt"
	"sort"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// SchemaChangeFamily identifies one DDL operation the [SchemaChanger] issues.
type SchemaChangeFamily int

// Schema-change families.
const (
	// SchemaCreateIndex creates a hash index on (:Person).name. It is an
	// idempotent op family: IF NOT EXISTS makes a re-create a clean no-op, so
	// its contract is SUCCESS, never a tolerated failure (rmp #2455).
	SchemaCreateIndex SchemaChangeFamily = iota
	// SchemaDropIndex drops the (:Person).name index (idempotent via IF EXISTS).
	SchemaDropIndex
	// SchemaCreateConstraint creates a UNIQUE constraint on (:Account).email,
	// alternating (seed-chosen) between the legacy ON ... ASSERT grammar and
	// the modern FOR ... REQUIRE grammar — both parsed by
	// cypher/ir/ddl_parser.go into the same IR. Like SchemaCreateIndex it is
	// idempotent via IF NOT EXISTS, so its contract is SUCCESS: a re-create is
	// absorbed as a no-op, never a tolerated "already exists" failure.
	SchemaCreateConstraint
	// SchemaDropConstraint drops the (:Account).email UNIQUE constraint
	// (idempotent via IF EXISTS).
	SchemaDropConstraint
)

// schemaChangeFamilyCount is the number of DDL families; the unit test asserts
// every family is reachable.
const schemaChangeFamilyCount = 4

// Index/constraint names the SchemaChanger churns. Fixed names let the
// create/drop families race on the same object, which is the contention the
// actor is built to exercise.
const (
	schemaIndexName      = "sim_person_name_idx"
	schemaConstraintName = "sim_account_email_uq"
)

// String renders a SchemaChangeFamily for reports.
func (f SchemaChangeFamily) String() string {
	switch f {
	case SchemaCreateIndex:
		return "CreateIndex"
	case SchemaDropIndex:
		return "DropIndex"
	case SchemaCreateConstraint:
		return "CreateConstraint"
	case SchemaDropConstraint:
		return "DropConstraint"
	default:
		return fmt.Sprintf("SchemaChangeFamily(%d)", int(f))
	}
}

// SchemaChangeOutcome records one DDL attempt. A DDL either succeeds or returns
// a typed FAILURE (e.g. a transient conflict under contention); both are
// acceptable. A panic, leak, or torn index/lost constraint is a violation,
// checked structurally after the run rather than per-attempt.
type SchemaChangeOutcome struct {
	// Counters is the statement's effect report as the terminal SUCCESS carried
	// it on the wire, decoded by [wireDDLCounters]: nil when the SUCCESS carried
	// no `stats` map (the server's report of a statement that changed nothing).
	// It is set only when Succeeded.
	Counters   *exec.QueryCounters
	FailureMsg string
	Family     SchemaChangeFamily
	Succeeded  bool
	Failed     bool
}

// Acceptable reports whether the DDL completed cleanly (success or typed
// FAILURE) without wedging the connection.
func (o SchemaChangeOutcome) Acceptable() bool { return o.Succeeded || o.Failed }

// MeetsContract reports whether the outcome satisfies the family's contract.
// The idempotent IF NOT EXISTS create families (SchemaCreateIndex,
// SchemaCreateConstraint) must SUCCEED — a re-create is a clean no-op by the
// engine's IF NOT EXISTS contract (cypher: TestCreateConstraint_IfNotExists_
// Idempotent), so a typed FAILURE there is a contract breach, not an
// acceptable bounded outcome (rmp #2455). The drop families keep the tolerant
// contract (success or typed FAILURE under contention).
func (o SchemaChangeOutcome) MeetsContract() bool {
	switch o.Family {
	case SchemaCreateIndex, SchemaCreateConstraint:
		return o.Succeeded
	default:
		return o.Acceptable()
	}
}

// SchemaChanger issues DDL (CREATE/DROP INDEX, CREATE/DROP CONSTRAINT) over the
// real Bolt wire, concurrently with honest writers and readers, to exercise
// index and constraint maintenance under races. Every statement is idempotent
// (IF [NOT] EXISTS) so a create/drop race never produces a spurious error; the
// invariants the harness asserts after the churn are that the index stays
// consistent with its base data and that a UNIQUE constraint, when present,
// stays enforced.
//
// SchemaChanger runs in the CONCURRENT mode (one goroutine), so its DDL
// interleaves non-deterministically with concurrent writes; correctness is the
// structural invariants at quiescence, not bit-replay.
//
// # Concurrency contract
//
// SchemaChanger is stateless; each [SchemaChanger.Run] call drives one connection
// it owns and may run on its own goroutine.
type SchemaChanger struct{}

// Name returns the actor's identifier.
func (SchemaChanger) Name() string { return "SchemaChanger" }

// PickFamily chooses a DDL family from the seed (one int draw).
func (SchemaChanger) PickFamily(seed *Seed) SchemaChangeFamily {
	return SchemaChangeFamily(seed.IntN(schemaChangeFamilyCount))
}

// PickModernForm chooses (one int draw) whether the next constraint DDL uses
// the modern FOR ... REQUIRE grammar (true) or the legacy ON ... ASSERT
// grammar (false), so the churn exercises both parse paths under the same
// seed-deterministic stream (rmp #2455). Only [SchemaCreateConstraint]
// consults the flag; the other families have a single grammar.
func (SchemaChanger) PickModernForm(seed *Seed) bool { return seed.IntN(2) == 1 }

// Run issues one DDL statement of the given family over c and returns the
// classified outcome. modern selects the constraint grammar for
// [SchemaCreateConstraint] (see [SchemaChanger.PickModernForm]); the other
// families ignore it. The connection must already be Connected.
func (a SchemaChanger) Run(c *WireClient, family SchemaChangeFamily, modern bool) (SchemaChangeOutcome, error) {
	out := SchemaChangeOutcome{Family: family}
	resp, err := c.Run(a.statement(family, modern), nil)
	if err != nil {
		return out, fmt.Errorf("sim: schema-change RUN(%s): %w", family, err)
	}
	if f, ok := resp.(*proto.Failure); ok {
		out.Failed = true
		out.FailureMsg = f.Code + ": " + f.Message
		return out, nil
	}
	// DDL statements produce no rows; drain to the terminal SUCCESS/FAILURE.
	_, term, err := c.PullAll()
	if err != nil {
		return out, fmt.Errorf("sim: schema-change PULL(%s): %w", family, err)
	}
	switch m := term.(type) {
	case *proto.Failure:
		out.Failed = true
		out.FailureMsg = m.Code + ": " + m.Message
	default:
		out.Succeeded = true
		counters, err := wireDDLCounters(term)
		if err != nil {
			return out, fmt.Errorf("sim: schema-change stats(%s): %w", family, err)
		}
		out.Counters = counters
	}
	return out, nil
}

// RunChecked is [SchemaChanger.Run] with the statement's wire-reported counters
// ADJUDICATED (rmp #2829): it snapshots srv's engine schema registries before
// and after the statement and holds the counters the terminal SUCCESS carried
// to their difference with [CheckDDLCounters] — the same expectation the
// in-process DDL route ([runDDLChecked], rmp #2822) applies. A counter that
// lies anywhere between the engine's operator and the Bolt `stats` encoding is
// returned as a violation. A typed FAILURE is not adjudicated, as on the
// in-process route: a failed statement applied no effect to report.
//
// The registry difference is attributable to this statement only while no other
// DDL runs against srv's engine; data writes never change the registries. Every
// caller in this package issues DDL from a single connection.
func (a SchemaChanger) RunChecked(srv *SimServer, c *WireClient, family SchemaChangeFamily, modern bool) (SchemaChangeOutcome, []Violation, error) {
	engine := NewEngineAdapter(srv.eng)
	before := readDDLSchemaNames(engine)
	out, err := a.Run(c, family, modern)
	if err != nil || !out.Succeeded {
		return out, nil, err
	}
	after := readDDLSchemaNames(engine)
	return out, CheckDDLCounters(0, a.statement(family, modern), before, after, out.Counters), nil
}

// wireDDLCounters decodes the `stats` map of a terminal SUCCESS into the
// counters the server encoded it from (bolt/server/result_stats.go resultStats).
// A SUCCESS without a `stats` map decodes to nil, which [CheckDDLCounters]
// treats as an all-zero report.
//
// The decoding is strict, so the wire report cannot be misread as a clean one:
// every key must be one the encoder emits, every counter must be an integer, and
// `contains-updates` must be the boolean true and present exactly when some
// counter is non-zero. Bolt's single properties-set is decoded into
// PropertiesSet; the encoder sums openCypher's +properties and -properties into
// it, and a DDL statement must report zero for both.
func wireDDLCounters(term any) (*exec.QueryCounters, error) {
	s, ok := term.(*proto.Success)
	if !ok {
		return nil, fmt.Errorf("terminal %T is not a SUCCESS", term)
	}
	rawStats, present := s.Metadata["stats"]
	if !present {
		return nil, nil
	}
	stats, ok := rawStats.(map[string]packstream.Value)
	if !ok {
		return nil, fmt.Errorf("stats is %T, want a map", rawStats)
	}
	c := &exec.QueryCounters{}
	fields := map[string]*int64{
		"nodes-created":         &c.NodesCreated,
		"nodes-deleted":         &c.NodesDeleted,
		"relationships-created": &c.RelationshipsCreated,
		"relationships-deleted": &c.RelationshipsDeleted,
		"properties-set":        &c.PropertiesSet,
		"labels-added":          &c.LabelsAdded,
		"labels-removed":        &c.LabelsRemoved,
		"indexes-added":         &c.IndexesAdded,
		"indexes-removed":       &c.IndexesRemoved,
		"constraints-added":     &c.ConstraintsAdded,
		"constraints-removed":   &c.ConstraintsRemoved,
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a deterministic first error
	containsUpdates := false
	for _, k := range keys {
		v := stats[k]
		if k == "contains-updates" {
			b, ok := v.(bool)
			if !ok || !b {
				return nil, fmt.Errorf("stats contains-updates is %T %v, want the boolean true", v, v)
			}
			containsUpdates = true
			continue
		}
		dst, known := fields[k]
		if !known {
			return nil, fmt.Errorf("stats carries unknown key %q", k)
		}
		n, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("stats %s is %T, want an integer", k, v)
		}
		*dst = n
	}
	if containsUpdates != c.ContainsUpdates() {
		return nil, fmt.Errorf("stats contains-updates=%t disagrees with its counters %+v", containsUpdates, *c)
	}
	return c, nil
}

// statement returns the idempotent DDL Cypher for a family. For
// [SchemaCreateConstraint], modern selects the FOR ... REQUIRE grammar over
// the legacy ON ... ASSERT grammar; both carry IF NOT EXISTS (the engine
// parses it in either position — cypher/ir/ddl_parser.go parseCreateConstraint
// accepts it before FOR/ON and after the assertion), so a re-create is a clean
// idempotent SUCCESS in both grammars.
func (SchemaChanger) statement(family SchemaChangeFamily, modern bool) string {
	switch family {
	case SchemaCreateIndex:
		// IF NOT EXISTS makes a re-create race a clean no-op rather than an error.
		return fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s FOR (n:Person) ON (n.name)", schemaIndexName)
	case SchemaDropIndex:
		return fmt.Sprintf("DROP INDEX %s IF EXISTS", schemaIndexName)
	case SchemaCreateConstraint:
		if modern {
			return fmt.Sprintf("CREATE CONSTRAINT %s IF NOT EXISTS FOR (a:Account) REQUIRE a.email IS UNIQUE", schemaConstraintName)
		}
		return fmt.Sprintf("CREATE CONSTRAINT %s ON (a:Account) ASSERT a.email IS UNIQUE IF NOT EXISTS", schemaConstraintName)
	case SchemaDropConstraint:
		return fmt.Sprintf("DROP CONSTRAINT %s IF EXISTS", schemaConstraintName)
	default:
		return "RETURN 1"
	}
}

// RunSchemaChurn drives a SchemaChanger through rounds DDL statements over a
// single connection, returning the per-round outcomes. Every statement goes
// through [SchemaChanger.RunChecked], so a wire-reported counter that disagrees
// with the engine's registries ends the churn with an error (rmp #2829); the
// random family draw makes absorbed forms (a drop of an absent object, a
// re-create of a present one) part of the stream. It stops early on ctx
// cancellation. It is the unit the concurrent integration test runs alongside
// honest writers.
func RunSchemaChurn(ctx context.Context, srv *SimServer, seed *Seed, rounds int) ([]SchemaChangeOutcome, error) {
	c, err := srv.Dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("sim: schema-churn connect: %w", err)
	}
	a := SchemaChanger{}
	outcomes := make([]SchemaChangeOutcome, 0, rounds)
	for i := 0; i < rounds; i++ {
		if ctx.Err() != nil {
			break
		}
		fam := a.PickFamily(seed)
		modern := false
		if fam == SchemaCreateConstraint {
			// Seed-alternate the legacy and modern constraint grammars so both
			// parse paths run under churn (rmp #2455). The draw is taken only for
			// the constraint family, keeping the other families' streams unchanged.
			modern = a.PickModernForm(seed)
		}
		out, violations, err := a.RunChecked(srv, c, fam, modern)
		if err != nil {
			return outcomes, err
		}
		outcomes = append(outcomes, out)
		if len(violations) > 0 {
			// The wire-reported counters disagree with the schema effect the
			// engine's own registries show (rmp #2829).
			return outcomes, ddlCountersError(a.statement(fam, modern), violations)
		}
		if out.Failed {
			// A typed DDL FAILURE (e.g. a re-create conflict) moves the Bolt session
			// to FAILED, in which every further RUN is illegal until a RESET. Reset
			// to recover the session so the churn continues on the same connection.
			if _, err := c.Reset(); err != nil {
				return outcomes, fmt.Errorf("sim: schema-churn reset after failure: %w", err)
			}
		}
	}
	return outcomes, nil
}
