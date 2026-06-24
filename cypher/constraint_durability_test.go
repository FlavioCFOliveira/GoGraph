package cypher_test

// constraint_durability_test.go — regression coverage for #1316 (durable
// constraints) and #1317 (validate-and-seed on CREATE CONSTRAINT).
//
// #1316: CREATE CONSTRAINT used to register only an in-memory hash index +
// registry entry, rebuilt EMPTY on every engine open; nothing was written to
// the WAL or snapshot and nothing re-registered on recovery. A client got a
// success ack, inserted data, the process was killed and reopened — the
// constraint was gone and duplicates were accepted with no error. The fix
// persists a typed constraint op to the WAL (re-registered by recovery.Open)
// AND carries the constraint set in the snapshot's constraints.bin component,
// so a constraint survives BOTH a plain reopen and a checkpoint + WAL truncate.
//
// #1317: CREATE CONSTRAINT now scans the pre-existing nodes, rejects creation
// over already-duplicated data, and seeds the UNIQUE value-set so a constraint
// added to a non-empty dataset is enforced against pre-existing values.
//
// Layer: short. goleak-clean (engines/graphs/WAL are local and closed).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func cdRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func cdStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

// cdCycle reopens dir, runs each query through the WAL-backed engine, and
// closes the WAL. When snap is true it first writes a full snapshot carrying
// the engine's constraints and truncates the WAL (the checkpoint path);
// otherwise it only fsyncs the WAL (pure WAL-replay recovery on the next open).
// It returns the terminal error of the LAST query (nil on success) so a test
// can assert a duplicate insert is rejected after recovery.
func cdCycle(t *testing.T, dir string, snap bool, queries ...string) error {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, cdRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("wal.Close: %v", cerr)
		}
	}()
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, cdStoreOpts())
	// Re-register the recovered constraints so they are enforced again — the
	// crux of #1316. Using the plain NewEngineWithStore here would leave the
	// registry empty and the test (a duplicate insert) would wrongly succeed.
	eng := cypher.NewEngineWithStoreAndConstraints(store, res.Constraints)

	var lastErr error
	for _, q := range queries {
		lastErr = cdRunOne(t, eng, q)
	}

	if snap {
		cs := csr.BuildFromAdjList(res.Graph.AdjList())
		if werr := snapshot.WriteSnapshotFullWithMapperCodecAndConstraints(
			filepath.Join(dir, "snapshot"), cs, res.Graph, txn.NewStringCodec(),
			eng.ConstraintSpecsForSnapshot(),
		); werr != nil {
			t.Fatalf("WriteSnapshotFullWithMapperCodecAndConstraints: %v", werr)
		}
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("wal.Sync: %v", serr)
	}
	if snap {
		if _, terr := w.Truncate(); terr != nil {
			t.Fatalf("wal.Truncate: %v", terr)
		}
	}
	return lastErr
}

