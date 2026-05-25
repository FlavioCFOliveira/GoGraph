package bulk

import (
	"os"
	"path/filepath"
	"testing"

	"gograph/store/wal"
)

// TestLoader_BypassesWAL verifies that the bulk loader writes only to
// the csrfile output and does not append any frames to a WAL opened
// independently in the same directory. The WAL file size must remain
// at its initial value (no frames written) after Finalise.
func TestLoader_BypassesWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// Open a WAL in the same directory. The bulk loader is never given
	// this handle — it writes directly to csrfile.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Confirm the WAL file exists on disk at this point.
	pre, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL before bulk: %v", err)
	}
	preSize := pre.Size()

	outPath := filepath.Join(dir, "graph.csr")
	l := New(Options{OutputPath: outPath, Directed: true})
	edges := []Edge{
		{Src: "a", Dst: "b", Weight: 1},
		{Src: "b", Dst: "c", Weight: 2},
		{Src: "c", Dst: "d", Weight: 3},
		{Src: "d", Dst: "e", Weight: 4},
		{Src: "e", Dst: "a", Weight: 5},
		{Src: "a", Dst: "c", Weight: 6},
		{Src: "b", Dst: "d", Weight: 7},
		{Src: "c", Dst: "e", Weight: 8},
		{Src: "d", Dst: "a", Weight: 9},
		{Src: "e", Dst: "b", Weight: 10},
	}
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if _, _, err := l.Finalise(); err != nil {
		t.Fatalf("Finalise: %v", err)
	}

	// The WAL file must not have grown — bulk loader does not write WAL frames.
	post, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL after bulk: %v", err)
	}
	if post.Size() != preSize {
		t.Fatalf("WAL size changed from %d to %d; bulk loader must not write WAL frames",
			preSize, post.Size())
	}

	// The csrfile must exist and be non-empty.
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("csrfile not created: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("csrfile is empty")
	}
}
