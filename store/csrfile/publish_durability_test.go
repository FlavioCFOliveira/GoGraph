package csrfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// buildPathCSRN builds a deterministic directed path 0→1→…→(n-1) with
// edge i carrying weight i, as a CSR. Both the in-process ordering test
// and the cross-process child use it so the published artefact is
// byte-identical across the two call sites. (buildPathCSR — fixed at 20
// nodes — already exists for the determinism test; this variant takes a
// size and string node keys.)
func buildPathCSRN(n int) (*csr.CSR[int64], error) {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for i := 0; i < n-1; i++ {
		if err := a.AddEdge(strconv.Itoa(i), strconv.Itoa(i+1), int64(i)); err != nil {
			return nil, err
		}
	}
	return csr.BuildFromAdjList(a), nil
}

func init() {
	// Register the child handler for the clean-exit cross-process
	// recovery test. The child bulk-publishes a deterministic directed
	// path into a csrfile via WriteToFile (the bulk loader's sole
	// durability mechanism), prints a golden "order=<n> size=<m>" line,
	// and exits 0. No crash injection: the point is that an
	// acknowledged, cleanly-exited publish is loadable by an independent
	// process — which holds only because WriteToFile fsyncs the parent
	// directory after the rename.
	subproc.Register("csrfile-bulk-publish", func(args []string) int {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "csrfile-bulk-publish: missing path/n args")
			return 1
		}
		path := args[0]
		n, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "csrfile-bulk-publish: bad n %q: %v\n", args[1], err)
			return 1
		}

		c, err := buildPathCSRN(n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "csrfile-bulk-publish: buildPathCSRN: %v\n", err)
			return 1
		}
		h, err := WriteToFile[int64](path, c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "csrfile-bulk-publish: WriteToFile: %v\n", err)
			return 1
		}

		// Golden summary the parent reconciles against. NVertices is the
		// padded vertex count the header records; NEdges is the live edge
		// count. Both must survive the cross-process boundary intact. The
		// sorted src→dst pairs (derived from the SAME CSR offset/edge
		// arrays that WriteToFile serialised) are the faithfulness oracle:
		// the internal node IDs assigned to the string keys are
		// hash-derived and non-contiguous, so the parent cannot assume a
		// 0→1→…→n-1 topology — it can only re-derive the identical pair set
		// from the opened file and compare.
		pairs := csrEdgePairs(c.VerticesSlice(), c.EdgesSlice())
		fmt.Printf("nvertices=%d nedges=%d\n", h.NVertices, h.NEdges)
		for _, p := range pairs {
			fmt.Printf("edge=%s\n", p)
		}
		return 0
	})
}

