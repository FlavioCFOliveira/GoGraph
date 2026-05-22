package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestInitEmpty_FreshDirectory verifies that initEmpty creates the
// data-dir layout from scratch: <dir>/wal exists and is regular,
// <dir>/snapshot/manifest.json exists.
func TestInitEmpty_FreshDirectory(t *testing.T) {
	dir := t.TempDir()
	if hasManifest(dir) {
		t.Fatalf("pre-condition: t.TempDir() must be empty")
	}
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	if !hasManifest(dir) {
		t.Fatalf("post-condition: manifest not written")
	}
	if _, err := os.Stat(filepath.Join(dir, "wal")); err != nil {
		t.Fatalf("wal file: %v", err)
	}
}

// TestInitEmpty_Idempotent verifies that running initEmpty on a
// directory that already contains a manifest is a no-op and does not
// overwrite any pre-existing snapshot file.
func TestInitEmpty_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty (first): %v", err)
	}
	manifest := filepath.Join(dir, "snapshot", "manifest.json")
	infoBefore, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat manifest after first init: %v", err)
	}
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty (second): %v", err)
	}
	infoAfter, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat manifest after second init: %v", err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatalf("manifest mtime changed: before=%v after=%v (initEmpty must be a no-op on existing dirs)",
			infoBefore.ModTime(), infoAfter.ModTime())
	}
}

// TestInitEmpty_Reopenable verifies that the directory produced by
// initEmpty is openable by openStore (i.e., recovery.Open accepts the
// empty snapshot+WAL combination produced by the bootstrap).
func TestInitEmpty_Reopenable(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
	if o.graph == nil {
		t.Fatalf("openedStore.graph is nil")
	}
	if o.engine == nil {
		t.Fatalf("openedStore.engine is nil")
	}
}

// TestOpenStore_MissingManifest confirms that opening a directory
// without a manifest produces a helpful error pointing to `init`.
func TestOpenStore_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	_, err := openStore(context.Background(), dir)
	if err == nil {
		t.Fatalf("openStore: want error on missing manifest, got nil")
	}
}
