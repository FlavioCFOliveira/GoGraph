package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

func init() {
	// Register the child handler before subproc.Dispatch() is called from
	// TestMain. The handler builds a deterministic 50-node path graph, writes
	// a snapshot to a temp directory, then emits "filename:sha256hex\n" for
	// every non-manifest file in that directory to stdout.
	subproc.Register("snapshot-write-sha256", func(args []string) int {
		dir, err := os.MkdirTemp("", "snap-child-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-write-sha256: MkdirTemp: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()

		snapDir := filepath.Join(dir, "snap")
		c := buildDeterministicCSR()
		if err := WriteSnapshotCSR(snapDir, c); err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-write-sha256: WriteSnapshotCSR: %v\n", err)
			return 1
		}

		hashes, err := hashSegmentFiles(snapDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot-write-sha256: hashSegmentFiles: %v\n", err)
			return 1
		}
		// Emit in deterministic order (alphabetical filename).
		names := make([]string, 0, len(hashes))
		for name := range hashes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Printf("%s:%s\n", name, hashes[name])
		}
		return 0
	})
}

// buildDeterministicCSR builds the canonical 50-node directed path
// 0→1→…→49 with edge weights equal to the source index.
// Being fully deterministic (no RNG, integer keys with FNV-1a hashing),
// it produces the same CSR — and therefore the same snapshot bytes —
// in every process.
func buildDeterministicCSR() *csr.CSR[int64] {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < 49; i++ {
		if err := a.AddEdge(i, i+1, int64(i)); err != nil {
			panic(fmt.Sprintf("buildDeterministicCSR AddEdge: %v", err))
		}
	}
	return csr.BuildFromAdjList(a)
}

// hashSegmentFiles returns a map of filename→sha256hex for every
// non-manifest file in the snapshot directory dir.
func hashSegmentFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ReadDir %s: %w", dir, err)
	}
	result := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Skip the manifest — it embeds a timestamp field (CreatedAt) that
		// is not stable across separate process executions.
		if e.Name() == "manifest.json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		h, err := sha256HexFile(path)
		if err != nil {
			return nil, err
		}
		result[e.Name()] = h
	}
	return result, nil
}

// sha256HexFile returns the lowercase hex-encoded SHA-256 of the file at path.
func sha256HexFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // test helper, path is caller-controlled
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseHashLines parses the "filename:sha256hex\n" output emitted by the
// "snapshot-write-sha256" handler into a map.
func parseHashLines(output []byte) (map[string]string, error) {
	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			return nil, fmt.Errorf("malformed line: %q", line)
		}
		result[line[:idx]] = line[idx+1:]
	}
	return result, nil
}

// TestSnapshot_CrossProcess_ByteEqual verifies that snapshot segment files
// written by two independent child processes from the same deterministic
// graph are byte-identical.
//
// The manifest is excluded from comparison because it embeds a
// time.Now()-derived CreatedAt timestamp. Only segment files (e.g.
// csr.bin) are hashed. Agreement there proves no process-local
// entropy (RNG seeds, pointer addresses, map iteration order) leaks
// into the binary serialisation.
func TestSnapshot_CrossProcess_ByteEqual(t *testing.T) {
	t.Parallel()

	run := func(label string) map[string]string {
		stdout, stderr, err := subproc.Run(t, "snapshot-write-sha256")
		if err != nil {
			t.Fatalf("%s: subproc.Run: %v\nstderr: %s", label, err, stderr)
		}
		hashes, err := parseHashLines(stdout)
		if err != nil {
			t.Fatalf("%s: parse output: %v\nstdout: %s", label, err, stdout)
		}
		if len(hashes) == 0 {
			t.Fatalf("%s: no segment files reported", label)
		}
		return hashes
	}

	child1 := run("child1")
	child2 := run("child2")

	if len(child1) != len(child2) {
		t.Fatalf("segment file count mismatch: child1=%d child2=%d",
			len(child1), len(child2))
	}

	mismatches := 0
	for name, h1 := range child1 {
		h2, ok := child2[name]
		if !ok {
			t.Errorf("child2 missing segment file %q", name)
			mismatches++
			continue
		}
		if h1 != h2 {
			t.Errorf("segment %q: child1=%s child2=%s", name, h1, h2)
			mismatches++
		}
	}
	if mismatches == 0 {
		t.Logf("all %d segment files are byte-equal across two independent child processes",
			len(child1))
	}
}
