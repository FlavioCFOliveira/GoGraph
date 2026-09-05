package recovery

// edge_property_recovery_test.go — regression gate for tasks #1418 and #2707.
//
// ONE invariant, proven on every edge-property write path: when the graph
// REFUSES a durably-committed property write, the write path must FAIL-STOP,
// never swallow the refusal. A refusal is reachable because the graph is
// caller-supplied and lpg.Graph.SetValidator is public — an installed
// SchemaValidator is the case these tests drive.
//
// #1418 (by-handle). txn.applyOp and recovery.applySetEdgePropertyByHandle both
// discarded the error from lpg.Graph.SetEdgePropertyByHandle. Fixed by
// propagating it: ErrCommittedNotApplied on Commit, `return false` on recovery.
//
// #2707 (by pair). The header this file carried until then ASSERTED that the
// sibling op OpSetEdgeProperty already propagated on BOTH paths. Half of that was
// false, and the false half was the untested one: recovery.applyOpCodec's
// OpSetEdgeProperty case read
//
//	_ = g.SetEdgeProperty(src, dst, key, val) //nolint:errcheck // no schema validator during WAL replay
//
// so a committed edge property was DROPPED on recovery with no error, no metric,
// and replay reporting success — while the adjacent OpSetNodeProperty case, one
// line above, fail-stopped with a counter. Fixed by mirroring that case exactly
// (store.recovery.applyOp.setEdgePropertyErrors + `return false`), and gated
// below by driving the real ReplayWAL rather than the internal apply helper.
//
// Tests:
//  1. Txn path, by handle (txn.go): Commit with a rejecting validator returns
//     ErrCommittedNotApplied — consistent with OpSetNodeProperty.
//  2. Recovery path, by handle (recovery.go): applySetEdgePropertyByHandle
//     returns false when the validator rejects (white-box via the internal
//     function).
//  3. Recovery path, by pair (#2707): ReplayWAL over a real committed WAL, into
//     a caller-supplied graph carrying a rejecting validator, reports a tail
//     error and emits the error counter instead of dropping the property.
//  4. Consistency: the node-property and edge-property lpg.Graph primitives all
//     surface the rejection, confirming the uniform behaviour the apply paths
//     are required to propagate.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// rejectingValidator is a lpg.SchemaValidator that rejects every write for the
// named property key unconditionally.
type rejectingValidator struct{ key string }

func (v *rejectingValidator) Validate(propertyName string, _ lpg.PropertyValue) error {
	if propertyName == v.key {
		return fmt.Errorf("rejectingValidator: rejected write to property %q", v.key)
	}
	return nil
}

// buildSetEdgePropertyByHandleRest encodes the "rest" body expected by
// applySetEdgePropertyByHandle: uint16 key-len + key + Int64Value bytes
// (tag 0x01 + 8-byte LE int64) + 8-byte trailing handle.
func buildSetEdgePropertyByHandleRest(key string, val int64, handle uint64) []byte {
	keyLen := len(key)
	buf := make([]byte, 2+keyLen+1+8+8)
	binary.LittleEndian.PutUint16(buf[0:], uint16(keyLen))
	copy(buf[2:], key)
	off := 2 + keyLen
	buf[off] = 0x01 // PropertyKindInt64 tag (graph/lpg property encoding)
	off++
	binary.LittleEndian.PutUint64(buf[off:], uint64(val))
	off += 8
	binary.LittleEndian.PutUint64(buf[off:], handle)
	return buf
}

