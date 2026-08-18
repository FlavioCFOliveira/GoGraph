package sim

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
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
	}
	return out, nil
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
// single connection, returning the per-round outcomes. It stops early on ctx
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
		out, err := a.Run(c, fam, modern)
		if err != nil {
			return outcomes, err
		}
		outcomes = append(outcomes, out)
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
