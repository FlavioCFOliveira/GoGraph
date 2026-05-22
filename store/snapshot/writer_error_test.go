package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// errWriter rejects every Write with the configured error.
type errWriter struct{ err error }

func (e errWriter) Write(_ []byte) (int, error) { return 0, e.err }

// partialWriter accepts the first n bytes and then errors on every
// subsequent Write. n=0 means error immediately.
type partialWriter struct {
	n   int
	err error
}

func (p *partialWriter) Write(b []byte) (int, error) {
	if p.n <= 0 {
		return 0, p.err
	}
	if len(b) <= p.n {
		p.n -= len(b)
		return len(b), nil
	}
	taken := p.n
	p.n = 0
	return taken, p.err
}

func TestWriteCSR_PropagatesWriterErrors(t *testing.T) {
	t.Parallel()
	// Build a CSR large enough to overflow the writer's internal
	// bufio (1 MB), forcing the buffer to flush through the underlying
	// writer at least once during the vertex/edge/weight write
	// sequence. Without this, WriteCSR's small writes are absorbed by
	// the buffer and the error is only surfaced at the final Flush.
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	const n = 130_000
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, int64(i)); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)

	sentinel := errors.New("synthetic write fault")
	cases := []struct {
		name       string
		makeWriter func() io.Writer
	}{
		{
			name: "immediate error",
			makeWriter: func() io.Writer {
				return errWriter{err: sentinel}
			},
		},
		{
			name: "errors after one 1 MB chunk",
			makeWriter: func() io.Writer {
				return &partialWriter{n: 1 << 20, err: sentinel}
			},
		},
		{
			name: "errors after two 1 MB chunks",
			makeWriter: func() io.Writer {
				return &partialWriter{n: 2 << 20, err: sentinel}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := WriteCSR(tc.makeWriter(), c)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestReadCSR_PropagatesReaderErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "only seven bytes of vertex count", data: make([]byte, 7)},
		{
			name: "vertex count read but no edge count",
			data: make([]byte, 12), // 8 bytes nV ok, 4 of 8 bytes for nE
		},
		{
			name: "header without flag bytes",
			data: make([]byte, 16), // nV ok, nE ok, but no (hasW, wsize)
		},
		{
			name: "header claims 5 vertices but supplies none",
			data: []byte{
				5, 0, 0, 0, 0, 0, 0, 0, // nV = 5
				0, 0, 0, 0, 0, 0, 0, 0, // nE = 0
				0, 0, // hasW, wsize
			},
		},
		{
			name: "verts ok but no edges payload",
			data: []byte{
				0, 0, 0, 0, 0, 0, 0, 0, // nV = 0
				5, 0, 0, 0, 0, 0, 0, 0, // nE = 5 (need 40 more bytes)
				0, 0, // hasW=0, wsize=0
			},
		},
		{
			name: "weighted edges declared but no weight bytes supplied",
			data: []byte{
				0, 0, 0, 0, 0, 0, 0, 0, // nV = 0
				1, 0, 0, 0, 0, 0, 0, 0, // nE = 1
				1, 8, // hasW = 1, wsize = 8 (int64)
				7, 0, 0, 0, 0, 0, 0, 0, // single edge target
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadCSR(bytes.NewReader(tc.data)); err == nil {
				t.Fatalf("expected error for input length %d", len(tc.data))
			}
		})
	}
}

// flakyCtx returns nil for the first nilCalls calls to Err(), then a
// fixed error from every subsequent call. Useful to exercise the
// inner ctx.Err() checkpoints of multi-stage helpers without racing
// against the work they perform.
type flakyCtx struct {
	parentCtx context.Context //nolint:containedctx // test fake on purpose
	nilCalls  int
	err       error
}

func (f *flakyCtx) Deadline() (time.Time, bool) { return f.parentCtx.Deadline() }
func (f *flakyCtx) Done() <-chan struct{}       { return f.parentCtx.Done() }
func (f *flakyCtx) Value(k any) any             { return f.parentCtx.Value(k) }
func (f *flakyCtx) Err() error {
	if f.nilCalls > 0 {
		f.nilCalls--
		return nil
	}
	return f.err
}

func TestWriteSnapshotCSR_ContextCancelledAfterFirstCheckpoint(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	sentinel := errors.New("simulated mid-publish cancellation")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		nilCalls:  1, // first Err() returns nil; subsequent calls return sentinel
		err:       sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSRCtx(ctx, dir, c); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotCSRCtx with flaky ctx = %v, want %v", err, sentinel)
	}
	// The publish must have rolled back: no published directory.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("published dir should not exist after mid-publish cancel, stat err = %v", err)
	}
	// And the staging dir must also be cleaned.
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging dir should be cleaned, stat err = %v", err)
	}
}

