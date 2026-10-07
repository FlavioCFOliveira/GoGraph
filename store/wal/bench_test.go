package wal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// discardFile is a no-op WALFile: it satisfies the (unexported) WALFile
// interface that OpenWith accepts but discards every byte and treats Sync /
// Truncate as cheap no-ops. It lets BenchmarkAppend isolate the cost of frame
// encoding plus buffered append from the cost of a real fsync(2), so the
// upcoming group-commit work (#1507) and per-frame alloc fix (#1509) can be
// measured against a fsync-free baseline.
//
// It deliberately tracks size so Seek(0, io.SeekEnd) in OpenWith reports a
// sane baseline; production code never reaches this type.
type discardFile struct {
	size int64
}

func (d *discardFile) Write(p []byte) (int, error) {
	d.size += int64(len(p))
	return len(p), nil
}

func (d *discardFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekEnd:
		return d.size + offset, nil
	case io.SeekStart:
		return offset, nil
	default: // io.SeekCurrent
		return d.size + offset, nil
	}
}

func (d *discardFile) Sync() error               { return nil }
func (d *discardFile) Truncate(size int64) error { d.size = size; return nil }
func (d *discardFile) Close() error              { return nil }

// Read satisfies the WALFile io.Reader requirement. The benchmark never reads
// back, so it reports EOF.
func (d *discardFile) Read(_ []byte) (int, error) { return 0, io.EOF }

// BenchmarkAppend measures the WAL append path at a range of representative
// frame sizes, in two regimes that bracket the durability cost:
//
//   - encode-only: append over a no-op discardFile, isolating frame encoding
//     and the buffered write from any disk I/O.
//   - fsync: append a frame to a real on-disk WAL and Sync it, exposing the
//     per-commit fsync(2) cost that group commit will later amortise.
//
// The fsync regime issues one Sync per iteration on purpose: that is the
// worst case (one durable commit per op) the group-commit gate must improve.
func BenchmarkAppend(b *testing.B) {
	for _, size := range []int{64, 256, 4096} {
		payload := make([]byte, size)

		b.Run(fmt.Sprintf("encode-only/size=%d", size), func(b *testing.B) {
			w, err := OpenWith(&discardFile{})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			b.SetBytes(int64(HeaderSizeV2 + size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := w.Append(payload); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("fsync/size=%d", size), func(b *testing.B) {
			w, err := Open(filepath.Join(b.TempDir(), "bench.wal"))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			b.SetBytes(int64(HeaderSizeV2 + size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := w.Append(payload); err != nil {
					b.Fatal(err)
				}
				if err := w.Sync(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPhase3Truncate measures checkpoint phase 3 over a log whose 56.5 MB
// suffix was committed after the checkpoint's redo position (rmp #2195). With
// segments, phase 3 unlinks the whole segments below the oldest retained
// position the control file records ([Writer.ReclaimSegments]); it holds no
// commit lock, so no committer waits on it. The redo position and the control
// write are set up outside the timer. The benchmark of the same name on the
// pre-segment build timed [Writer.TruncatePrefix], which copied the suffix
// under the commit lock.
func BenchmarkPhase3Truncate(b *testing.B) {
	const prefix, suffix, frame = 32 << 20, 56_500_000, 64 << 10
	payload := make([]byte, frame)
	b.Run("suffix=56.5MB", func(b *testing.B) {
		root := b.TempDir()
		for b.Loop() {
			_ = os.MkdirAll(filepath.Join(root, "it"), 0o700)
			b.StopTimer()
			iterDir := filepath.Join(root, "it")
			w, err := Open(filepath.Join(iterDir, "wal"))
			if err != nil {
				b.Fatal(err)
			}
			for n := 0; n < prefix; n += frame {
				if err := w.Append(payload); err != nil {
					b.Fatal(err)
				}
			}
			if err := w.Sync(); err != nil {
				b.Fatal(err)
			}
			redo := w.DurableOffset()
			for n := 0; n < suffix; n += frame {
				if err := w.Append(payload); err != nil {
					b.Fatal(err)
				}
			}
			if err := w.Sync(); err != nil {
				b.Fatal(err)
			}
			if err := w.MarkCheckpoint(redo); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := w.ReclaimSegments(); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			_ = w.Close()
			_ = os.RemoveAll(filepath.Join(root, "it")) // keep one log on the volume at a time
			b.StartTimer()
		}
	})
}
