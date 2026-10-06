package txn_test

// noop_over_inflight_test.go — an op whose effect is decided against another
// transaction's UNCOMMITTED write must wait for it, never be acknowledged as a
// no-op it was not (ACID audit round 6, finding C1).
//
// The store logs effects only: an op whose in-memory apply wrote no version is
// left out of the WAL. That is sound only when "wrote nothing" was decided
// against COMMITTED state. Decided against a peer's pending write — a pending
// revival, a pending first creation, a pending identical edge handle — the op
// takes no claim, logs nothing and is acknowledged at once; when the peer then
// fails its fsync, the acknowledged effect is gone from memory, and it was
// never in the WAL.
//
// Every case here holds a peer transaction B inside its durable step, with its
// writes applied and uncommitted, then commits A through the store. B's durable
// step then FAILS, so the serial history is "B aborted, A committed": memory,
// the WAL recovered from a copy taken the instant A was acknowledged, and the
// final WAL recovered must each equal the state a fresh store reaches by
// committing A alone over the same setup.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

type ioOp = txn.Op[string, float64]

// ioHandle is the stable handle the setups give the x→y edge.
const ioHandle = 7

// ioFreshHandle is the handle of an edge no setup creates: the op [Tx.AddEdge]
// buffers, which mints its handle at buffer time.
const ioFreshHandle = 9

// ioOps is every non-schema op kind a [txn.Tx] buffers, each on the fixed
// domain {x, y} and the x→y edge carrying ioHandle. [txn.OpAddEdge] and
// [txn.OpAddEdgeWeighted] are left out: no Tx buffers them (Tx.AddEdge buffers
// an [txn.OpAddEdgeH] with a handle minted at buffer time), and replaying one
// mints its handle at apply time, so its handle is not a function of the log.
var ioOps = []ioOp{
	{Kind: txn.OpAddNode, Src: "x"},
	{Kind: txn.OpRemoveNode, Src: "x"},
	{Kind: txn.OpSetNodeLabel, Src: "x", Label: "L"},
	{Kind: txn.OpRemoveNodeLabel, Src: "x", Label: "L"},
	{Kind: txn.OpSetNodeProperty, Src: "x", Key: "p", Value: lpg.Int64Value(1)},
	{Kind: txn.OpDelNodeProperty, Src: "x", Key: "p"},
	{Kind: txn.OpAddEdgeH, Src: "x", Dst: "y", Weight: 2, Handle: ioFreshHandle},
	{Kind: txn.OpRemoveEdge, Src: "x", Dst: "y"},
	{Kind: txn.OpSetEdgeLabel, Src: "x", Dst: "y", Label: "R"},
	{Kind: txn.OpSetEdgeProperty, Src: "x", Dst: "y", Key: "e", Value: lpg.Int64Value(1)},
	{Kind: txn.OpDelEdgeProperty, Src: "x", Dst: "y", Key: "e"},
	{Kind: txn.OpAddEdgeH, Src: "x", Dst: "y", Weight: 1, Handle: ioHandle},
	{Kind: txn.OpSetEdgeLabelByHandle, Src: "x", Dst: "y", Handle: ioHandle, Label: "R"},
	{Kind: txn.OpSetEdgePropertyByHandle, Src: "x", Dst: "y", Handle: ioHandle, Key: "q", Value: lpg.Int64Value(1)},
	{Kind: txn.OpDelEdgePropertyByHandle, Src: "x", Dst: "y", Handle: ioHandle, Key: "q"},
	{Kind: txn.OpRemoveEdgeInstanceByHandle, Src: "x", Dst: "y", Handle: ioHandle},
	{Kind: txn.OpRemoveEdgeByHandle, Src: "x", Dst: "y", Handle: ioHandle},
}

