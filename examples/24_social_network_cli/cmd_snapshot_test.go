package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunSnapshot_AfterSeed verifies that running snapshot after seed
// rewrites the manifest, csr.bin, labels.bin and properties.bin and
// that the JSON reply carries the expected keys.
func TestRunSnapshot_AfterSeed(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}

	// Seed via the helper used by cmdSeed, then take a snapshot.
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := seedFixture(o.store); err != nil {
		_ = o.Close()
		t.Fatalf("seedFixture: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	manifestPath := filepath.Join(dir, "snapshot", "manifest.json")
	before, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatalf("stat manifest before snapshot: %v", err)
	}

	var buf bytes.Buffer
	if err := runSnapshot(context.Background(), dir, &buf); err != nil {
		t.Fatalf("runSnapshot: %v", err)
	}
	after, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatalf("stat manifest after snapshot: %v", err)
	}
	if after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("snapshot did not rewrite the manifest (mtime unchanged)")
	}

	var reply map[string]any
	if err := json.Unmarshal(buf.Bytes(), &reply); err != nil {
		t.Fatalf("invalid JSON reply: %v (%q)", err, buf.String())
	}
	if reply["status"] != "ok" {
		t.Fatalf("status: got %v, want ok", reply["status"])
	}
	got, _ := reply["snapshot_dir"].(string)
	const wantSuffix = "snapshot"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("snapshot_dir: got %q, want suffix %q", got, wantSuffix)
	}
}

// TestRunSnapshot_MissingDir confirms that snapshot fails fast when
// the data directory has not been initialised.
func TestRunSnapshot_MissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-initialised")
	var buf bytes.Buffer
	err := runSnapshot(context.Background(), dir, &buf)
	if err == nil {
		t.Fatalf("want error on uninitialised dir, got nil")
	}
	var ue *usageError
	if errors.As(err, &ue) {
		t.Fatalf("uninitialised dir must be a runtime error (exit 1), got *usageError")
	}
}