func TestWriteSnapshotCSR_ContextCancelledBeforeRename(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	sentinel := errors.New("cancel right before rename")
	ctx := &flakyCtx{
		parentCtx: context.Background(),
		nilCalls:  2, // first two Err()s return nil; the third (pre-rename) trips
		err:       sentinel,
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSRCtx(ctx, dir, c); !errors.Is(err, sentinel) {
		t.Fatalf("WriteSnapshotCSRCtx pre-rename cancel = %v, want %v", err, sentinel)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("published dir should not exist after pre-rename cancel, stat err = %v", err)
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging dir should be cleaned after pre-rename cancel, stat err = %v", err)
	}
}

func TestWriteSnapshotCSR_ContextPreCancelled(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSRCtx(ctx, dir, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteSnapshotCSRCtx with cancelled ctx = %v, want context.Canceled", err)
	}
	// No published directory must exist after a pre-cancel.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("published dir should not exist after pre-cancel, stat err = %v", err)
	}
}

func TestWriteSnapshotCSR_ParentIsRegularFile(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	// Plant a regular file where MkdirAll(filepath.Dir(dir), ...) would
	// try to create the parent directory: that turns MkdirAll into an
	// error, exercising the failure branch of WriteSnapshotCSRCtx.
	tmp := t.TempDir()
	notDir := filepath.Join(tmp, "imposter")
	if err := os.WriteFile(notDir, []byte("not a dir"), 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	dir := filepath.Join(notDir, "snap")
	if err := WriteSnapshotCSR(dir, c); err == nil {
		t.Fatalf("WriteSnapshotCSR with file-as-parent should error")
	}
}

func TestWriteSnapshotCSR_TmpPathPreexistsAsFile(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	parent := t.TempDir()
	dir := filepath.Join(parent, "snap")
	// Pre-create the staging .tmp path as a regular file (not a dir),
	// which forces the os.MkdirAll(tmp, ...) inside WriteSnapshotCSRCtx
	// to fail — exercising one more error branch. (The preceding
	// os.RemoveAll succeeds even for files.)
	tmp := dir + ".tmp"
	if err := os.WriteFile(tmp, nil, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	// Make the parent directory read-only AFTER planting the file so
	// the publisher's own RemoveAll(tmp) and MkdirAll(tmp) both fail
	// reliably.
	if err := os.Chmod(parent, 0o500); err != nil { //nolint:gosec // intentionally read-only to trigger MkdirAll failure
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(parent, 0o700) }() //nolint:gosec // test cleanup restores write
	if err := WriteSnapshotCSR(dir, c); err == nil {
		t.Fatalf("WriteSnapshotCSR with read-only parent should error")
	}
}

func TestWriteSnapshotCSR_ReplaceExistingDirectory(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	dir := filepath.Join(t.TempDir(), "snap")
	if err := WriteSnapshotCSR(dir, c); err != nil {
		t.Fatal(err)
	}
	// Mutate the live snapshot by appending a stray file then republish.
	stray := filepath.Join(dir, "stray.bin")
	if err := os.WriteFile(stray, []byte("garbage"), 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	a2 := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a2.AddEdge("c", "d", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c2 := csr.BuildFromAdjList(a2)
	if err := WriteSnapshotCSR(dir, c2); err != nil {
		t.Fatalf("second WriteSnapshotCSR: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("stale stray.bin should be gone after atomic replace, stat err=%v", err)
	}
}

func TestCSRWeightSize_ExhaustiveTypes(t *testing.T) {
	t.Parallel()
	if got := csrWeightSize[struct{}](); got != 0 {
		t.Fatalf("struct{} weight size = %d, want 0", got)
	}
	if got := csrWeightSize[int8](); got != 1 {
		t.Fatalf("int8 weight size = %d, want 1", got)
	}
	if got := csrWeightSize[uint16](); got != 2 {
		t.Fatalf("uint16 weight size = %d, want 2", got)
	}
	if got := csrWeightSize[int32](); got != 4 {
		t.Fatalf("int32 weight size = %d, want 4", got)
	}
	if got := csrWeightSize[float32](); got != 4 {
		t.Fatalf("float32 weight size = %d, want 4", got)
	}
	if got := csrWeightSize[int64](); got != 8 {
		t.Fatalf("int64 weight size = %d, want 8", got)
	}
	if got := csrWeightSize[uint64](); got != 8 {
		t.Fatalf("uint64 weight size = %d, want 8", got)
	}
	if got := csrWeightSize[float64](); got != 8 {
		t.Fatalf("float64 weight size = %d, want 8", got)
	}
	// Unsupported (e.g., a struct) returns 0 — the writer will skip
	// the weights section.
	type fancy struct {
		A int
		B string
	}
	if got := csrWeightSize[fancy](); got != 0 {
		t.Fatalf("unsupported W weight size = %d, want 0", got)
	}
}

func TestWriteManifest_PropagatesWriterError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("manifest sink failed")
	err := WriteManifest(errWriter{err: sentinel}, Manifest{Version: ManifestVersion})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WriteManifest = %v, want %v", err, sentinel)
	}
}
