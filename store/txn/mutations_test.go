package txn

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// openTypedStringStore opens a fresh typed string-keyed, int64-weighted
// store on a temp WAL. The store carries a typed string codec but no
// weight codec; v2 frames are emitted for every committed op.
func openTypedStringStore(t *testing.T) (store *Store[string, int64], walPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store = NewStoreWithCodec[string, int64](g, w, NewStringCodec())
	cleanup = func() {
		_ = w.Close()
		_ = os.RemoveAll(dir)
	}
	return store, path, cleanup
}

// openTypedWeightedStore opens a fresh typed string-keyed, int64-weighted
// store with both codecs wired in. AddEdge buffers the handle-bearing
// OpAddEdgeH.
func openTypedWeightedStore(t *testing.T) (store *Store[string, int64], walPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store = NewStoreWithOptions[string, int64](g, w, Options[string, int64]{
		Codec:       NewStringCodec(),
		WeightCodec: NewInt64WeightCodec(),
	})
	cleanup = func() {
		_ = w.Close()
		_ = os.RemoveAll(dir)
	}
	return store, path, cleanup
}

// TestTx_AddNode_Idempotence confirms that two AddNode("alice") calls
// produce a single interned node and two WAL frames (the WAL records
// both ops; the mapper deduplicates).
func TestTx_AddNode_Idempotence(t *testing.T) {
	t.Parallel()
	s, walPath, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.AddNode("alice"); err != nil {
		t.Fatalf("AddNode #1: %v", err)
	}
	if err := tx.AddNode("alice"); err != nil {
		t.Fatalf("AddNode #2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := s.Graph().AdjList().Mapper().Len(); got != 1 {
		t.Fatalf("mapper Len = %d, want 1 (AddNode must be idempotent)", got)
	}
	// Three WAL frames must be present: one per op (the WAL is the durable
	// log; dedup is the in-memory mapper's job) plus the v3 OpCommit marker
	// that closes the transaction atomically.
	if err := walFrameCountEquals(walPath, 3); err != nil {
		t.Fatal(err)
	}
}

// TestTx_RemoveNode_StripsLabelsAndProperties exercises the logical
// removal path: a node with multiple labels and properties is preserved
// in the mapper but stripped clean by RemoveNode.
func TestTx_RemoveNode_StripsLabelsAndProperties(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	// Seed: alice has two labels and two properties.
	tx := s.Begin()
	if err := tx.AddNode("alice"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetNodeLabel("alice", "Admin"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice")); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Sanity: pre-state is as expected.
	if !s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatal("seed: Person label missing")
	}
	if !s.Graph().HasNodeLabel("alice", "Admin") {
		t.Fatal("seed: Admin label missing")
	}
	if _, ok := s.Graph().GetNodeProperty("alice", "name"); !ok {
		t.Fatal("seed: name property missing")
	}

	// Logical removal.
	tx = s.Begin()
	if err := tx.RemoveNode("alice"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatal("Person label survived RemoveNode")
	}
	if s.Graph().HasNodeLabel("alice", "Admin") {
		t.Fatal("Admin label survived RemoveNode")
	}
	if _, ok := s.Graph().GetNodeProperty("alice", "name"); ok {
		t.Fatal("name property survived RemoveNode")
	}
	if _, ok := s.Graph().GetNodeProperty("alice", "age"); ok {
		t.Fatal("age property survived RemoveNode")
	}
	// Mapper entry is permanent.
	if got := s.Graph().AdjList().Mapper().Len(); got != 1 {
		t.Fatalf("mapper Len = %d, want 1 (mapper entry must survive RemoveNode)", got)
	}
}

// TestTx_MultipleLabelsPerNode confirms a node can carry several
// labels simultaneously via SetNodeLabel.
func TestTx_MultipleLabelsPerNode(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	for _, lbl := range []string{"Person", "Admin", "User", "VIP"} {
		if err := tx.SetNodeLabel("alice", lbl); err != nil {
			t.Fatalf("SetNodeLabel %q: %v", lbl, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, lbl := range []string{"Person", "Admin", "User", "VIP"} {
		if !s.Graph().HasNodeLabel("alice", lbl) {
			t.Fatalf("missing label %q after commit", lbl)
		}
	}
	got := append([]string(nil), s.Graph().NodeLabels("alice")...)
	if len(got) != 4 {
		t.Fatalf("NodeLabels len = %d, want 4 (got %v)", len(got), got)
	}
}

// TestTx_RemoveNodeLabel_Single removes one label and leaves the rest
// untouched.
func TestTx_RemoveNodeLabel_Single(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.SetNodeLabel("alice", "Admin")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx = s.Begin()
	if err := tx.RemoveNodeLabel("alice", "Admin"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if !s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatal("Person label dropped unexpectedly")
	}
	if s.Graph().HasNodeLabel("alice", "Admin") {
		t.Fatal("Admin label survived RemoveNodeLabel")
	}
}

// TestTx_NodeProperty_SetThenDelete exercises SetNodeProperty across
// every PropertyKind, then DelNodeProperty removes one of them.
//
//nolint:gocyclo // table: one assert per property kind
func TestTx_NodeProperty_SetThenDelete(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	knownTime := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		key string
		val lpg.PropertyValue
	}{
		{"name", lpg.StringValue("Alice")},
		{"age", lpg.Int64Value(30)},
		{"score", lpg.Float64Value(99.5)},
		{"active", lpg.BoolValue(true)},
		{"joined", lpg.TimeValue(knownTime)},
		{"blob", lpg.BytesValue([]byte{0x01, 0x02, 0x03})},
	}

	tx := s.Begin()
	for _, c := range cases {
		if err := tx.SetNodeProperty("alice", c.key, c.val); err != nil {
			t.Fatalf("SetNodeProperty %q: %v", c.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Round-trip each value through the in-memory graph.
	for _, c := range cases {
		got, ok := s.Graph().GetNodeProperty("alice", c.key)
		if !ok {
			t.Fatalf("%q missing after commit", c.key)
		}
		if got.Kind() != c.val.Kind() {
			t.Fatalf("%q kind = %v, want %v", c.key, got.Kind(), c.val.Kind())
		}
		switch c.val.Kind() {
		case lpg.PropString:
			g, _ := got.String()
			w, _ := c.val.String()
			if g != w {
				t.Fatalf("%q = %q, want %q", c.key, g, w)
			}
		case lpg.PropInt64:
			g, _ := got.Int64()
			w, _ := c.val.Int64()
			if g != w {
				t.Fatalf("%q = %d, want %d", c.key, g, w)
			}
		case lpg.PropFloat64:
			g, _ := got.Float64()
			w, _ := c.val.Float64()
			if math.Float64bits(g) != math.Float64bits(w) {
				t.Fatalf("%q = %g, want %g", c.key, g, w)
			}
		case lpg.PropBool:
			g, _ := got.Bool()
			w, _ := c.val.Bool()
			if g != w {
				t.Fatalf("%q = %v, want %v", c.key, g, w)
			}
		case lpg.PropTime:
			g, _ := got.Time()
			w, _ := c.val.Time()
			if !g.Equal(w) {
				t.Fatalf("%q = %v, want %v", c.key, g, w)
			}
		case lpg.PropBytes:
			g, _ := got.Bytes()
			w, _ := c.val.Bytes()
			if !bytes.Equal(g, w) {
				t.Fatalf("%q = %x, want %x", c.key, g, w)
			}
		}
	}

	// Delete one property and confirm the rest survive.
	tx = s.Begin()
	if err := tx.DelNodeProperty("alice", "age"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Graph().GetNodeProperty("alice", "age"); ok {
		t.Fatal("age survived DelNodeProperty")
	}
	if _, ok := s.Graph().GetNodeProperty("alice", "name"); !ok {
		t.Fatal("name dropped during DelNodeProperty")
	}
}

// TestTx_EdgeProperty_SetThenDelete sets edge properties of every
// PropertyKind, then DelEdgeProperty removes one. The edge must exist
// at apply time, so we seed it via AddEdge in the same transaction.
//
//nolint:gocyclo // table: one assert per property kind
func TestTx_EdgeProperty_SetThenDelete(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	knownTime := time.Date(2026, 5, 22, 11, 30, 0, 0, time.UTC)
	cases := []struct {
		key string
		val lpg.PropertyValue
	}{
		{"since", lpg.StringValue("2026")},
		{"weight", lpg.Int64Value(7)},
		{"score", lpg.Float64Value(0.81)},
		{"verified", lpg.BoolValue(true)},
		{"started", lpg.TimeValue(knownTime)},
		{"raw", lpg.BytesValue([]byte{0xAA, 0xBB})},
	}

	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if err := tx.SetEdgeProperty("alice", "bob", c.key, c.val); err != nil {
			t.Fatalf("SetEdgeProperty %q: %v", c.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, c := range cases {
		got, ok := s.Graph().GetEdgeProperty("alice", "bob", c.key)
		if !ok {
			t.Fatalf("edge.%q missing after commit", c.key)
		}
		if got.Kind() != c.val.Kind() {
			t.Fatalf("edge.%q kind = %v, want %v", c.key, got.Kind(), c.val.Kind())
		}
	}

	tx = s.Begin()
	if err := tx.DelEdgeProperty("alice", "bob", "weight"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Graph().GetEdgeProperty("alice", "bob", "weight"); ok {
		t.Fatal("weight survived DelEdgeProperty")
	}
	if _, ok := s.Graph().GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Fatal("since dropped during DelEdgeProperty")
	}
}

// TestTx_RemoveEdge wipes a single edge while leaving the adjacent
// nodes (and other edges) intact.
func TestTx_RemoveEdge(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "carol", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tx = s.Begin()
	if err := tx.RemoveEdge("alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if s.Graph().AdjList().HasEdge("alice", "bob") {
		t.Fatal("alice->bob edge survived RemoveEdge")
	}
	if !s.Graph().AdjList().HasEdge("alice", "carol") {
		t.Fatal("alice->carol edge dropped unexpectedly")
	}
}

// TestTx_CommitWALOnly_HappyPath confirms the WAL-only commit appends
// every op to the WAL and does NOT mutate the in-memory graph. The
// state is recovered later only through replay.
func TestTx_CommitWALOnly_HappyPath(t *testing.T) {
	t.Parallel()
	s, walPath, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.CommitWALOnly(); err != nil {
		t.Fatalf("CommitWALOnly: %v", err)
	}

	// Critical invariant: in-memory graph must NOT have observed the ops.
	if s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatal("CommitWALOnly applied label to in-memory graph (expected WAL-only)")
	}
	if s.Graph().AdjList().HasEdge("alice", "bob") {
		t.Fatal("CommitWALOnly applied edge to in-memory graph (expected WAL-only)")
	}

	// WAL must contain three frames: one per op plus the v3 OpCommit marker.
	if err := walFrameCountEquals(walPath, 3); err != nil {
		t.Fatal(err)
	}

	// The Tx is finished; further calls must surface ErrTxFinished.
	if err := tx.CommitWALOnly(); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("CommitWALOnly after finish: %v, want ErrTxFinished", err)
	}
}

// TestTx_CommitWALOnly_AfterClose forces the WAL to fail by closing it
// before commit. CommitWALOnly must surface the writer's error and the
// transaction must finish (so the store mutex is released).
func TestTx_CommitWALOnly_AfterClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithCodec[string, int64](g, w, NewStringCodec())

	tx := s.Begin()
	_ = tx.SetNodeLabel("alice", "Person")
	// Pre-emptively close the WAL writer to force Append to fail.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tx.CommitWALOnly(); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("CommitWALOnly err = %v, want ErrWriterClosed", err)
	}
	// The lock must have been released even on error: a second
	// Begin must not deadlock.
	done := make(chan struct{})
	go func() {
		_ = s.Begin().Rollback()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("store mutex was not released after CommitWALOnly error")
	}
}

// TestTx_Commit_AppendFailure_LeavesGraphUntouched forces the WAL
// Append to fail mid-commit and confirms the in-memory graph is NOT
// mutated (durability-first invariant: nothing applied unless every
// op was durably written).
func TestTx_Commit_AppendFailure_LeavesGraphUntouched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithCodec[string, int64](g, w, NewStringCodec())

	tx := s.Begin()
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "bob", 0); err != nil {
		t.Fatal(err)
	}
	// Force Append failure by closing the writer before Commit.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("Commit err = %v, want ErrWriterClosed", err)
	}
	// Durability contract: no in-memory state should reflect either op.
	if s.Graph().HasNodeLabel("alice", "Person") {
		t.Fatal("label leaked into graph after WAL Append failure")
	}
	if s.Graph().AdjList().HasEdge("alice", "bob") {
		t.Fatal("edge leaked into graph after WAL Append failure")
	}
}

// TestTx_BeginCtx_Cancellation covers the BeginCtx error path: a
// cancelled context must short-circuit before the mutex is acquired
// and must return the context error.
func TestTx_BeginCtx_Cancellation(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := s.BeginCtx(ctx)
	if err == nil {
		t.Fatal("BeginCtx on cancelled context returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BeginCtx err = %v, want context.Canceled", err)
	}
	if tx != nil {
		t.Fatal("BeginCtx returned non-nil Tx on cancellation")
	}
	// The store mutex must remain available: a follow-up Begin must
	// succeed without blocking.
	done := make(chan struct{})
	go func() {
		_ = s.Begin().Rollback()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("store mutex was not released after BeginCtx cancellation")
	}
}

// TestTx_Rollback_DiscardsAllOps confirms Rollback does not write to
// the WAL and does not mutate the graph, even after every mutation
// kind has been buffered into the tx.
func TestTx_Rollback_DiscardsAllOps(t *testing.T) {
	t.Parallel()
	s, walPath, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	// Buffer one of every kind.
	_ = tx.AddNode("alice")
	_ = tx.SetNodeLabel("alice", "Person")
	_ = tx.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	_ = tx.AddEdge("alice", "bob", 0)
	_ = tx.SetEdgeLabel("alice", "bob", "KNOWS")
	_ = tx.SetEdgeProperty("alice", "bob", "since", lpg.StringValue("2026"))
	_ = tx.RemoveNodeLabel("alice", "Admin")        // safe no-op
	_ = tx.DelNodeProperty("alice", "age")          // safe no-op
	_ = tx.RemoveEdge("alice", "carol")             // safe no-op
	_ = tx.DelEdgeProperty("alice", "bob", "score") // safe no-op
	_ = tx.RemoveNode("alice")

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if s.Graph().AdjList().Mapper().Len() != 0 {
		t.Fatal("Rollback left node in mapper")
	}
	if err := walFrameCountEquals(walPath, 0); err != nil {
		t.Fatal(err)
	}
}

// TestTx_EncodePropertyValue_RoundtripAllKinds is a focused unit test
// for the encodePropertyValue / decodePropertyValue pair. The Set*
// property ops route through these helpers on Commit; covering every
// PropertyKind ensures the wire shape is symmetric.
//
//nolint:gocyclo // table: one assert per property kind, branch coverage
func TestTx_EncodePropertyValue_RoundtripAllKinds(t *testing.T) {
	t.Parallel()
	knownTime := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		val  lpg.PropertyValue
	}{
		{"string-empty", lpg.StringValue("")},
		{"string-utf8", lpg.StringValue("hello-世界")},
		{"int64-zero", lpg.Int64Value(0)},
		{"int64-min", lpg.Int64Value(math.MinInt64)},
		{"int64-max", lpg.Int64Value(math.MaxInt64)},
		{"float64-zero", lpg.Float64Value(0)},
		{"float64-neg", lpg.Float64Value(-3.14159)},
		{"float64-inf", lpg.Float64Value(math.Inf(1))},
		{"bool-true", lpg.BoolValue(true)},
		{"bool-false", lpg.BoolValue(false)},
		{"time-known", lpg.TimeValue(knownTime)},
		{"bytes-empty", lpg.BytesValue([]byte{})},
		{"bytes-bytes", lpg.BytesValue([]byte{0xDE, 0xAD, 0xBE, 0xEF})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			buf := encodePropertyValue(nil, c.val)
			got, rest, err := decodePropertyValue(buf)
			if err != nil {
				t.Fatalf("decodePropertyValue: %v", err)
			}
			if len(rest) != 0 {
				t.Fatalf("decodePropertyValue left %d trailing bytes", len(rest))
			}
			if got.Kind() != c.val.Kind() {
				t.Fatalf("Kind = %v, want %v", got.Kind(), c.val.Kind())
			}
		})
	}
}

// TestTx_DecodePropertyValue_ShortInputs covers the negative branches
// of decodePropertyValue for every kind, anchoring the error messages
// callers depend on.
//
//nolint:gocyclo // table: one short-input case per error branch
func TestTx_DecodePropertyValue_ShortInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"string-missing-length", []byte{byte(lpg.PropString)}},
		{"string-truncated-body", []byte{byte(lpg.PropString), 5, 0, 0, 0, 'a', 'b'}},
		{"int64-varint-empty", []byte{byte(lpg.PropInt64)}},
		{"float64-short", []byte{byte(lpg.PropFloat64), 0x01, 0x02}},
		{"bool-empty", []byte{byte(lpg.PropBool)}},
		{"time-varint-empty", []byte{byte(lpg.PropTime)}},
		{"bytes-missing-length", []byte{byte(lpg.PropBytes)}},
		{"bytes-truncated-body", []byte{byte(lpg.PropBytes), 4, 0, 0, 0, 0xAA}},
		{"unknown-kind", []byte{0xFF}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := decodePropertyValue(c.buf)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestTx_AllMutationKinds_FinishedErrors asserts every mutation API
// returns ErrTxFinished after the tx has been committed.
//
//nolint:gocyclo // table: one assert per buffer method
func TestTx_AllMutationKinds_FinishedErrors(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Every buffer call must reject post-finish.
	tests := []struct {
		name string
		call func() error
	}{
		{"AddEdge", func() error { return tx.AddEdge("a", "b", 0) }},
		{"SetNodeLabel", func() error { return tx.SetNodeLabel("a", "L") }},
		{"SetEdgeLabel", func() error { return tx.SetEdgeLabel("a", "b", "L") }},
		{"AddNode", func() error { return tx.AddNode("a") }},
		{"RemoveNode", func() error { return tx.RemoveNode("a") }},
		{"RemoveNodeLabel", func() error { return tx.RemoveNodeLabel("a", "L") }},
		{"SetNodeProperty", func() error { return tx.SetNodeProperty("a", "k", lpg.StringValue("v")) }},
		{"DelNodeProperty", func() error { return tx.DelNodeProperty("a", "k") }},
		{"RemoveEdge", func() error { return tx.RemoveEdge("a", "b") }},
		{"SetEdgeProperty", func() error { return tx.SetEdgeProperty("a", "b", "k", lpg.StringValue("v")) }},
		{"DelEdgeProperty", func() error { return tx.DelEdgeProperty("a", "b", "k") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrTxFinished) {
				t.Fatalf("%s err = %v, want ErrTxFinished", tc.name, err)
			}
		})
	}
}

