package csrfile

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

func init() {
	// Register the child handler that opens a csrfile, iterates all
	// vertices in ascending order, and prints each vertex's neighbour
	// list as "<nodeID>:<comma-separated neighbour IDs>\n". Both proc A
	// and proc B use this same handler; the parent spawns them with the
	// same csrfile path and compares their stdout line-by-line.
	subproc.Register("csrfile-mmap-readers", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "csrfile-mmap-readers: missing path arg")
			return 1
		}
		path := args[0]

		r, err := Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "csrfile-mmap-readers: Open: %v\n", err)
			return 1
		}
		defer func() {
			if cerr := r.Close(); cerr != nil {
				fmt.Fprintf(os.Stderr, "csrfile-mmap-readers: Close: %v\n", cerr)
			}
		}()

		verts := r.Vertices()
		edges := r.Edges()
		n := len(verts)
		if n == 0 {
			// Empty graph — nothing to print.
			return 0
		}

		for v := 0; v < n; v++ {
			var start, end uint64
			start = verts[v]
			if v+1 < n {
				end = verts[v+1]
			} else {
				end = uint64(len(edges))
			}

			neighbours := make([]graph.NodeID, 0, end-start)
			for i := start; i < end; i++ {
				neighbours = append(neighbours, edges[i])
			}
			// Sort neighbours so output is deterministic regardless of
			// internal storage order.
			slices.Sort(neighbours)

			strs := make([]string, len(neighbours))
			for i, nb := range neighbours {
				strs[i] = strconv.FormatUint(uint64(nb), 10)
			}
			fmt.Printf("%d:%s\n", v, strings.Join(strs, ","))
		}
		return 0
	})
}

// TestCSRFile_CrossProc_MmapNeighbours writes a csrfile from a
// deterministic BarabasiAlbert(200, 3, 42) graph, then spawns two
// independent child processes that each mmap-open the file and print
// every vertex's neighbour list. The test asserts that both outputs
// are identical, verifying that mmap-read consistency is preserved
// across process boundaries.
//
// Two children are used (instead of one) to rule out lucky single-run
// coincidences and to confirm that concurrent mmap readers observe the
// same bytes.
func TestCSRFile_CrossProc_MmapNeighbours(t *testing.T) {
	t.Parallel()

	// Build the csrfile.
	shape := shapegen.BarabasiAlbert(200, 3, 42)
	g, err := shape.Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("shapegen.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	dir := t.TempDir()
	path := filepath.Join(dir, "graph.csrfile")
	if _, err := WriteToFile[int64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	// Spawn two independent readers with the same file path.
	out1, stderr1, err1 := subproc.Run(t, "csrfile-mmap-readers", path)
	out2, stderr2, err2 := subproc.Run(t, "csrfile-mmap-readers", path)

	if err1 != nil {
		t.Fatalf("reader 1 failed: %v\nstderr: %s", err1, stderr1)
	}
	if err2 != nil {
		t.Fatalf("reader 2 failed: %v\nstderr: %s", err2, stderr2)
	}

	// Parse and sort lines to make comparison order-independent across
	// any implementation changes that may reorder output.
	lines1 := splitLines(string(out1))
	lines2 := splitLines(string(out2))

	sort.Strings(lines1)
	sort.Strings(lines2)

	if len(lines1) != len(lines2) {
		t.Fatalf("output line count mismatch: reader1=%d reader2=%d",
			len(lines1), len(lines2))
	}
	for i := range lines1 {
		if lines1[i] != lines2[i] {
			t.Errorf("line %d mismatch:\n  reader1: %q\n  reader2: %q",
				i, lines1[i], lines2[i])
		}
	}

	t.Logf("both readers produced %d lines (vertices)", len(lines1))
}

// splitLines returns the non-empty lines from s.
func splitLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