// cdRunOne runs one autocommit query and returns its terminal error (a
// build-time error from RunInTxAny, or the drain/Close error). It never calls
// t.Fatal so callers can inspect the error of a deliberately-violating query.
func cdRunOne(t *testing.T, eng *cypher.Engine, q string) error {
	t.Helper()
	r, err := eng.RunInTxAny(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for r.Next() { //nolint:revive // drain to run the write to completion
	}
	rerr := r.Err()
	if cerr := r.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	return rerr
}

// runConstraintReopen drives the #1316 acceptance criterion: declare a UNIQUE
// constraint, insert one node, close; reopen and assert a duplicate insert is
// rejected. snap selects the checkpoint-then-reopen variant.
func runConstraintReopen(t *testing.T, snap bool) {
	t.Helper()
	dir := t.TempDir()

	// Cycle 1: declare the constraint, insert one node, then close (and
	// optionally checkpoint+truncate).
	if err := cdCycle(t, dir, snap,
		`CREATE CONSTRAINT u_email ON (n:User) ASSERT n.email IS UNIQUE`,
		`CREATE (n:User {email: 'a@example.com'})`,
	); err != nil {
		t.Fatalf("cycle 1 (declare + first insert): %v", err)
	}

	// Cycle 2: reopen. The constraint must have survived recovery, so inserting
	// a duplicate must be rejected with exec.ErrConstraintViolation. Before the
	// #1316 fix the constraint is gone after reopen and this insert succeeds.
	dupErr := cdCycle(t, dir, snap,
		`CREATE (n:User {email: 'a@example.com'})`,
	)
	if dupErr == nil {
		t.Fatalf("duplicate insert after reopen (snap=%v) was accepted; the UNIQUE constraint did not survive recovery", snap)
	}
	if !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("duplicate insert after reopen (snap=%v): got %v, want one wrapping exec.ErrConstraintViolation", snap, dupErr)
	}

	// A genuinely new value must still be accepted after reopen — the constraint
	// is enforced, not a blanket reject.
	if err := cdCycle(t, dir, snap,
		`CREATE (n:User {email: 'b@example.com'})`,
	); err != nil {
		t.Fatalf("distinct insert after reopen (snap=%v) rejected: %v", snap, err)
	}
}

// TestConstraint_SurvivesWALReopen is the #1316 AC: a constraint persisted via
// the WAL op survives a plain reopen (no checkpoint).
func TestConstraint_SurvivesWALReopen(t *testing.T) {
	t.Parallel()
	runConstraintReopen(t, false)
}

// TestConstraint_SurvivesCheckpointReopen is the #1316 checkpoint variant: the
// constraint survives a checkpoint that truncates the WAL prefix which first
// declared it, because the snapshot's constraints.bin carries it forward.
func TestConstraint_SurvivesCheckpointReopen(t *testing.T) {
	t.Parallel()
	runConstraintReopen(t, true)
}

// cdRunMem runs one autocommit query on an in-memory engine and returns its
// terminal error.
func cdRunMem(t *testing.T, eng *cypher.Engine, q string) error {
	t.Helper()
	r, err := eng.RunInTxAny(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for r.Next() { //nolint:revive // drain
	}
	rerr := r.Err()
	if cerr := r.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	return rerr
}

// TestCreateConstraint_InMemoryRejectsPreExisting is the #1317 AC on a
// store-less in-memory engine (no persistence in the loop): CREATE CONSTRAINT
// over already-duplicated data errors; a post-creation duplicate is rejected.
func TestCreateConstraint_InMemoryRejectsPreExisting(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// Two pre-existing duplicates, then CREATE CONSTRAINT must reject.
	if err := cdRunMem(t, eng, `CREATE (n:P {k: 'x'})`); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := cdRunMem(t, eng, `CREATE (n:P {k: 'x'})`); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	createErr := cdRunMem(t, eng, `CREATE CONSTRAINT u_k ON (n:P) ASSERT n.k IS UNIQUE`)
	if createErr == nil || !errors.Is(createErr, exec.ErrConstraintViolation) {
		t.Fatalf("CREATE CONSTRAINT over duplicates: got %v, want ErrConstraintViolation", createErr)
	}
}

// TestCreateConstraint_InMemorySeedsThenRejects verifies the seed half on an
// in-memory engine: one pre-existing node, CREATE CONSTRAINT succeeds (clean),
// then a duplicate of the pre-existing value is rejected.
func TestCreateConstraint_InMemorySeedsThenRejects(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	if err := cdRunMem(t, eng, `CREATE (n:P {k: 'only'})`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := cdRunMem(t, eng, `CREATE CONSTRAINT u_k ON (n:P) ASSERT n.k IS UNIQUE`); err != nil {
		t.Fatalf("CREATE CONSTRAINT over clean data: %v", err)
	}
	dupErr := cdRunMem(t, eng, `CREATE (n:P {k: 'only'})`)
	if dupErr == nil || !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("post-creation duplicate of pre-existing value: got %v, want ErrConstraintViolation", dupErr)
	}
}

// TestCreateConstraint_FloatUniqueViaCypher is the #1318 AC end-to-end through
// Cypher: a UNIQUE constraint on a float property rejects a duplicate float
// inserted via CREATE. Before the fix the float value-set key was empty and the
// duplicate committed silently.
func TestCreateConstraint_FloatUniqueViaCypher(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	if err := cdRunMem(t, eng, `CREATE CONSTRAINT u_score ON (n:M) ASSERT n.score IS UNIQUE`); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if err := cdRunMem(t, eng, `CREATE (n:M {score: 1.5})`); err != nil {
		t.Fatalf("first float insert: %v", err)
	}
	dupErr := cdRunMem(t, eng, `CREATE (n:M {score: 1.5})`)
	if dupErr == nil || !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("duplicate float insert: got %v, want ErrConstraintViolation", dupErr)
	}
	// A distinct float still inserts.
	if err := cdRunMem(t, eng, `CREATE (n:M {score: 2.5})`); err != nil {
		t.Fatalf("distinct float insert rejected: %v", err)
	}
}