func ioOpName(op *ioOp) string {
	names := map[txn.OpKind]string{
		txn.OpAddNode: "AddNode", txn.OpRemoveNode: "RemoveNode", txn.OpSetNodeLabel: "SetNodeLabel",
		txn.OpRemoveNodeLabel: "RemoveNodeLabel", txn.OpSetNodeProperty: "SetNodeProperty",
		txn.OpDelNodeProperty: "DelNodeProperty", txn.OpAddEdge: "AddEdge", txn.OpAddEdgeWeighted: "AddEdgeWeighted",
		txn.OpRemoveEdge: "RemoveEdge", txn.OpSetEdgeLabel: "SetEdgeLabel", txn.OpSetEdgeProperty: "SetEdgeProperty",
		txn.OpDelEdgeProperty: "DelEdgeProperty", txn.OpAddEdgeH: "AddEdgeH",
		txn.OpSetEdgeLabelByHandle: "SetEdgeLabelByHandle", txn.OpSetEdgePropertyByHandle: "SetEdgePropertyByHandle",
		txn.OpDelEdgePropertyByHandle: "DelEdgePropertyByHandle", txn.OpRemoveEdgeInstanceByHandle: "RemoveEdgeInstanceByHandle",
		txn.OpRemoveEdgeByHandle: "RemoveEdgeByHandle",
	}
	if op.Kind == txn.OpAddEdgeH && op.Handle == ioFreshHandle {
		return "AddEdgeFresh"
	}
	return names[op.Kind]
}

// ioSetups are the committed states the matrix starts from.
var ioSetups = []struct {
	name string
	ops  []ioOp
}{
	{"empty", nil},
	{"x,y alive", []ioOp{{Kind: txn.OpAddNode, Src: "x"}, {Kind: txn.OpAddNode, Src: "y"}}},
	{"x labelled, edge with metadata", []ioOp{
		{Kind: txn.OpAddNode, Src: "x"}, {Kind: txn.OpAddNode, Src: "y"},
		{Kind: txn.OpSetNodeLabel, Src: "x", Label: "L"},
		{Kind: txn.OpSetNodeProperty, Src: "x", Key: "p", Value: lpg.Int64Value(1)},
		{Kind: txn.OpAddEdgeH, Src: "x", Dst: "y", Weight: 1, Handle: ioHandle},
		{Kind: txn.OpSetEdgeLabelByHandle, Src: "x", Dst: "y", Handle: ioHandle, Label: "R"},
		{Kind: txn.OpSetEdgePropertyByHandle, Src: "x", Dst: "y", Handle: ioHandle, Key: "q", Value: lpg.Int64Value(1)},
		{Kind: txn.OpSetEdgeLabel, Src: "x", Dst: "y", Label: "R"},
		{Kind: txn.OpSetEdgeProperty, Src: "x", Dst: "y", Key: "e", Value: lpg.Int64Value(1)},
	}},
	{"x dead", []ioOp{
		{Kind: txn.OpAddNode, Src: "x"}, {Kind: txn.OpAddNode, Src: "y"},
		{Kind: txn.OpSetNodeLabel, Src: "x", Label: "L"},
		{Kind: txn.OpSetNodeProperty, Src: "x", Key: "p", Value: lpg.Int64Value(1)},
		{Kind: txn.OpRemoveNode, Src: "x"},
	}},
	{"edge removed", []ioOp{
		{Kind: txn.OpAddNode, Src: "x"}, {Kind: txn.OpAddNode, Src: "y"},
		{Kind: txn.OpAddEdgeH, Src: "x", Dst: "y", Weight: 1, Handle: ioHandle},
		{Kind: txn.OpRemoveEdgeByHandle, Src: "x", Dst: "y", Handle: ioHandle},
	}},
}

var ioOpts = txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}