// TestEdgePropertyByHandle_Txn_ValidatorRejection_ReportsErrCommittedNotApplied
// exercises the txn.go applyOp path.
//
// Pre-fix: _ = g.SetEdgePropertyByHandle(...) silences the error → Commit nil.
// Post-fix: return g.SetEdgePropertyByHandle(...) propagates ErrCommittedNotApplied.
//
// THE VALIDATOR IS NOW INSTALLED AFTER BUFFERING, and that is the only way left
// to reach this path. Since rmp #2602 txn.Tx.SetEdgePropertyByHandle validates
// when the op is buffered, so a validator installed BEFORE the buffer refuses
// there and the op never reaches apply — which is the point of that fix, and
// would make this test assert nothing about the apply path.
//
// Installing it between the buffer and the Commit is not a contrivance: the
// validator is swappable at runtime through a lock-free atomic
// (lpg.atomicValidator), so a schema installed while a transaction is open is a
// real ordering. It is also the ordering that keeps the ORIGINAL guard alive —
// the apply path must PROPAGATE a rejection rather than silence it — which the
// buffer-time check does not subsume.
func TestEdgePropertyByHandle_Txn_ValidatorRejection_ReportsErrCommittedNotApplied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	const propKey = "secret"
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer g.SetValidator(nil)

	s := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	tx := s.Begin()
	mustTx(t, tx.AddNode("x"))
	mustTx(t, tx.AddNode("y"))
	mustTx(t, tx.AddEdge("x", "y", 1))
	// Buffered with NO validator installed, so the op reaches t.ops...
	mustTx(t, tx.SetEdgePropertyByHandle("x", "y", 1, propKey, lpg.Int64Value(42)))

	// ...and the validator arrives before the apply phase, which runs after the
	// WAL fsync. This is the ordering the apply-path guard exists for.
	g.SetValidator(&rejectingValidator{key: propKey})

	commitErr := tx.Commit()
	if commitErr == nil {
		t.Fatal("Commit returned nil with a rejecting validator; expected ErrCommittedNotApplied")
	}
	if !errors.Is(commitErr, txn.ErrCommittedNotApplied) {
		t.Fatalf("Commit error = %v; want errors.Is(..., txn.ErrCommittedNotApplied)", commitErr)
	}

	_ = w.Close()
}

// TestEdgePropertyByHandle_Recovery_ValidatorRejection_IsFailStop exercises
// applySetEdgePropertyByHandle directly (white-box).
//
// Pre-fix: returns true (error silenced). Post-fix: returns false (fail-stop).
func TestEdgePropertyByHandle_Recovery_ValidatorRejection_IsFailStop(t *testing.T) {
	t.Parallel()

	const propKey = "classified"

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}
	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}

	g.SetValidator(&rejectingValidator{key: propKey})
	defer g.SetValidator(nil)

	rest := buildSetEdgePropertyByHandleRest(propKey, 99, h)
	ok := applySetEdgePropertyByHandle(g, "a", "b", rest)
	if ok {
		t.Fatal("applySetEdgePropertyByHandle returned true with a rejecting validator; expected false (fail-stop)")
	}
}

// TestEdgePropertyByHandle_Recovery_ConsistentWithSetNodeProperty confirms
// that the graph-level lpg.Graph methods used by both apply paths surface errors
// uniformly when the validator rejects a write — OpSetEdgePropertyByHandle and
// OpSetNodeProperty must therefore exhibit the same fail-stop behaviour.
func TestEdgePropertyByHandle_Recovery_ConsistentWithSetNodeProperty(t *testing.T) {
	t.Parallel()

	const propKey = "forbidden"

	// Sub-test A: SetEdgePropertyByHandle returns error on validator rejection.
	t.Run("SetEdgePropertyByHandle_returns_error", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.AddNode("b"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		h, err := g.AddEdgeH("a", "b", 0)
		if err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
		g.SetValidator(&rejectingValidator{key: propKey})
		defer g.SetValidator(nil)

		if err := g.SetEdgePropertyByHandle("a", "b", h, propKey, lpg.Int64Value(1)); err == nil {
			t.Fatal("SetEdgePropertyByHandle: expected error from validator, got nil")
		}
	})

	// Sub-test B: SetNodeProperty returns error on validator rejection (sibling).
	t.Run("SetNodeProperty_returns_error", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		g.SetValidator(&rejectingValidator{key: propKey})
		defer g.SetValidator(nil)

		if err := g.SetNodeProperty("n", propKey, lpg.Int64Value(1)); err == nil {
			t.Fatal("SetNodeProperty: expected error from validator, got nil")
		}
	})
}

// namedCounter is a metrics backend that records exactly one counter by name.
// That counter is the ONLY machine-readable signal a recovery fail-stop emits for
// this op — ReplayResult carries no per-op field — so asserting the fix landed
// means installing a backend that can see it.
type namedCounter struct {
	mu   sync.Mutex
	name string
	n    uint64
}

func (c *namedCounter) IncCounter(name string, delta uint64) {
	if name != c.name {
		return
	}
	c.mu.Lock()
	c.n += delta
	c.mu.Unlock()
}
func (c *namedCounter) ObserveLatency(string, time.Duration) {}
func (c *namedCounter) SetGauge(string, float64)             {}

