package csrfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/subproc"
)

func init() {
	subproc.Register("csrfile-writer-sha256", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "csrfile-writer-sha256: missing output path arg")
			return 1
		}
		outPath := args[0]
		a := adjlist.New[int, int64](adjlist.Config{Directed: true})
		for i := 0; i < 19; i++ {
			if err := a.AddEdge(i, i+1, int64(i)); err != nil {
				fmt.Fprintf(os.Stderr, "AddEdge: %v\n", err)
				return 1
			}
		}
		c := csr.BuildFromAdjList(a)
		if _, err := WriteToFile[int64](outPath, c); err != nil {
			fmt.Fprintf(os.Stderr, "WriteToFile: %v\n", err)
			return 1
		}
		sum, err := sha256File(outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sha256: %v\n", err)
			return 1
		}
		fmt.Println(sum)
		return 0
	})
}

// sha256File returns the hex-encoded SHA-256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // test helper, path is caller-controlled
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildPathCSR builds the canonical 20-node directed path (0→1→…→19)
// with edge weights equal to the source index and returns the CSR.
func buildPathCSR() *csr.CSR[int64] {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < 19; i++ {
		if err := a.AddEdge(i, i+1, int64(i)); err != nil {
			panic(fmt.Sprintf("buildPathCSR AddEdge: %v", err))
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestWriter_Determinism verifies that WriteToFile produces a
// bit-identical file regardless of when or in which process it is
// called, given the same graph.
func TestWriter_Determinism(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path1 := filepath.Join(dir, "det1.csr")
	path2 := filepath.Join(dir, "det2.csr")

	// In-process: two independent writes must produce identical bytes.
	c1 := buildPathCSR()
	if _, err := WriteToFile[int64](path1, c1); err != nil {
		t.Fatalf("WriteToFile path1: %v", err)
	}
	c2 := buildPathCSR()
	if _, err := WriteToFile[int64](path2, c2); err != nil {
		t.Fatalf("WriteToFile path2: %v", err)
	}

	sum1, err := sha256File(path1)
	if err != nil {
		t.Fatalf("sha256 path1: %v", err)
	}
	sum2, err := sha256File(path2)
	if err != nil {
		t.Fatalf("sha256 path2: %v", err)
	}
	if sum1 != sum2 {
		t.Fatalf("in-process determinism: sha256 mismatch:\n  path1=%s\n  path2=%s", sum1, sum2)
	}

	// Cross-process: spawn a subprocess that writes the same graph and
	// reports its SHA-256; must match the in-process result.
	subPath := filepath.Join(t.TempDir(), "sub.csr")
	stdout, stderr, err := subproc.Run(t, "csrfile-writer-sha256", subPath)
	if err != nil {
		t.Fatalf("subprocess error: %v\nstderr: %s", err, stderr)
	}
	subSum := strings.TrimSpace(string(stdout))
	if subSum != sum1 {
		t.Fatalf("cross-process determinism: sha256 mismatch:\n  parent=%s\n  child=%s", sum1, subSum)
	}
}
