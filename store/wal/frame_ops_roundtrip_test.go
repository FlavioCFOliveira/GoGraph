package wal

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// TestWALOps_RoundTrip writes 20 frames through a WAL Writer (4 each of 5
// simulated op kinds), syncs once, then replays via OpenReader, asserting:
//   - byte-for-byte payload equality in the order appended
//   - Stats().Frames == 20 and Stats().Syncs == 1
//
// AllocsPerRun is called to document Append allocation behaviour (the test
// passes regardless of the value).
func TestWALOps_RoundTrip(t *testing.T) {
	// Note: t.Parallel() is intentionally absent here because
	// testing.AllocsPerRun panics when called inside a parallel test.
	opKinds := []string{
		"add-node:",
		"add-edge:",
		"remove:",
		"set-prop:",
		"del-prop:",
	}
	const total = 20 // 4 per kind × 5 kinds

	// Build expected payloads in the order they will be written.
	expected := make([][]byte, 0, total)
	for seq := 0; seq < total; seq++ {
		kind := opKinds[seq%len(opKinds)]
		p := []byte(fmt.Sprintf("%sseq-%02d", kind, seq))
		expected = append(expected, p)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, p := range expected {
		if err := w.Append(p); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	st := w.Stats()
	if st.Frames != total {
		t.Fatalf("Stats.Frames = %d, want %d", st.Frames, total)
	}
	if st.Syncs != 1 {
		t.Fatalf("Stats.Syncs = %d, want 1", st.Syncs)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Document Append allocation count. The test passes regardless of the value.
	w2, err := Open(filepath.Join(dir, "alloc.wal"))
	if err != nil {
		t.Fatalf("Open (alloc): %v", err)
	}
	probe := []byte("hello")
	allocsPerAppend := testing.AllocsPerRun(100, func() {
		_ = w2.Append(probe)
	})
	_ = w2.Close()
	t.Logf("AllocsPerRun for Append(%d-byte payload) = %.1f", len(probe), allocsPerAppend)

	// Replay and verify order + payload equality.
	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	idx := 0
	if err := r.Replay(func(f Frame) error {
		if idx >= len(expected) {
			t.Errorf("Replay yielded extra frame %d", idx)
			idx++
			return nil
		}
		want := expected[idx]
		if !bytes.Equal(f.Payload, want) {
			t.Errorf("frame %d: got %q, want %q", idx, f.Payload, want)
		}
		idx++
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if idx != total {
		t.Fatalf("Replay yielded %d frames, want %d", idx, total)
	}
}
