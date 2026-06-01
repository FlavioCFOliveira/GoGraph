package wal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriter_OpenAppendSync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Append([]byte("first")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Append([]byte("second")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := w.Stats()
	if st.Frames != 2 || st.Syncs != 1 {
		t.Fatalf("Stats = %+v, want Frames=2 Syncs=1", st)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWriter_AppendReadBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	payloads := [][]byte{
		[]byte("alpha"),
		[]byte("beta"),
		[]byte("gamma"),
	}
	for _, p := range payloads {
		if err := w.Append(p); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is from t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	rdr := bytes.NewReader(data)
	for i, want := range payloads {
		got, err := Decode(rdr)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got.Payload, want) {
			t.Fatalf("frame %d payload mismatch", i)
		}
	}
	if _, err := Decode(rdr); !errors.Is(err, ErrTornFrame) {
		t.Fatalf("tail: expected ErrTornFrame, got %v", err)
	}
}

func TestWriter_AfterCloseIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Append([]byte("x")); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Append after Close: %v", err)
	}
	if err := w.Sync(); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Sync after Close: %v", err)
	}
	if err := w.Close(); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Close after Close: %v", err)
	}
}

func TestWriter_CreatesFileWith0600(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Write and durably commit a frame, then read it back, to confirm the
	// tightened create mode does not disturb append/sync/recovery.
	if err := w.Append([]byte("secret mutation")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("WAL file mode = %#o, want 0o600", got)
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is from t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(out.Payload, []byte("secret mutation")) {
		t.Fatalf("Payload mismatch: %q", out.Payload)
	}
}

func TestWriter_Concurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "c.wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	const goroutines = 32
	const per = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = w.Append([]byte(fmt.Sprintf("%d-%d", g, i)))
			}
			_ = w.Sync()
		}(g)
	}
	wg.Wait()
	st := w.Stats()
	if st.Frames != goroutines*per {
		t.Fatalf("Frames = %d, want %d", st.Frames, goroutines*per)
	}
}

func TestWriter_GroupCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "g.wal"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = w.Close() }()
	for i := 0; i < 100; i++ {
		if err := w.Append([]byte("payload")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := w.Stats()
	if st.Frames != 100 || st.Syncs != 1 {
		t.Fatalf("group commit Stats = %+v", st)
	}
}

func BenchmarkWriter_AppendSync_Batch(b *testing.B) {
	for _, batchSize := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("batch=%d", batchSize), func(b *testing.B) {
			dir := b.TempDir()
			w, err := Open(filepath.Join(dir, "bench.wal"))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			payload := make([]byte, 256)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for k := 0; k < batchSize; k++ {
					_ = w.Append(payload)
				}
				_ = w.Sync()
			}
		})
	}
}
