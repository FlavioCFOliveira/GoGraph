package snapshot

import (
	"errors"
	"sync"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/graph/lpg"
)

// itoaSnap converts a non-negative int to its decimal string without
// importing strconv; used to generate deterministic node names in this
// file only.
func itoaSnap(i int) string {
	if i == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// buildRotationGraph returns a small string-keyed LPG graph with
// nEdges edges for use in the rotation test. Each graph variant
// uses a different generation counter in node names so successive
// snapshots carry different content.
func buildRotationGraph(gen, nEdges int) *lpg.Graph[string, int64] {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	adj := g.AdjList()
	for i := 0; i < nEdges; i++ {
		src := "n" + itoaSnap(gen*100+i)
		dst := "n" + itoaSnap(gen*100+(i+1)%nEdges)
		_ = adj.AddEdge(src, dst, int64(i))
	}
	return g
}

// TestSnapshot_RotationConcurrent runs 32 reader goroutines each
// calling [LoadSnapshotFull] in a tight loop (10 iterations) while
// the main goroutine overwrites the snapshot directory 20 times with
// [WriteSnapshotFull].
//
// Acceptance criteria:
//   - No goroutine panics.
//   - Every load either succeeds or returns a typed error — never an
//     opaque internal panic.
//   - go test -race reports no data races (the atomic tmp+rename
//     publication in [WriteSnapshotFull] must shield readers).
//
// The test does NOT assert that every read sees a complete or
// consistent snapshot: since POSIX rename is the atomic publication
// primitive, a reader that opens the directory between the RemoveAll
// and Rename may see an absent manifest and get an error. Both
// outcomes (success or typed error) are acceptable.
func TestSnapshot_RotationConcurrent(t *testing.T) {
	dir := t.TempDir()

	// Write the initial snapshot so readers have something to open.
	g0 := buildRotationGraph(0, 8)
	c0 := csr.BuildFromAdjList(g0.AdjList())
	if err := WriteSnapshotFull(dir, c0, g0); err != nil {
		t.Fatalf("initial WriteSnapshotFull: %v", err)
	}

	const (
		readers   = 32
		readIter  = 10
		rotations = 20
	)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		readErr []error
	)

	// Start reader goroutines.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readIter; j++ {
				_, err := LoadSnapshotFull(dir)
				if err == nil {
					continue
				}
				// Accept only typed errors — anything else is a bug.
				// In practice: ErrManifestUnsupported, ErrCorrupted,
				// ErrManifestCorrupted, or an os.PathError from a
				// missing manifest during the publication window.
				if errors.Is(err, ErrManifestUnsupported) ||
					errors.Is(err, ErrCorrupted) ||
					errors.Is(err, ErrManifestCorrupted) {
					// Typed error during rotation window — acceptable.
					continue
				}
				// os.PathError covers the brief window where the
				// old snapshot is removed but the new one is not yet
				// renamed into place.
				var pathErr interface{ Timeout() bool }
				if errors.As(err, &pathErr) {
					continue
				}
				// Any unrecognised error is a genuine failure.
				mu.Lock()
				readErr = append(readErr, err)
				mu.Unlock()
			}
		}()
	}

	// Writer: rotate the snapshot 20 times.
	for i := 1; i <= rotations; i++ {
		g := buildRotationGraph(i, 5+i%4)
		c := csr.BuildFromAdjList(g.AdjList())
		if err := WriteSnapshotFull(dir, c, g); err != nil {
			t.Errorf("rotation %d WriteSnapshotFull: %v", i, err)
		}
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, err := range readErr {
		t.Errorf("unexpected read error during rotation: %v", err)
	}
}