// TestCreateConstraint_RejectsPreExistingDuplicates is the #1317 AC: CREATE
// CONSTRAINT over data that already contains a duplicate value is rejected,
// with the constraint NOT registered. Before the fix the constraint was
// created over an empty value-set without scanning, so it was silently inert.
func TestCreateConstraint_RejectsPreExistingDuplicates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed two nodes that share a value, with no constraint yet.
	if err := cdCycle(t, dir, false,
		`CREATE (n:Person {email: 'dup@example.com'})`,
		`CREATE (n:Person {email: 'dup@example.com'})`,
	); err != nil {
		t.Fatalf("seed duplicates: %v", err)
	}

	// CREATE CONSTRAINT over the already-duplicated data must fail.
	createErr := cdCycle(t, dir, false,
		`CREATE CONSTRAINT u_email ON (n:Person) ASSERT n.email IS UNIQUE`,
	)
	if createErr == nil {
		t.Fatal("CREATE CONSTRAINT over already-duplicated data was accepted; want a constraint violation")
	}
	if !errors.Is(createErr, exec.ErrConstraintViolation) {
		t.Fatalf("got %v, want one wrapping exec.ErrConstraintViolation", createErr)
	}

	// The constraint must NOT have been registered (creation was rejected), so
	// a third duplicate still inserts cleanly.
	if err := cdCycle(t, dir, false,
		`CREATE (n:Person {email: 'dup@example.com'})`,
	); err != nil {
		t.Fatalf("insert after rejected CREATE CONSTRAINT should be unconstrained, got: %v", err)
	}
}

// TestCreateConstraint_SeedsValueSetAgainstPreExisting is the second half of
// #1317: when CREATE CONSTRAINT succeeds over clean pre-existing data, the
// value-set is seeded from that data, so a post-creation write that duplicates
// a PRE-EXISTING value is rejected. Before the fix the value-set started empty
// and such a duplicate was accepted.
func TestCreateConstraint_SeedsValueSetAgainstPreExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// One pre-existing node, then declare the constraint (clean data → ok).
	if err := cdCycle(t, dir, false,
		`CREATE (n:Customer {code: 'C1'})`,
		`CREATE CONSTRAINT u_code ON (n:Customer) ASSERT n.code IS UNIQUE`,
	); err != nil {
		t.Fatalf("seed + create constraint: %v", err)
	}

	// Inserting a node that duplicates the PRE-EXISTING value must be rejected:
	// only possible if CREATE CONSTRAINT seeded the value-set from 'C1'.
	dupErr := cdCycle(t, dir, false,
		`CREATE (n:Customer {code: 'C1'})`,
	)
	if dupErr == nil {
		t.Fatal("post-creation duplicate of a pre-existing value was accepted; value-set was not seeded on CREATE CONSTRAINT")
	}
	if !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("got %v, want one wrapping exec.ErrConstraintViolation", dupErr)
	}
}