// ioDump renders the committed observable state of x and y.
func ioDump(g *lpg.Graph[string, float64]) string {
	var b strings.Builder
	adj := g.AdjList()
	props := func(m map[string]lpg.PropertyValue) string {
		ks := make([]string, 0, len(m))
		for k, v := range m {
			ks = append(ks, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	sorted := func(s []string) []string { s = append([]string(nil), s...); sort.Strings(s); return s }
	for _, k := range []string{"x", "y"} {
		id, ok := adj.Mapper().Lookup(k)
		alive := ok && !g.IsTombstoned(id)
		fmt.Fprintf(&b, "%s alive=%v", k, alive)
		if !alive {
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(&b, " labels=%v props={%s}\n", sorted(g.NodeLabels(k)), props(g.NodeProperties(k)))
		nbrs, ws, hs := adj.LoadEntryH(id)
		var es []string
		for j, d := range nbrs {
			dk, _ := adj.Mapper().Resolve(d)
			var h uint64
			if j < len(hs) {
				h = hs[j]
			}
			e := fmt.Sprintf("  -> %s w=%v h=%d", dk, ws[j], h)
			if h != 0 {
				e += fmt.Sprintf(" hl=%v hp={%s}", sorted(g.EdgeLabelsByHandle(k, dk, h)), props(g.EdgePropertiesByHandle(k, dk, h)))
			}
			es = append(es, e)
		}
		sort.Strings(es)
		for _, e := range es {
			b.WriteString(e + "\n")
		}
		if len(nbrs) > 0 {
			fmt.Fprintf(&b, "  pair labels=%v props={%s}\n", sorted(g.EdgeLabels(k, "y")), props(g.EdgeProperties(k, "y")))
		}
	}
	return b.String()
}

func ioOpen(t *testing.T, dir string) (*wal.Writer, *lpg.Graph[string, float64], *txn.Store[string, float64]) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return w, g, txn.NewStoreWithOptions[string, float64](g, w, ioOpts)
}

func ioCommit(st *txn.Store[string, float64], ops ...ioOp) error {
	tx := st.Begin()
	for i := range ops {
		tx.BufferOpForTest(ops[i])
	}
	return tx.Commit()
}

func ioRecover(t *testing.T, dir string) string {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(ioOpts))
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	return ioDump(res.Graph)
}

// ioCopyDir copies the files of src into dst, leaving out the WAL lock: the
// state a crash at this instant leaves on disk.
func ioCopyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if info.Name() == "wal.lock" {
			return nil
		}
		in, err := os.Open(p) //nolint:gosec // p is a file under the test's own temporary directory
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target) //nolint:gosec // target is under the test's own temporary directory
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatalf("crash copy: %v", err)
	}
}

type ioOracle struct {
	dump string
	err  bool
}

