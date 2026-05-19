package ldbc

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRun_SF1Synthetic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	spec := Spec{
		Scale:       ScaleSF1,
		Queries:     16,
		Synthetic:   true,
		OutDir:      dir,
		BulkOutFile: filepath.Join(dir, "snb_sf1.csr"),
	}
	rep, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.IngestTime <= 0 {
		t.Fatalf("IngestTime not recorded")
	}
	if len(SortStats(rep)) == 0 {
		t.Fatalf("no query stats recorded")
	}
}

func TestRun_RealisticNotImplemented(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Spec{Synthetic: false})
	if err == nil {
		t.Fatalf("expected error for non-synthetic mode")
	}
}
