package wal

import (
	"bufio"
	"fmt"
	"io"
)

// walFile is the minimal file-system interface that [Writer] requires
// of its underlying file handle. *os.File and *testfs.FaultFile both
// satisfy this interface, which allows fault-injection tests to
// substitute a synthetic file without touching production paths.
//
// The interface is intentionally unexported: production callers
// always open WAL files via [Open] (which wraps *os.File); only
// tests reach for [OpenWith].
type walFile interface {
	io.Writer
	io.Seeker
	// Sync flushes OS write buffers to durable storage.
	Sync() error
	// Truncate resizes the file to size bytes.
	Truncate(size int64) error
	// Close releases underlying OS resources.
	Close() error
}

// OpenWith builds a [Writer] over an already-open file handle. The
// caller transfers ownership: [Writer.Close] will call f.Close().
//
// This constructor exists primarily for tests that inject a
// *testfs.FaultFile; production code should use [Open].
func OpenWith(f walFile) (*Writer, error) {
	if f == nil {
		return nil, fmt.Errorf("wal: OpenWith: nil file")
	}
	// Seek to the end so Append is truly append-only even when the
	// caller opened the file without O_APPEND. The resulting position
	// doubles as the durable baseline: bytes already in the file at
	// open time are presumed durable, so a later sync failure rolls
	// the file back exactly here.
	pos, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("wal: OpenWith: seek to end: %w", err)
	}
	return &Writer{
		f:            f,
		bw:           bufio.NewWriterSize(f, 64*1024),
		durableSize:  pos,
		appendedSize: pos,
	}, nil
}
