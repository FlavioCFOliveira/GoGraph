package sim

// codec_matrix_test.go — the SHORT-layer half of the codec matrix (rmp #2473).
//
// The short layer's budget for this package is already largely spent (~32 s of
// a 60 s ceiling), and the matrix drives seven codec arms through two
// crash-bearing scenarios each. So the full sweep lives in the soak layer
// (codec_matrix_soak_test.go) and the short layer keeps only what is cheap and
// what would otherwise let the soak sweep rot unnoticed:
//
//   - the ErrNoWeightCodec negative probe, which involves one small transaction
//     and one reopen;
//   - the whole matrix at the SMALLEST size, so a refactor that silently routed
//     every arm back onto the string codec, or stopped reaching the snapshot,
//     fails in the layer that runs on every change instead of leaving the soak
//     sweep green and uninformative;
//   - pure assertions on the arm table and on the gates themselves.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// TestCodecMatrix_NoWeightCodecContract drives txn.ErrNoWeightCodec
// deliberately and pins what the engine ACTUALLY does on a store built without
// a weight codec — including the asymmetry between AddEdge (which accepts a
// zero weight) and AddEdgeWithHandle (which does not).
func TestCodecMatrix_NoWeightCodecContract(t *testing.T) {
	defer goleak.VerifyNone(t)

	p, err := probeNoWeightCodec(context.Background(), 0x2473_0001)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if v := checkNoWeightCodecContract(p); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("no-weight-codec contract: %s: %s", viol.Op, viol.Message)
		}
	}
	// Witness — what the engine actually did, logged rather than asserted twice.
	t.Logf("ErrNoWeightCodec probe: AddEdge(non-zero)=%v, AddEdge(zero)=%v, "+
		"AddEdgeWithHandle(zero)=%v, committed=%t, zero-weight edge recovered=%t",
		p.nonZeroErr, p.zeroErr, p.withHandleErr, p.committed, p.zeroEdgeRecovered)

	// The sentinel must be the documented one, not merely a non-nil error.
	if !errors.Is(p.nonZeroErr, txn.ErrNoWeightCodec) {
		t.Errorf("AddEdge(non-zero) error = %v, want to wrap txn.ErrNoWeightCodec", p.nonZeroErr)
	}
}

// TestCodecMatrix_SmokeSweep runs the WHOLE matrix — every arm, both scenarios
// — at the smallest size. The soak layer runs the same sweep at full scale; the
// point of running it here too is that a refactor which silently routed every
// arm back onto the string codec, or stopped reaching the snapshot at all,
// would leave the soak sweep passing while proving nothing, and this catches
// that on every change for a fraction of a second.
//
// The two gates are reported SEPARATELY: a verdict violation is an engine
// defect, a non-vacuity violation is a run that exercised too little.
func TestCodecMatrix_SmokeSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	res, err := runCodecMatrix(context.Background(), 0x2473_0002, smokeCodecMatrix())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for i := range res.evidence {
		t.Logf("%s", res.evidence[i].summary())
	}
	for _, v := range res.verdict {
		t.Errorf("VERDICT %s: %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range res.vacuity {
		t.Errorf("NON-VACUITY %s: %s", v.Op, v.Message)
	}

	// The sweep's own assert-something-was-seen check, kept out of the gates
	// above so a failure here names the layout rather than a durability verdict:
	// at least one arm must have reached the version-2 byte-mapper, which is the
	// path no scenario in this package reached before rmp #2473.
	byteMappers := 0
	for i := range res.evidence {
		if res.evidence[i].mapperFormat == codecMapperFormatBytes {
			byteMappers++
		}
	}
	if byteMappers == 0 {
		t.Fatal("no arm reached the version-2 byte-mapper: the sweep exercised only the mapper" +
			" layout the string-keyed scenarios already covered")
	}
	t.Logf("arms reaching the version-2 byte-mapper: %d of %d evidence rows",
		byteMappers, len(res.evidence))
}

// TestCodecMatrix_ArmsAreDistinct guards the arm table itself. Every arm must
// carry a unique label, and the expected mapper layout must be version 1 for
// exactly the string arm and version 2 for every other — the property that
// makes the format verdict discriminating rather than universally true.
func TestCodecMatrix_ArmsAreDistinct(t *testing.T) {
	arms := codecMatrixArms()
	if len(arms) < 4 {
		t.Fatalf("codecMatrixArms() has %d arms, want at least 4 (uuid, int64, binary-marshaler, string)", len(arms))
	}
	seen := make(map[string]bool, len(arms))
	stringArms, byteArms := 0, 0
	for _, arm := range arms {
		if seen[arm.name()] {
			t.Errorf("duplicate codec arm label %q", arm.name())
		}
		seen[arm.name()] = true
		switch arm.wantMapperFormat() {
		case codecMapperFormatString:
			stringArms++
		case codecMapperFormatBytes:
			byteArms++
		default:
			t.Errorf("arm %s expects mapper format %d, want 1 or 2", arm.name(), arm.wantMapperFormat())
		}
	}
	if stringArms != 1 {
		t.Errorf("%d arms expect the version-1 string mapper, want exactly 1 (the control)", stringArms)
	}
	if byteArms == 0 {
		t.Error("no arm expects the version-2 byte-mapper: the matrix would not reach it at all")
	}
	// The pairs the task names explicitly must all be present.
	for _, want := range []string{"uuid/float64", "int64/int64", "binarymarshaler/binarymarshaler"} {
		if !seen[want] {
			t.Errorf("codec arm %q is missing from the matrix", want)
		}
	}
	t.Logf("codec matrix arms: %d (%d string-mapper, %d byte-mapper)", len(arms), stringArms, byteArms)
}

