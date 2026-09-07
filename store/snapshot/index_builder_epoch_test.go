package snapshot

// index_builder_epoch_test.go — rmp #2797: the manifest field that tells a
// reader WHICH index builder produced the indexes/<name>.bin payloads.
//
// The behavioural consequence — a pre-epoch payload rebuilt instead of
// hydrated, and a current-epoch one still hydrated — is gated in package cypher
// (index_builder_epoch_test.go), which owns the index bindings, and the
// whole-image refusal itself is gated in store/recovery. What belongs HERE is
// only the format contract those two rest on: that the writer stamps the epoch,
// that an absent field decodes to 0, and that 0 leaves no key in the file.
//
// Layer: short.

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestManifest_IndexBuilderEpochRoundTrip pins the three states the field can be
// in on disk, because the whole remediation rests on the middle one: ABSENT must
// come back as 0 and 0 must be indistinguishable from absent, or a manifest
// written before the field existed would decode to something a reader could
// mistake for a real epoch.
func TestManifest_IndexBuilderEpochRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("current epoch survives the round trip", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := WriteManifest(&buf, Manifest{Version: ManifestVersion, IndexBuilderEpoch: CurrentIndexBuilderEpoch}); err != nil {
			t.Fatalf("WriteManifest: %v", err)
		}
		if !strings.Contains(buf.String(), `"index_builder_epoch": 1`) {
			t.Fatalf("the encoded manifest does not carry the epoch:\n%s", buf.String())
		}
		got, err := LoadManifest(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		if got.IndexBuilderEpoch != CurrentIndexBuilderEpoch {
			t.Fatalf("IndexBuilderEpoch = %d, want %d", got.IndexBuilderEpoch, CurrentIndexBuilderEpoch)
		}
	})

	t.Run("zero is omitted, exactly like a manifest that predates the field", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := WriteManifest(&buf, Manifest{Version: ManifestVersion}); err != nil {
			t.Fatalf("WriteManifest: %v", err)
		}
		if strings.Contains(buf.String(), "index_builder_epoch") {
			t.Fatalf("a zero epoch left a key in the file, so an old manifest is no longer "+
				"byte-identical to what previous builds wrote:\n%s", buf.String())
		}
		got, err := LoadManifest(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("LoadManifest: %v", err)
		}
		if got.IndexBuilderEpoch != 0 {
			t.Fatalf("IndexBuilderEpoch = %d for an omitted field, want 0", got.IndexBuilderEpoch)
		}
	})

	t.Run("a manifest written without the key decodes to zero", func(t *testing.T) {
		t.Parallel()
		// Hand-built rather than produced by WriteManifest: this is the shape a
		// PREVIOUS build wrote, and the point is that this build reads it as
		// "no epoch" rather than rejecting it.
		payload, err := json.Marshal(map[string]any{
			"version":           ManifestVersion,
			"indexes_commit_ts": 42,
		})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got, lerr := LoadManifest(bytes.NewReader(payload))
		if lerr != nil {
			t.Fatalf("LoadManifest over a pre-field manifest = %v, want nil", lerr)
		}
		if got.IndexBuilderEpoch != 0 {
			t.Fatalf("IndexBuilderEpoch = %d, want 0", got.IndexBuilderEpoch)
		}
		if got.IndexesCommitTS != 42 {
			t.Fatalf("IndexesCommitTS = %d, want 42: the rest of the manifest must still decode",
				got.IndexesCommitTS)
		}
		if got.IntegrityVerified {
			t.Fatal("a trailer-less manifest reported IntegrityVerified")
		}
	})
}

// TestWriteSnapshotFull_StampsIndexBuilderEpoch is the writer half: every full
// snapshot this build publishes names its own index builder, unconditionally —
// including when it publishes no index payload and no watermark at all.
//
// The unconditional part is the load-bearing one. A snapshot that gains its
// first index later must not inherit an absence that would then look like "built
// by an unknown builder" for ever, which is the same argument
// [Manifest.IndexesCommitTS] already makes for the watermark.
func TestWriteSnapshotFull_StampsIndexBuilderEpoch(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "snapshot")
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("a", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, cs, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	m, err := ReadManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadManifestFile: %v", err)
	}
	if m.IndexBuilderEpoch != CurrentIndexBuilderEpoch {
		t.Fatalf("IndexBuilderEpoch = %d, want %d", m.IndexBuilderEpoch, CurrentIndexBuilderEpoch)
	}
	// The present-time writer publishes no watermark, so this snapshot is
	// unhydratable anyway — and that is precisely why the epoch must be stamped
	// independently of it rather than alongside it.
	if m.IndexesCommitTS != 0 {
		t.Fatalf("IndexesCommitTS = %d, want 0 from a present-time writer", m.IndexesCommitTS)
	}
	if len(m.Indexes) != 0 {
		t.Fatalf("Indexes = %+v, want none: this graph has no index manager", m.Indexes)
	}
}