// TestConstraint_DistinctValueSurvivesReopen confirms the recovered value-set
// is seeded from the recovered nodes: after reopen, re-inserting the SAME value
// that was committed before the reopen is rejected (the value-set was rebuilt
// by scanning the recovered graph, not left empty). This exercises the #1316
// "repopulate each unique value-set by scanning recovered nodes" requirement
// directly, independent of whether the live value-set carried over in memory.
func TestConstraint_DistinctValueSurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Declare and insert in cycle 1.
	if err := cdCycle(t, dir, false,
		`CREATE CONSTRAINT u_login ON (n:Account) ASSERT n.login IS UNIQUE`,
		`CREATE (n:Account {login: 'root'})`,
	); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	// Reopen in a FRESH cycle and immediately attempt to re-insert the
	// pre-existing value. A fresh engine instance means the only way this can
	// be rejected is if recovery re-registered the constraint AND the value-set
	// was re-seeded from the recovered 'root' node.
	dupErr := cdCycle(t, dir, false,
		`CREATE (n:Account {login: 'root'})`,
	)
	if dupErr == nil {
		t.Fatal("re-inserting a pre-existing value after reopen was accepted; the value-set was not re-seeded from recovered nodes")
	}
	if !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("got %v, want one wrapping exec.ErrConstraintViolation", dupErr)
	}
}

// TestExistenceConstraint_WALBackedEnforcedAndSurvivesRecovery is the #1754 AC on
// the WAL-backed engine: a NOT NULL constraint is enforced on the WAL-backed
// write path (a CREATE omitting the property is rejected), AND a recovered engine
// re-registers the constraint via recovery.Constraints and continues to enforce
// it on NEW writes — confirming no recovery-side change is needed (recovery
// replays only committed, already-valid transactions).
func TestExistenceConstraint_WALBackedEnforcedAndSurvivesRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Cycle 1: declare the constraint and insert a VALID node (with the prop), so
	// the WAL carries the constraint op plus one good node.
	if err := cdCycle(t, dir, false,
		`CREATE CONSTRAINT acct_email_nn ON (n:Acct) ASSERT n.email IS NOT NULL`,
		`CREATE (n:Acct {id: 1, email: 'a@x'})`,
	); err != nil {
		t.Fatalf("cycle 1 (declare + valid insert): %v", err)
	}

	// Cycle 1b: on the same WAL-backed path, a CREATE omitting email must be
	// rejected with exec.ErrConstraintViolation (commit-time enforcement on the
	// WAL adapter), and must leave nothing durable.
	omitErr := cdCycle(t, dir, false, `CREATE (n:Acct {id: 2})`)
	if omitErr == nil {
		t.Fatal("WAL-backed CREATE omitting the constrained property was accepted")
	}
	if !errors.Is(omitErr, exec.ErrConstraintViolation) {
		t.Fatalf("WAL-backed omit: got %v, want one wrapping exec.ErrConstraintViolation", omitErr)
	}

	// Cycle 2: reopen in a FRESH cycle. The recovered engine
	// (NewEngineWithStoreAndConstraints) re-registers the constraint, so a NEW
	// write omitting the property is still rejected — proving recovery-side
	// enforcement works with no recovery change.
	afterRecoveryErr := cdCycle(t, dir, false, `CREATE (n:Acct {id: 3})`)
	if afterRecoveryErr == nil {
		t.Fatal("after recovery, a CREATE omitting the constrained property was accepted; the constraint did not survive recovery")
	}
	if !errors.Is(afterRecoveryErr, exec.ErrConstraintViolation) {
		t.Fatalf("after recovery: got %v, want one wrapping exec.ErrConstraintViolation", afterRecoveryErr)
	}

	// And a valid insert after recovery is still accepted (enforcement, not a
	// blanket reject).
	if err := cdCycle(t, dir, false, `CREATE (n:Acct {id: 4, email: 'd@x'})`); err != nil {
		t.Fatalf("valid insert after recovery rejected: %v", err)
	}
}