// TestCodecMatrix_NonVacuityGateDiscriminates proves the shape gate can fail.
// A gate that cannot fire proves nothing about the runs it passes, so it is
// driven here against evidence that is degenerate in each way it claims to
// catch — and against healthy evidence, which it must accept.
func TestCodecMatrix_NonVacuityGateDiscriminates(t *testing.T) {
	arms := []codecArm{findCodecArm(t, "int64/int64")}
	healthy := codecArmEvidence{
		arm: "int64/int64", scenario: codecScenarioUpgrade, ran: true,
		ackedNodes: 16, recoveredNodes: 16, ackedEdges: 12,
		recoveredWeights: 12, mapperFormat: codecMapperFormatBytes,
	}
	if v := checkCodecMatrixNonVacuity(arms, []codecArmEvidence{healthy}); len(v) > 0 {
		t.Fatalf("the gate rejected healthy evidence: %v", v)
	}

	cases := []struct {
		name   string
		mutate func(e *codecArmEvidence)
	}{
		{"arm never ran", func(e *codecArmEvidence) { e.ran = false }},
		{"no acked nodes", func(e *codecArmEvidence) { e.ackedNodes = 0 }},
		{"empty recovered graph", func(e *codecArmEvidence) { e.recoveredNodes = 0 }},
		{"no edges, so no weight codec", func(e *codecArmEvidence) { e.ackedEdges = 0 }},
		{"no mapper published", func(e *codecArmEvidence) { e.mapperFormat = 0 }},
		{"no weight ever confirmed", func(e *codecArmEvidence) { e.recoveredWeights = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			degenerate := healthy
			tc.mutate(&degenerate)
			if v := checkCodecMatrixNonVacuity(arms, []codecArmEvidence{degenerate}); len(v) == 0 {
				t.Fatalf("the non-vacuity gate accepted degenerate evidence (%s)", tc.name)
			}
		})
	}
}

// TestCodecMatrix_PinnedWeightGapMustBeObserved guards the pin on the arm whose
// weight type the snapshot CSR writer cannot persist. The pin is only worth
// having while it is exercised: an arm documented as losing its weights that
// never observes a dropped weight is asserting nothing, and the gate must say
// so rather than pass.
func TestCodecMatrix_PinnedWeightGapMustBeObserved(t *testing.T) {
	arm := findCodecArm(t, "binarymarshaler/binarymarshaler")
	if arm.weightSurvivesSnapshot() {
		t.Fatal("the binary-marshaler weight arm is no longer marked as unpersistable by the" +
			" snapshot CSR writer; if the engine now persists it, restore the full round-trip" +
			" assertion and delete this test")
	}
	arms := []codecArm{arm}
	base := codecArmEvidence{
		arm: arm.name(), scenario: codecScenarioUpgrade, ran: true,
		ackedNodes: 16, recoveredNodes: 16, ackedEdges: 12,
		mapperFormat: codecMapperFormatBytes,
	}

	observed := base
	observed.weightGaps = 12
	if v := checkCodecMatrixNonVacuity(arms, []codecArmEvidence{observed}); len(v) > 0 {
		t.Fatalf("the gate rejected evidence in which the pinned gap WAS observed: %v", v)
	}

	unobserved := base
	unobserved.weightGaps = 0
	unobserved.recoveredWeights = 12
	if v := checkCodecMatrixNonVacuity(arms, []codecArmEvidence{unobserved}); len(v) == 0 {
		t.Fatal("the gate accepted a run in which the pinned weight gap was never observed:" +
			" the pin would be vacuous")
	}
}

// TestCodecMatrix_MapperFormatGateDiscriminates proves the format verdict can
// fail: an arm whose durable mapper carries the OTHER layout must be reported,
// and an arm that published no mapper must not be (that is the non-vacuity
// gate's question, not this one's).
func TestCodecMatrix_MapperFormatGateDiscriminates(t *testing.T) {
	arm := findCodecArm(t, "int64/int64")

	wrong := codecArmEvidence{arm: arm.name(), mapperFormat: codecMapperFormatString}
	if v := checkCodecMapperFormat(0, arm, &wrong); len(v) == 0 {
		t.Fatal("the format verdict accepted a version-1 mapper for a non-string key arm")
	}
	right := codecArmEvidence{arm: arm.name(), mapperFormat: codecMapperFormatBytes}
	if v := checkCodecMapperFormat(0, arm, &right); len(v) > 0 {
		t.Fatalf("the format verdict rejected the correct layout: %v", v)
	}
	absent := codecArmEvidence{arm: arm.name(), mapperFormat: 0}
	if v := checkCodecMapperFormat(0, arm, &absent); len(v) > 0 {
		t.Fatalf("the format verdict fired on an absent mapper (that is the vacuity gate's job): %v", v)
	}
}

// findCodecArm returns the named arm from the matrix, failing the test when the
// table no longer carries it.
func findCodecArm(t *testing.T, name string) codecArm {
	t.Helper()
	for _, arm := range codecMatrixArms() {
		if arm.name() == name {
			return arm
		}
	}
	t.Fatalf("codec arm %q not found in codecMatrixArms()", name)
	return nil
}