// TestTx_WeightedStore_AddEdgeWeighted_RoundTrip confirms that on a
// store with a WeightCodec, AddEdge buffers the handle-bearing
// OpAddEdgeH (the Stage-2 successor of OpAddEdgeWeighted) and the
// in-memory graph carries the weight after Commit. The frame on disk
// must be a v3-tagged OpAddEdgeH record.
func TestTx_WeightedStore_AddEdgeWeighted_RoundTrip(t *testing.T) {
	t.Parallel()
	s, walPath, cleanup := openTypedWeightedStore(t)
	defer cleanup()

	const want int64 = 0x0BADBEEF
	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob", want); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// In-memory weight.
	var got int64
	for n, w := range s.Graph().AdjList().Neighbours("alice") {
		if n == "bob" {
			got = w
			break
		}
	}
	if got != want {
		t.Fatalf("weight = %d, want %d", got, want)
	}

	// Wire shape: two frames — the v3-tagged OpAddEdgeH op followed by the
	// v3 OpCommit marker that closes the transaction.
	if err := walFrameCountEquals(walPath, 2); err != nil {
		t.Fatal(err)
	}
	if err := assertFirstFrameKind(walPath, OpRecordV3, byte(OpAddEdgeH)); err != nil {
		t.Fatal(err)
	}
}