// ioSerial is the state a fresh store reaches by committing a alone over setup.
func ioSerial(t *testing.T, setup []ioOp, a *ioOp) ioOracle {
	t.Helper()
	w, g, st := ioOpen(t, t.TempDir())
	defer w.Close()
	if len(setup) > 0 {
		if err := ioCommit(st, setup...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	err := ioCommit(st, *a)
	return ioOracle{dump: ioDump(g), err: err != nil}
}

// ioRunCase runs one cell of the matrix and reports a non-empty verdict when the
// acknowledged history diverges from the serial one.
func ioRunCase(t *testing.T, setup []ioOp, b, a *ioOp, want ioOracle) string {
	t.Helper()
	dir := t.TempDir()
	w, g, st := ioOpen(t, dir)
	if len(setup) > 0 {
		if err := ioCommit(st, setup...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	setupDump := ioDump(g)
	inB, releaseB, doneB := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		doneB <- g.ApplyDurable(context.Background(), func(wtx lpg.WriteTx) error {
			return txn.ApplyOpForTest(g.Writer(wtx), *b)
		}, func() error {
			close(inB)
			<-releaseB
			return errors.New("injected fsync failure")
		})
	}()
	select {
	case <-inB:
	case err := <-doneB:
		// B's apply refused on its own (an op that errors over this setup):
		// nothing is in flight, so the cell tests nothing.
		_ = err
		_ = w.Close()
		return ""
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(releaseB)
		close(released)
	}()
	errA := ioCommit(st, *a)
	crash := t.TempDir()
	ioCopyDir(t, dir, crash)
	<-doneB
	<-released
	mem := ioDump(g)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var verdicts []string
	if (errA != nil) != want.err {
		verdicts = append(verdicts, fmt.Sprintf("A error %v, serial error %v", errA, want.err))
	}
	detail := fmt.Sprintf("\n--- serial ---\n%s--- memory ---\n%s", want.dump, mem)
	if errA == nil {
		if mem != want.dump {
			verdicts = append(verdicts, "memory != serial")
		}
		if got := ioRecover(t, crash); got != want.dump {
			verdicts = append(verdicts, "crash-at-ack recovery != serial")
			detail += "--- recovered at ack ---\n" + got
		}
	} else if mem != setupDump {
		verdicts = append(verdicts, "refused A changed memory")
	}
	if got := ioRecover(t, dir); got != mem {
		verdicts = append(verdicts, "final recovery != memory")
		detail += "--- recovered at end ---\n" + got
	}
	if len(verdicts) == 0 {
		return ""
	}
	return strings.Join(verdicts, "; ") + detail
}

func ioMatrix(t *testing.T, include func(setup int, b, a *ioOp) bool) {
	bad := 0
	for si := range ioSetups {
		s := &ioSetups[si]
		for ai := range ioOps {
			a := &ioOps[ai]
			var want *ioOracle
			for bi := range ioOps {
				b := &ioOps[bi]
				if !include(si, b, a) {
					continue
				}
				if want == nil {
					o := ioSerial(t, s.ops, a)
					want = &o
				}
				if v := ioRunCase(t, s.ops, b, a, *want); v != "" {
					bad++
					t.Errorf("setup %q, B=%s pending (then aborted), A=%s committed: %s", s.name, ioOpName(b), ioOpName(a), v)
				}
			}
		}
	}
	if bad > 0 {
		t.Logf("%d diverging cells", bad)
	}
}

// TestNoOpOverInFlight_SameOpDiagonal is the short-layer cut of the matrix:
// every op kind A, over every setup, while a peer holds the SAME op pending —
// exactly the cells where A finds nothing left to do in the stored state.
func TestNoOpOverInFlight_SameOpDiagonal(t *testing.T) {
	ioMatrix(t, func(_ int, b, a *ioOp) bool { return b.Kind == a.Kind })
}

// TestNoOpOverInFlight_CreationAndRemoval is every op that writes onto a node
// another transaction is creating, reviving or removing. The three shapes the
// audit reproduced as lost acknowledged writes — AddNode over a pending
// revival and over a pending first creation, and AddEdgeH over a pending
// identical handle — are cells of it and of the same-op diagonal.
func TestNoOpOverInFlight_CreationAndRemoval(t *testing.T) {
	ioMatrix(t, func(_ int, b, _ *ioOp) bool {
		return b.Kind == txn.OpAddNode || b.Kind == txn.OpRemoveNode
	})
}

// TestNoOpOverInFlight_FullMatrix is every (setup, B, A) cell. Soak layer: it
// commits and recovers some three thousand stores.
func TestNoOpOverInFlight_FullMatrix(t *testing.T) {
	testlayers.RequireSoak(t)
	ioMatrix(t, func(int, *ioOp, *ioOp) bool { return true })
}

// TestNoOpOverInFlight_AckedAddNodeSurvivesCrash is the public-API shape of the
// first cell: a real store commit B revives x and then applies a long batch,
// and a store commit A asks AddNode(x) while B is still applying. The WAL
// directory is copied the instant A is acknowledged — a crash at that moment —
// and recovered: A's acknowledged x must be in it. Soak layer: B's batch is
// long so that A reliably lands inside it, and the test fails rather than
// passes when it does not.
func TestNoOpOverInFlight_AckedAddNodeSurvivesCrash(t *testing.T) {
	testlayers.RequireSoak(t)
	dir := t.TempDir()
	w, g, st := ioOpen(t, dir)
	const batch = 300000
	seed := st.Begin()
	_ = seed.AddNode("x")
	for i := 0; i < batch; i++ {
		_ = seed.AddNode(fmt.Sprintf("y%d", i))
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ioCommit(st, ioOp{Kind: txn.OpRemoveNode, Src: "x"}); err != nil {
		t.Fatal(err)
	}
	xid, _ := g.AdjList().Mapper().Lookup("x")
	b := st.Begin()
	_ = b.AddNode("x")
	for i := 0; i < batch; i++ {
		_ = b.SetNodeProperty(fmt.Sprintf("y%d", i), "q", lpg.Int64Value(int64(i)))
	}
	doneB := make(chan error, 1)
	go func() { doneB <- b.Commit() }()
	deadline := time.Now().Add(30 * time.Second)
	for g.IsTombstonedStored(xid) { // until B's apply has revived x, uncommitted
		if time.Now().After(deadline) {
			t.Fatal("B never revived x")
		}
		time.Sleep(50 * time.Microsecond)
	}
	select {
	case <-doneB:
		t.Fatal("B finished before A began: the interleaving was not driven")
	default:
	}
	errA := ioCommit(st, ioOp{Kind: txn.OpAddNode, Src: "x"})
	crash := t.TempDir()
	ioCopyDir(t, dir, crash)
	if errB := <-doneB; errB != nil {
		t.Fatalf("B: %v", errB)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if errA != nil {
		t.Fatalf("A: %v", errA)
	}
	res, err := recovery.Open[string, float64](crash, recovery.OptionsFromTxn(ioOpts))
	if err != nil {
		t.Fatal(err)
	}
	rid, ok := res.Graph.AdjList().Mapper().Lookup("x")
	if !ok || res.Graph.IsTombstoned(rid) {
		t.Error("an acknowledged AddNode(x) is absent from a recovery taken at its acknowledgement")
	}
}
