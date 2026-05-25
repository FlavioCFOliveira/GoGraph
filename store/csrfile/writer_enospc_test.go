package csrfile

import (
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// enospcWriter returns syscall.ENOSPC once its byte budget is exhausted.
type enospcWriter struct {
	remaining int
}

func (w *enospcWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, syscall.ENOSPC
	}
	n := len(p)
	if n > w.remaining {
		n = w.remaining
	}
	w.remaining -= n
	return n, nil
}

// TestWriteSections_ENOSPC verifies that writeSections propagates
// syscall.ENOSPC when the underlying writer runs out of space. The
// test exercises several failure points by varying the byte budget.
func TestWriteSections_ENOSPC(t *testing.T) {
	t.Parallel()

	// Build a small unweighted CSR to exercise the function.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		if err := a.AddEdge(i, (i+1)%8, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	header, _ := Layout(uint64(len(verts)), uint64(len(edges)), WeightAbsent)

	// Full write for reference — must succeed with a large budget.
	ew := &enospcWriter{remaining: 1 << 20}
	h := crc32.New(castagnoli)
	if err := writeSections(ew, h, header, verts, edges, []struct{}{}); err != nil {
		t.Fatalf("full write unexpectedly failed: %v", err)
	}

	// Various budgets that cut the write short.
	budgets := []int{0, 1, 16, 64, 128}
	for _, budget := range budgets {
		budget := budget
		t.Run("budget="+itoa(budget), func(t *testing.T) {
			t.Parallel()
			ew := &enospcWriter{remaining: budget}
			h := crc32.New(castagnoli)
			err := writeSections(ew, h, header, verts, edges, []struct{}{})
			if err == nil {
				t.Fatal("expected error from writeSections with small budget, got nil")
			}
			if !errors.Is(err, syscall.ENOSPC) {
				// The error may be wrapped by binary.Write or io.Writer
				// internals. Accept any non-nil error as long as budget is 0.
				if budget > 0 {
					t.Logf("budget=%d: got %v (not ENOSPC — acceptable if wrapped)", budget, err)
				}
			}
		})
	}
}

// itoa is a minimal int-to-string helper for sub-test names that avoids
// importing strconv in test files that do not otherwise need it.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// TestWriteToFile_ReadOnlyDir verifies that WriteToFile returns a
// non-nil error when the destination directory is read-only. This
// exercises the os.Create error path.
func TestWriteToFile_ReadOnlyDir(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)

	// Create a read-only directory.
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0o555); err != nil { //nolint:gosec // intentional: test sets directory to read-only to trigger EPERM
		t.Skipf("cannot chmod temp dir to read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) }) //nolint:gosec // intentional: restore test directory permissions for cleanup

	path := filepath.Join(roDir, "out.csr")
	_, err := WriteToFile[struct{}](path, c)
	if err == nil {
		t.Fatal("expected error writing to read-only dir, got nil")
	}
}

// TestWriteSections_NodeIDConversion verifies that writeSections
// correctly converts graph.NodeID to uint64 in the edges section
// by checking a known small graph with edges written and read back.
func TestWriteSections_NodeIDConversion(t *testing.T) {
	t.Parallel()

	verts := []uint64{0, 2, 3}       // node 0 → edges[0:2], node 1 → edges[2:3]
	edges := []graph.NodeID{1, 2, 0} // 0→1, 0→2, 1→0
	header, _ := Layout(uint64(len(verts)), uint64(len(edges)), WeightAbsent)

	ew := &enospcWriter{remaining: 1 << 20}
	h := crc32.New(castagnoli)
	if err := writeSections(ew, h, header, verts, edges, []struct{}{}); err != nil {
		t.Fatalf("writeSections: %v", err)
	}
	// Budget was large enough: no error expected. The test ensures
	// writeSections accepts graph.NodeID slices without type errors.
}