func (c *namedCounter) count() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestEdgeProperty_ReplayWAL_ValidatorRejection_IsFailStop is the gate for rmp
// #2707's first defect: recovery.applyOpCodec's txn.OpSetEdgeProperty case
// swallowed the error from lpg.Graph.SetEdgeProperty, so a DURABLY COMMITTED
// edge property was silently lost on replay.
//
// It drives the real, exported ReplayWAL — not the internal apply helper —
// because the swallowed error is reachable exactly through the property that
// makes the suppression's justification ("no schema validator during WAL replay")
// false: ReplayWAL takes a CALLER-SUPPLIED *lpg.Graph and lpg.Graph.SetValidator
// is public. The producer store writes and fsyncs the property with no validator
// installed, as an ordinary embedder does; the consumer is a different graph that
// carries one, as an embedder that declared a schema before reopening does.
//
// Gate invariant:
//   - FAILS on the unfixed code: replay returns a nil TailErr, the counter never
//     fires, and the recovered graph is missing an acknowledged write — durability
//     and consistency lost, fail-SILENT.
//   - PASSES on the fixed code: replay stops with a tail error and the
//     store.recovery.applyOp.setEdgePropertyErrors counter fires once, exactly as
//     the adjacent txn.OpSetNodeProperty case has always done.
//
// It installs the process-wide metrics backend, so it must NOT run in parallel.
func TestEdgeProperty_ReplayWAL_ValidatorRejection_IsFailStop(t *testing.T) {
	const (
		propKey    = "score"
		errCounter = "store.recovery.applyOp.setEdgePropertyErrors"
		propValue  = int64(42)
		srcKey     = "a"
		dstKey     = "b"
		edgeWeight = float64(1)
	)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- Producer: an ordinary WAL-backed store with NO validator commits an
	// edge property. The commit is acknowledged and fsynced, so the property is
	// durable and every recovery is obliged to reproduce it or fail loudly.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	producer := lpg.New[string, float64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, float64](producer, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})

	tx := s.Begin()
	mustTx(t, tx.AddNode(srcKey))
	mustTx(t, tx.AddNode(dstKey))
	mustTx(t, tx.AddEdge(srcKey, dstKey, edgeWeight))
	mustTx(t, tx.SetEdgeProperty(srcKey, dstKey, propKey, lpg.Int64Value(propValue)))
	if cerr := tx.Commit(); cerr != nil {
		t.Fatalf("producer Commit: %v", cerr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal.Close: %v", cerr)
	}
	// The write really was acknowledged: without this the test could pass over a
	// WAL that never carried the op at all.
	if _, ok := producer.GetEdgeProperty(srcKey, dstKey, propKey); !ok {
		t.Fatalf("producer graph is missing %q after an acknowledged commit; the test drives the wrong frame", propKey)
	}

	// --- Consumer: replay into a DIFFERENT, caller-supplied graph that carries a
	// validator rejecting propKey.
	recovered := lpg.New[string, float64](adjlist.Config{Directed: true})
	recovered.SetValidator(&rejectingValidator{key: propKey})
	defer recovered.SetValidator(nil)

	probe := &namedCounter{name: errCounter}
	metrics.SetBackend(probe)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	res, rerr := ReplayWAL(context.Background(), r, recovered,
		txn.NewStringCodec(), txn.NewFloat64WeightCodec(), 0)
	if rerr != nil {
		t.Fatalf("ReplayWAL returned an unexpected function error: %v", rerr)
	}

	// (1) The refusal must be REPORTED. This is the whole defect: before the fix
	// TailErr is nil and replay claims success.
	if res.TailErr == nil {
		_, present := recovered.GetEdgeProperty(srcKey, dstKey, propKey)
		t.Fatalf("ReplayWAL reported success (TailErr == nil) while the graph refused a durably committed "+
			"edge property; recovered graph has %q present:%v. A committed write was dropped fail-silent "+
			"(rmp #2707, ACID Durability + Consistency)", propKey, present)
	}

	// (2) The refusal must be COUNTED, mirroring the adjacent OpSetNodeProperty
	// case's store.recovery.applyOp.setNodePropertyErrors exactly.
	if got := probe.count(); got != 1 {
		t.Errorf("%s = %d, want 1", errCounter, got)
	}

	// (3) And replay must genuinely have stopped rather than continued past the
	// refused op: the refused property is absent, which is only acceptable
	// BECAUSE (1) reported it.
	if _, present := recovered.GetEdgeProperty(srcKey, dstKey, propKey); present {
		t.Errorf("recovered graph carries %q although the validator rejected it", propKey)
	}
}
