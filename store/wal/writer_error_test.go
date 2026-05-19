package wal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriter_Truncate_FreesAllBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	for i := 0; i < 8; i++ {
		if err := w.Append([]byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	preInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	freed, err := w.Truncate()
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if freed != preInfo.Size() {
		t.Fatalf("Truncate freed = %d, want %d", freed, preInfo.Size())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("post-Truncate file size = %d, want 0", info.Size())
	}
	// Verify subsequent Append+Sync writes from offset 0 cleanly.
	if err := w.Append([]byte("post")); err != nil {
		t.Fatalf("post-Truncate Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("post-Truncate Sync: %v", err)
	}
	postFrame, err := Decode(bytes.NewReader(mustRead(t, path)))
	if err != nil {
		t.Fatalf("Decode of post-Truncate frame: %v", err)
	}
	if string(postFrame.Payload) != "post" {
		t.Fatalf("post-Truncate frame payload = %q, want %q", postFrame.Payload, "post")
	}
}

func TestWriter_Truncate_AfterCloseReturnsErrWriterClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Truncate(); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Truncate after Close = %v, want ErrWriterClosed", err)
	}
}

func TestWriter_AppendCtx_ReturnsCtxErrWhenPreCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.AppendCtx(ctx, []byte("never written")); !errors.Is(err, context.Canceled) {
		t.Fatalf("AppendCtx cancelled = %v, want context.Canceled", err)
	}
	if got := w.Stats().Frames; got != 0 {
		t.Fatalf("Frames = %d after cancelled AppendCtx, want 0", got)
	}
}

func TestWriter_SyncCtx_ReturnsCtxErrWhenPreCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "x.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.SyncCtx(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncCtx cancelled = %v, want context.Canceled", err)
	}
	if got := w.Stats().Syncs; got != 0 {
		t.Fatalf("Syncs = %d after cancelled SyncCtx, want 0", got)
	}
}

func TestOpen_NonExistentParentDirectoryReturnsError(t *testing.T) {
	t.Parallel()
	bogus := filepath.Join(t.TempDir(), "no", "such", "parent", "wal")
	if _, err := Open(bogus); err == nil {
		t.Fatalf("Open with missing parent dir should error")
	}
}

func TestReader_OpenNonExistentPathReturnsError(t *testing.T) {
	t.Parallel()
	bogus := filepath.Join(t.TempDir(), "missing.wal")
	if _, err := OpenReader(bogus); err == nil {
		t.Fatalf("OpenReader on missing path should error")
	}
}

func TestReader_TailOffset_AtCleanEOF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 3)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for range r.Frames() { //nolint:revive // we only need to drive the iterator
	}
	if got := r.TailOffset(); got != info.Size() {
		t.Fatalf("clean-EOF TailOffset = %d, want %d", got, info.Size())
	}
	if r.TailError() != nil {
		t.Fatalf("clean-EOF TailError = %v, want nil", r.TailError())
	}
}

func TestReader_TailOffset_AtTornFrame(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 3)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Cut one byte off the file: the last frame becomes torn but the
	// first two should remain readable. TailOffset should mark the
	// start of the torn frame.
	if err := os.Truncate(path, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for range r.Frames() { //nolint:revive
	}
	if got := r.TailOffset(); got >= info.Size() {
		t.Fatalf("torn TailOffset = %d, want < %d", got, info.Size())
	}
	if r.TailError() == nil {
		t.Fatalf("torn TailError = nil, want a non-nil error")
	}
}

func TestReader_CloseWithoutOwnedCloserIsNoop(t *testing.T) {
	t.Parallel()
	r := NewReader(bytes.NewReader([]byte{}), nil)
	if err := r.Close(); err != nil {
		t.Fatalf("Close with nil closer = %v, want nil", err)
	}
}

func TestReader_ReplayPropagatesApplyError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	writeNFrames(t, path, 3)
	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	sentinel := errors.New("stop on second frame")
	count := 0
	err = r.Replay(func(_ Frame) error {
		count++
		if count == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Replay = %v, want sentinel", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // t.TempDir-rooted
	if err != nil {
		t.Fatal(err)
	}
	return b
}