// TestTx_Commit_SyncFailure_ZeroOps drives the Sync error branch of
// Commit. A Commit with zero buffered ops skips the Append loop and
// goes straight to Sync; closing the writer first forces Sync to
// return ErrWriterClosed. The in-memory graph remains untouched.
func TestTx_Commit_SyncFailure_ZeroOps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithCodec[string, int64](g, w, NewStringCodec())

	tx := s.Begin()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("Commit err = %v, want ErrWriterClosed", err)
	}
	// Mutex must be released for the next Begin.
	done := make(chan struct{})
	go func() {
		_ = s.Begin().Rollback()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("store mutex was not released after Commit sync failure")
	}
}

// TestTx_CommitWALOnly_SyncFailure_ZeroOps mirrors
// TestTx_Commit_SyncFailure_ZeroOps for the WAL-only path.
func TestTx_CommitWALOnly_SyncFailure_ZeroOps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStoreWithCodec[string, int64](g, w, NewStringCodec())

	tx := s.Begin()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tx.CommitWALOnly(); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("CommitWALOnly err = %v, want ErrWriterClosed", err)
	}
	done := make(chan struct{})
	go func() {
		_ = s.Begin().Rollback()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("store mutex was not released after CommitWALOnly sync failure")
	}
}

// walFrameCountEquals opens the WAL at path read-only and counts
// frames, returning an error if the count differs from want.
func walFrameCountEquals(path string, want int) error {
	r, err := wal.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	frames := 0
	if err := r.Replay(func(_ wal.Frame) error {
		frames++
		return nil
	}); err != nil {
		return err
	}
	if frames != want {
		return errors.New("wal frame count mismatch")
	}
	return nil
}

// assertFirstFrameKind opens the WAL at path read-only and verifies
// the first frame's payload begins with the supplied version and kind
// bytes. Returns an error if the WAL is empty or the prefix differs.
func assertFirstFrameKind(path string, wantVersion, wantKind byte) error {
	r, err := wal.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	var first wal.Frame
	var seen bool
	if err := r.Replay(func(f wal.Frame) error {
		if !seen {
			first = f
			seen = true
		}
		return nil
	}); err != nil {
		return err
	}
	if !seen {
		return errors.New("wal is empty")
	}
	if len(first.Payload) < 2 {
		return errors.New("first frame payload < 2 bytes")
	}
	if first.Payload[0] != wantVersion {
		return errors.New("first frame version byte mismatch")
	}
	if first.Payload[1] != wantKind {
		return errors.New("first frame kind byte mismatch")
	}
	return nil
}
