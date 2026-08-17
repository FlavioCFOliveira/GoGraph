//go:build soak || nightly

package sim

// codec_matrix_soak_test.go — the full key/weight codec matrix across crash and
// upgrade (rmp #2473). Runs under the soak layer only (docs/test-layers.md);
// the short layer keeps the cheap probes in codec_matrix_test.go.
//
// Every arm drives two crash-bearing scenarios over the real durability stack,
// so the sweep is far too expensive for the package's 60 s short budget — and
// the short layer's cost is not allowed to grow for it.

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// soakCodecMatrix is the full-scale size this layer runs at; the short layer
// runs the same sweep at smokeCodecMatrix size.
func soakCodecMatrix() codecMatrixSize { return codecMatrixSize{txns: 12, nodesPerTxn: 8} }

// TestCodecMatrix_SoakFullSweep runs every codec arm through both the
// crash-storm and the upgrade scenario.
//
// The two gates are reported SEPARATELY and mean different things. A VERDICT
// violation says the engine lost, corrupted, or resurrected data on this codec
// pair. A NON-VACUITY violation says the run did not exercise enough for those
// verdicts to have been informative — no defect, but no evidence either.
func TestCodecMatrix_SoakFullSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	res, err := runCodecMatrix(context.Background(), 0x2473_50A4, soakCodecMatrix())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Witness first, so a failure below is read against what the run measured.
	for i := range res.evidence {
		ev := &res.evidence[i]
		t.Logf("%s", ev.summary())
		if ev.boundary.crossed {
			t.Logf("  snapshot boundary: %s", ev.boundary.summary())
		}
	}

	for _, v := range res.verdict {
		t.Errorf("VERDICT %s: %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range res.vacuity {
		t.Errorf("NON-VACUITY %s: %s", v.Op, v.Message)
	}
}

// TestCodecMatrix_SoakEveryArmReachesItsMapper is the sweep's own
// assert-something-was-seen check, kept as a separate test so a failure names
// the mapper layout rather than being buried among durability verdicts.
//
// It is the one property that would make the whole matrix vacuous if it broke
// silently: if every arm somehow published the version-1 string mapper, each
// arm would still pass every durability oracle while collectively exercising
// exactly the coverage the string scenarios already had.
func TestCodecMatrix_SoakEveryArmReachesItsMapper(t *testing.T) {
	defer goleak.VerifyNone(t)

	res, err := runCodecMatrix(context.Background(), 0x2473_50A5, soakCodecMatrix())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	byArm := make(map[string]uint16, len(res.evidence))
	for i := range res.evidence {
		if res.evidence[i].mapperFormat != 0 {
			byArm[res.evidence[i].arm] = res.evidence[i].mapperFormat
		}
	}
	byteMappers := 0
	for _, arm := range codecMatrixArms() {
		got, ok := byArm[arm.name()]
		if !ok {
			t.Errorf("arm %s published no mapper.bin in either scenario", arm.name())
			continue
		}
		if got != arm.wantMapperFormat() {
			t.Errorf("arm %s published mapper.bin format %d, want %d", arm.name(), got, arm.wantMapperFormat())
		}
		if got == codecMapperFormatBytes {
			byteMappers++
		}
	}
	if byteMappers == 0 {
		t.Fatal("no arm reached the version-2 byte-mapper: the sweep exercised only the layout" +
			" the string scenarios already covered")
	}
	t.Logf("mapper layouts observed: %v (%d arms on the version-2 byte-mapper)", byArm, byteMappers)
}