// TestWriteToFile_PublishOrdering is the deterministic ordering proxy
// that pins the durability fix WITHOUT crash injection. It installs a
// publish-trace recorder, writes a csrfile, and asserts that WriteToFile
// emits exactly the canonical crash-safe sequence:
//
//	rename -> parent-fsync
//
// The "parent-fsync" event fires only when parentDirFsync(path) is
// invoked after the os.Rename. Removing the parentDirFsync call (the
// pre-fix state, where the temp file is fsynced and renamed but the
// parent directory is never fsynced) drops that event, so the recorded
// sequence becomes just ["rename"] and this test FAILS. With the fix in
// place the sequence is ["rename", "parent-fsync"] and the test PASSES.
// This is the real fail-before / pass-after regression guard for the
// bulk-loader durability hole.
//
// It must not run in parallel: publishTraceHook is process-global and a
// concurrent WriteToFile in another test would record into this
// recorder.
func TestWriteToFile_PublishOrdering(t *testing.T) {
	c, err := buildPathCSRN(16)
	if err != nil {
		t.Fatalf("buildPathCSRN: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ordering.csr")

	var got []string
	rec := func(event, p string) {
		// Defend against any foreign event for a different path (there is
		// none while this serial test owns the hook, but be explicit).
		if p == path {
			got = append(got, event)
		}
	}
	publishTraceHook.Store(&rec)
	t.Cleanup(func() { publishTraceHook.Store(nil) })

	if _, err := WriteToFile[int64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	want := []string{"rename", "parent-fsync"}
	if len(got) != len(want) {
		t.Fatalf("publish step sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("publish step[%d] = %q, want %q (full sequence %v, want %v)",
				i, got[i], want[i], got, want)
		}
	}

	// The parent-fsync step must come strictly after the rename: a writer
	// that fsynced the directory before publishing would not make the new
	// dirent durable.
	if got[len(got)-1] != "parent-fsync" {
		t.Fatalf("last publish step = %q, want the parent-dir fsync to be last", got[len(got)-1])
	}
}

// TestWriteToFile_CrossProc_PublishRecoverable is the clean-exit
// cross-process recovery guard. A child process (proc A) bulk-publishes
// a deterministic directed path via WriteToFile and exits 0; the parent
// (proc B) opens the published file with csrfile.Open and asserts the
// full graph is present and faithful (vertex count, edge count, and
// every edge of the path).
//
// Crossing an OS process boundary is what gives the test teeth: proc A's
// in-memory page cache and open file descriptors are gone by the time
// proc B opens the file, so proc B can only succeed if the publish was
// flushed to stable storage — header + payload via the temp-file fsync,
// and the rename's directory entry via the post-rename parent-dir fsync.
// Together with TestWriteToFile_PublishOrdering (which proves the
// fsync-after-rename ordering), this pins the durability fix without a
// crashpoint. The literal SIGKILL-at-breakpoint variant is deferred to
// task #1313 (build-tag gating of internal/crashpoint).
func TestWriteToFile_CrossProc_PublishRecoverable(t *testing.T) {
	t.Parallel()

	const n = 30
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.csr")

	stdout, stderr, err := subproc.Run(t, "csrfile-bulk-publish", path, strconv.Itoa(n))
	if err != nil {
		t.Fatalf("proc A failed: %v\nstderr: %s", err, stderr)
	}

	// Parse proc A's golden summary: the header counts plus the sorted
	// edge-pair oracle.
	var goldenNV, goldenNE uint64
	var goldenEdges []string
	for _, line := range splitLines(string(stdout)) {
		switch {
		case strings.HasPrefix(line, "nvertices="):
			if _, scanErr := fmt.Sscanf(line, "nvertices=%d nedges=%d", &goldenNV, &goldenNE); scanErr != nil {
				t.Fatalf("could not parse summary line %q: %v", line, scanErr)
			}
		case strings.HasPrefix(line, "edge="):
			goldenEdges = append(goldenEdges, strings.TrimPrefix(line, "edge="))
		default:
			t.Fatalf("unexpected proc A line %q (full stdout %q)", line, stdout)
		}
	}
	if goldenNE != uint64(n-1) {
		t.Fatalf("proc A reported nedges=%d, want %d", goldenNE, n-1)
	}
	if uint64(len(goldenEdges)) != goldenNE {
		t.Fatalf("proc A emitted %d edge pairs, want %d", len(goldenEdges), goldenNE)
	}

	// Proc B: open the file proc A published — this only succeeds if the
	// header, payload, and the rename's directory entry are all durable.
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open published file: %v", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	if got := r.Header().NVertices; got != goldenNV {
		t.Errorf("NVertices = %d, want %d (from proc A)", got, goldenNV)
	}
	if got := r.Header().NEdges; got != goldenNE {
		t.Errorf("NEdges = %d, want %d (from proc A)", got, goldenNE)
	}

	// Re-derive the edge-pair set from the opened file and assert it is
	// byte-for-byte identical to proc A's. This proves the FULL graph
	// (every src→dst edge), not merely the counts, survived the publish
	// across the process boundary — independent of the internal node-ID
	// assignment, which is hash-derived and non-contiguous.
	gotEdges := csrEdgePairs(r.Vertices(), r.Edges())
	if len(gotEdges) != len(goldenEdges) {
		t.Fatalf("recovered %d edge pairs, want %d", len(gotEdges), len(goldenEdges))
	}
	for i := range goldenEdges {
		if gotEdges[i] != goldenEdges[i] {
			t.Fatalf("edge pair[%d] = %q, want %q", i, gotEdges[i], goldenEdges[i])
		}
	}
}

// csrEdgePairs walks a CSR vertices-offset array and edges array and
// returns every edge as a sorted "src->dst" string. The vertices slice
// is shard-padded (its length is the padded MaxNodeID, not the live node
// count), so trailing equal offsets simply yield no edges. Sorting makes
// the result a stable, storage-order-independent oracle that proc A and
// proc B can compare across the process boundary.
func csrEdgePairs(verts []uint64, edges []graph.NodeID) []string {
	pairs := make([]string, 0, len(edges))
	for v := 0; v < len(verts); v++ {
		start := verts[v]
		var end uint64
		if v+1 < len(verts) {
			end = verts[v+1]
		} else {
			end = uint64(len(edges))
		}
		for i := start; i < end; i++ {
			pairs = append(pairs, fmt.Sprintf("%d->%d", v, uint64(edges[i])))
		}
	}
	sort.Strings(pairs)
	return pairs
}
