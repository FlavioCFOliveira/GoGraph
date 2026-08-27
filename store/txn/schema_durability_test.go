package txn

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// typedStore builds a store whose graph carries a schema declaring "age" as an
// int64 property, and returns it with the WAL path so a test can replay.
func typedStore(t *testing.T) (*Store[string, int64], string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	sc := schema.New(nil, nil)
	if _, err := sc.RegisterProperty("age", lpg.PropInt64); err != nil {
		t.Fatalf("RegisterProperty: %v", err)
	}
	g.SetValidator(sc)

	return NewStoreWithCodec(g, w, NewStringCodec()), path
}

// walOpKinds opens the WAL read-only and returns the op-kind byte of every v3
// frame, in order.
//
// Reading the LOG rather than replaying it is the sharper assertion for rmp
// #2602: recovery can only materialise what the log contains, so the absence of
// an OpSetNodeProperty frame settles the question without depending on how
// recovery is wired.
func walOpKinds(t *testing.T, path string) []byte {
	t.Helper()
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	var kinds []byte
	if err := r.Replay(func(f wal.Frame) error {
		if len(f.Payload) >= 2 && f.Payload[0] == OpRecordV3 {
			kinds = append(kinds, f.Payload[1])
		}
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return kinds
}

// hasKind reports whether kinds contains k.
func hasKind(kinds []byte, k OpKind) bool {
	for _, got := range kinds {
		if got == byte(k) {
			return true
		}
	}
	return false
}

// TestSchemaRefusedValueNeverReachesTheWAL guards rmp #2602: a value the
// installed schema validator refuses must not become durable.
//
// Before this, Tx.Commit appended and fsynced every buffered op and only THEN
// applied it through lpg, which is where the validator hook lives. The rejection
// therefore arrived after the frame was durable: Commit returned
// ErrCommittedNotApplied, the LIVE graph stayed clean — and recovery, which
// installs no validator, replayed the refused value straight in. Measured
// 2026-08-24: an `age` declared PropInt64 came back from a host crash as a
// STRING.
//
// That is a Consistency breach in the ACID sense the module claims, which names
// label/property typing among the invariants a committed transaction must leave
// satisfied. The Cypher write path was never exposed, because
// walMutatorAdapter.SetNodeProperty validates BEFORE buffering its WAL op; this
// makes store/txn agree with it.
func TestSchemaRefusedValueNeverReachesTheWAL(t *testing.T) {
	t.Parallel()
	s, walPath := typedStore(t)

	tx := s.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// THE assertion: the refusal lands at BUFFER time, before anything is
	// appended, rather than from Commit after the fsync.
	err := tx.SetNodeProperty("n", "age", lpg.StringValue("not-an-int"))
	if err == nil {
		t.Fatalf("SetNodeProperty accepted a STRING for a property declared PropInt64: " +
			"the op is now buffered and Commit will make it durable before the validator " +
			"ever sees it (rmp #2602)")
	}
	if !errors.Is(err, schema.ErrTypeMismatch) {
		t.Errorf("SetNodeProperty error = %v, want one satisfying errors.Is(err, "+
			"schema.ErrTypeMismatch) so the caller can tell a schema refusal from any "+
			"other buffering failure", err)
	}

	// The transaction is still usable and its VALID work still commits: the
	// refusal rejected one op, not the batch.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after a refused buffer: %v; the refusal must reject the OP, "+
			"not poison the transaction", err)
	}

	// The live graph must not carry it...
	if _, ok := s.Graph().GetNodeProperty("n", "age"); ok {
		t.Errorf("the live graph carries `age` after the refusal")
	}

	// ...and neither must the WAL, which is the only thing recovery can read.
	kinds := walOpKinds(t, walPath)
	if hasKind(kinds, OpSetNodeProperty) {
		t.Errorf("the WAL carries an OpSetNodeProperty frame after the refusal (kinds %v): "+
			"the refused op reached the log, so a crash and reopen resurrects a value the "+
			"schema rejected, because replay installs no validator (rmp #2602)", kinds)
	}
	// Assert something WAS seen: the AddNode frame must be there, or the check
	// above would pass simply because nothing was logged at all.
	if !hasKind(kinds, OpAddNode) {
		t.Fatalf("the WAL carries no OpAddNode frame (kinds %v), so the absence of the "+
			"property frame proves nothing about the refused op", kinds)
	}
}

// TestSchemaAcceptedValueStillReachesTheWAL is the control. Without it the guard
// above could be satisfied by a buffer path that refuses everything, which would
// pass while breaking every typed write.
func TestSchemaAcceptedValueStillReachesTheWAL(t *testing.T) {
	t.Parallel()
	s, walPath := typedStore(t)

	tx := s.Begin()
	if err := tx.AddNode("n"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.SetNodeProperty("n", "age", lpg.Int64Value(41)); err != nil {
		t.Fatalf("SetNodeProperty rejected a value the schema declares: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	v, ok := s.Graph().GetNodeProperty("n", "age")
	if !ok {
		t.Fatalf("live graph carries no `age` after an ACCEPTED typed write")
	}
	if got, isInt := v.Int64(); !isInt || got != 41 {
		t.Errorf("live graph age = %v (int=%t), want 41", got, isInt)
	}
	kinds := walOpKinds(t, walPath)
	if !hasKind(kinds, OpSetNodeProperty) {
		t.Errorf("the WAL carries no OpSetNodeProperty frame (kinds %v): a valid typed write "+
			"must still reach the log, or the guard has broken every typed write rather than "+
			"the refused one", kinds)
	}
}

// TestSchemaRefusalGuardsEveryValueBearingSetter checks the whole family rather
// than the one method the defect was reported against, so a sibling left
// unguarded is a failure here instead of a second ticket later.
func TestSchemaRefusalGuardsEveryValueBearingSetter(t *testing.T) {
	t.Parallel()
	bad := lpg.StringValue("not-an-int")

	for _, tc := range []struct {
		name string
		call func(tx *Tx[string, int64]) error
	}{
		{"SetNodeProperty", func(tx *Tx[string, int64]) error {
			return tx.SetNodeProperty("a", "age", bad)
		}},
		{"SetEdgeProperty", func(tx *Tx[string, int64]) error {
			return tx.SetEdgeProperty("a", "b", "age", bad)
		}},
		{"SetEdgePropertyByHandle", func(tx *Tx[string, int64]) error {
			return tx.SetEdgePropertyByHandle("a", "b", 1, "age", bad)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _ := typedStore(t)
			tx := s.Begin()
			if err := tc.call(tx); !errors.Is(err, schema.ErrTypeMismatch) {
				t.Errorf("%s buffered a value the schema refuses (err = %v): every setter that "+
					"carries a PropertyValue must reject before the WAL, not after the fsync "+
					"(rmp #2602)", tc.name, err)
			}
		})
	}
}
