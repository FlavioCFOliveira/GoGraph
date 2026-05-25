package csrfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gograph/internal/subproc"
)

func init() {
	subproc.Register("csrfile-fixture-sha256", func(args []string) int {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "csrfile-fixture-sha256: expected args: <output-path> <seed>")
			return 1
		}
		outPath := args[0]
		seed, err := strconv.ParseUint(args[1], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "csrfile-fixture-sha256: parse seed: %v\n", err)
			return 1
		}
		spec := FixtureSpec{Vertices: 100, Edges: 1000, Seed: seed}
		c, err := BuildFixture(spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "BuildFixture: %v\n", err)
			return 1
		}
		if _, err := WriteToFile(outPath, c); err != nil {
			fmt.Fprintf(os.Stderr, "WriteToFile: %v\n", err)
			return 1
		}
		sum, err := sha256FixtureFile(outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sha256: %v\n", err)
			return 1
		}
		fmt.Println(sum)
		return 0
	})
}

// sha256FixtureFile is a package-local helper to avoid a name conflict
// with sha256File in writer_determinism_test.go.
func sha256FixtureFile(path string) (string, error) {
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

// TestFixtureGenerator_Determinism verifies that BuildFixture followed
// by WriteToFile is deterministic both in-process and across processes.
func TestFixtureGenerator_Determinism(t *testing.T) {
	t.Parallel()

	const seed = uint64(42)
	spec := FixtureSpec{Vertices: 100, Edges: 1000, Seed: seed}

	dir := t.TempDir()
	path1 := filepath.Join(dir, "fix1.csr")
	path2 := filepath.Join(dir, "fix2.csr")

	// In-process: two independent calls must produce identical bytes.
	c1, err := BuildFixture(spec)
	if err != nil {
		t.Fatalf("BuildFixture[1]: %v", err)
	}
	if _, err := WriteToFile(path1, c1); err != nil {
		t.Fatalf("WriteToFile[1]: %v", err)
	}

	c2, err := BuildFixture(spec)
	if err != nil {
		t.Fatalf("BuildFixture[2]: %v", err)
	}
	if _, err := WriteToFile(path2, c2); err != nil {
		t.Fatalf("WriteToFile[2]: %v", err)
	}

	sum1, err := sha256FixtureFile(path1)
	if err != nil {
		t.Fatalf("sha256[1]: %v", err)
	}
	sum2, err := sha256FixtureFile(path2)
	if err != nil {
		t.Fatalf("sha256[2]: %v", err)
	}
	if sum1 != sum2 {
		t.Fatalf("in-process determinism: sha256 mismatch:\n  path1=%s\n  path2=%s", sum1, sum2)
	}

	// Cross-process: subprocess writes the same fixture and reports SHA-256.
	subPath := filepath.Join(t.TempDir(), "sub.csr")
	stdout, stderr, err := subproc.Run(t, "csrfile-fixture-sha256", subPath, strconv.FormatUint(seed, 10))
	if err != nil {
		t.Fatalf("subprocess error: %v\nstderr: %s", err, stderr)
	}
	subSum := strings.TrimSpace(string(stdout))
	if subSum != sum1 {
		t.Fatalf("cross-process determinism: sha256 mismatch:\n  parent=%s\n  child=%s", sum1, subSum)
	}
}
